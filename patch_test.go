package revgrep

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestGitPatch_nonGitDir(t *testing.T) {
	t.Chdir(t.TempDir())

	patch, newFiles, err := GitPatch(context.Background(), patchOption{})
	if err != nil {
		t.Errorf("error expected nil, got: %v", err)
	}

	if patch != nil {
		t.Errorf("patch expected nil, got: %v", patch)
	}

	if newFiles != nil {
		t.Errorf("newFiles expected nil, got: %v", newFiles)
	}
}

// A file moved into the working directory from outside of it must be detected as a rename:
// only its edited lines are new.
func TestChecker_Prepare_renameIntoSubdir(t *testing.T) {
	for _, subdir := range []string{"sub", "my dir", "süb"} {
		t.Run(subdir, func(t *testing.T) {
			repo := newRenameRepo(t, subdir)

			t.Chdir(filepath.Join(repo, subdir))

			checker := Checker{RevisionFrom: "HEAD~1"}

			err := checker.Prepare(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			assertIsNew(t, &checker, map[string]bool{
				"moved.go:4":    true,
				"moved.go:1":    false,
				"moved.go:8":    false,
				"existing.go:3": true,
				"existing.go:1": false,
			})

			for _, file := range []string{"other.go", "outside/other.go", "../outside/other.go"} {
				if _, ok := checker.changes[file]; ok {
					t.Errorf("file %q outside of the working directory must not be tracked", file)
				}
			}
		})
	}
}

// A user's diff.relative setting must not hide the rename source either.
func TestChecker_Prepare_diffRelativeConfig(t *testing.T) {
	repo := newRenameRepo(t, "sub")

	runGit(t, repo, "config", "--local", "diff.relative", "true")

	t.Chdir(filepath.Join(repo, "sub"))

	checker := Checker{RevisionFrom: "HEAD~1"}

	err := checker.Prepare(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertIsNew(t, &checker, map[string]bool{
		"moved.go:4": true,
		"moved.go:1": false,
	})
}

// From the repository root nothing is restricted: paths stay repository-relative.
func TestChecker_Prepare_renameFromRoot(t *testing.T) {
	repo := newRenameRepo(t, "sub")

	t.Chdir(repo)

	checker := Checker{RevisionFrom: "HEAD~1"}

	err := checker.Prepare(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertIsNew(t, &checker, map[string]bool{
		"sub/moved.go:4":        true,
		"sub/moved.go:1":        false,
		"sub/existing.go:3":     true,
		"outside/other.go:2":    true,
		"outside/other.go:1":    false,
		"outside/moved.go:1":    false,
		"sub/not-in-patch.go:1": false,
	})
}

// Without revisions, the unstaged changes and untracked files of the working directory are reported.
func TestChecker_Prepare_unstagedAndUntrackedInSubdir(t *testing.T) {
	repo := newRenameRepo(t, "sub")

	writeLines(t, repo, "sub/existing.go", "a", "b", "C", "d", "E")
	writeLines(t, repo, "outside/other.go", "a", "b", "c", "D")
	writeLines(t, repo, "sub/untracked.go", "a", "b")
	writeLines(t, repo, "outside/untracked.go", "a", "b")

	t.Chdir(filepath.Join(repo, "sub"))

	checker := Checker{}

	err := checker.Prepare(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertIsNew(t, &checker, map[string]bool{
		"existing.go:5":  true,
		"existing.go:1":  false,
		"untracked.go:1": true,
		"untracked.go:2": true,
	})

	for _, file := range []string{"other.go", "outside/other.go", "outside/untracked.go", "../outside/untracked.go"} {
		if _, ok := checker.changes[file]; ok {
			t.Errorf("file %q outside of the working directory must not be tracked", file)
		}
	}
}

// newRenameRepo creates a repository whose last commit moves outside/moved.go to <subdir>/moved.go
// (editing line 4), edits <subdir>/existing.go (line 3) and outside/other.go (line 2).
func newRenameRepo(t *testing.T, subdir string) string {
	t.Helper()

	repo := t.TempDir()

	runGit(t, repo, "init", "--initial-branch=main")
	runGit(t, repo, "config", "--local", "user.name", "testdata")
	runGit(t, repo, "config", "--local", "user.email", "testdata@example.com")

	writeLines(t, repo, "outside/moved.go", "a", "b", "c", "d", "e", "f", "g", "h")
	writeLines(t, repo, "outside/other.go", "a", "b", "c", "d")
	writeLines(t, repo, subdir+"/existing.go", "a", "b", "c", "d")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "base")

	runGit(t, repo, "mv", "outside/moved.go", subdir+"/moved.go")
	writeLines(t, repo, subdir+"/moved.go", "a", "b", "c", "D", "e", "f", "g", "h")
	writeLines(t, repo, "outside/other.go", "a", "B", "c", "d")
	writeLines(t, repo, subdir+"/existing.go", "a", "b", "C", "d")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "move")

	return repo
}

// assertIsNew checks [Checker.IsNew] for each "file:line" key.
func assertIsNew(t *testing.T, checker *Checker, want map[string]bool) {
	t.Helper()

	for key, isNewWant := range want {
		file, lineStr, _ := strings.Cut(key, ":")

		line, err := strconv.Atoi(lineStr)
		if err != nil {
			t.Fatal(err)
		}

		if _, isNew := checker.IsNew(file, line); isNew != isNewWant {
			t.Errorf("IsNew(%q, %d) = %v, want %v", file, line, isNew, isNewWant)
		}
	}
}

func TestRestrictToPrefix(t *testing.T) {
	patch := `diff --git a/outside/a.go b/outside/a.go
index 1111111..2222222 100644
--- a/outside/a.go
+++ b/outside/a.go
@@ -1 +1 @@
-old
+new
diff --git a/sub/b.go b/sub/b.go
index 1111111..2222222 100644
--- a/sub/b.go
+++ b/sub/b.go
@@ -1,2 +1,2 @@
 keep
-old
++ looks like a header
diff --git a/old/c.go b/sub/dir/c.go
similarity index 90%
rename from old/c.go
rename to sub/dir/c.go
index 1111111..2222222 100644
--- a/old/c.go
+++ b/sub/dir/c.go
@@ -1 +1 @@
-old
+new
diff --git a/sub/gone.go b/sub/gone.go
deleted file mode 100644
index 1111111..0000000
--- a/sub/gone.go
+++ /dev/null
@@ -1 +0,0 @@
-gone
diff --git a/sub/renamed.go b/sub2/renamed.go
similarity index 100%
rename from sub/renamed.go
rename to sub2/renamed.go
diff --git a/sub2/d.go b/sub2/d.go
index 1111111..2222222 100644
--- a/sub2/d.go
+++ b/sub2/d.go
@@ -1 +1 @@
-old
+new
`

	want := `diff --git a/sub/b.go b/sub/b.go
index 1111111..2222222 100644
--- a/sub/b.go
+++ b/b.go
@@ -1,2 +1,2 @@
 keep
-old
++ looks like a header
diff --git a/old/c.go b/sub/dir/c.go
similarity index 90%
rename from old/c.go
rename to sub/dir/c.go
index 1111111..2222222 100644
--- a/old/c.go
+++ b/dir/c.go
@@ -1 +1 @@
-old
+new
`

	have := restrictToPrefix(bytes.NewBufferString(patch), "sub/").String()

	if !reflect.DeepEqual(have, want) {
		t.Errorf("unexpected patch:\nhave:\n%s\nwant:\n%s", have, want)
	}
}

func TestRestrictToPrefix_spaces(t *testing.T) {
	patch := "diff --git a/my dir/a b.go b/my dir/a b.go\n--- a/my dir/a b.go\t\n+++ b/my dir/a b.go\t\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/my dir/c.go b/my dir/c.go\n--- a/my dir/c.go\t\n+++ b/my dir/c.go\t\n@@ -1 +1 @@\n-old\n+new\n"

	want := "diff --git a/my dir/a b.go b/my dir/a b.go\n--- a/my dir/a b.go\t\n+++ b/a b.go\t\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/my dir/c.go b/my dir/c.go\n--- a/my dir/c.go\t\n+++ b/c.go\n@@ -1 +1 @@\n-old\n+new\n"

	have := restrictToPrefix(bytes.NewBufferString(patch), "my dir/").String()
	if have != want {
		t.Errorf("unexpected patch:\nhave:\n%q\nwant:\n%q", have, want)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir

	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func writeLines(t *testing.T, dir, name string, lines ...string) {
	t.Helper()

	path := filepath.Join(dir, filepath.FromSlash(name))

	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}
