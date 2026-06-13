package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepo makes a temp git repo with one commit on branch main.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestExpandPath(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	cases := map[string]string{
		"~/projects": "/home/tester/projects",
		"~":          "/home/tester",
		"/abs/path":  "/abs/path",
		"relative":   "relative",
	}
	for in, want := range cases {
		if got := expandPath(in); got != want {
			t.Errorf("expandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListSubdirs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"alpha", "beta", ".hidden"} {
		os.Mkdir(filepath.Join(root, d), 0755)
	}
	os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0644)

	got := ListSubdirs(root)
	want := []string{filepath.Join(root, "alpha"), filepath.Join(root, "beta")}
	if !equalSlice(got, want) {
		t.Errorf("ListSubdirs = %v, want %v (hidden dirs and files excluded, sorted)", got, want)
	}
}

func TestListSubdirsMissing(t *testing.T) {
	if got := ListSubdirs("/no/such/dir/anywhere"); got != nil {
		t.Errorf("missing root should give nil, got %v", got)
	}
}

func TestGetProjectStatusNonExistent(t *testing.T) {
	ps := GetProjectStatus("/no/such/path/here")
	if ps.Exists || ps.IsGitRepo {
		t.Errorf("non-existent path: %+v", ps)
	}
}

func TestGetProjectStatusPlainDir(t *testing.T) {
	ps := GetProjectStatus(t.TempDir())
	if !ps.Exists {
		t.Error("plain dir should exist")
	}
	if ps.IsGitRepo {
		t.Error("plain dir should not be a git repo")
	}
}

func TestGetProjectStatusGitRepo(t *testing.T) {
	dir := initRepo(t)
	ps := GetProjectStatus(dir)
	if !ps.Exists || !ps.IsGitRepo {
		t.Fatalf("repo not detected: %+v", ps)
	}
	if ps.Branch != "main" {
		t.Errorf("branch = %q, want main", ps.Branch)
	}
	if ps.Dirty {
		t.Errorf("clean repo reported dirty: %+v", ps)
	}

	// An untracked file makes it dirty.
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0644)
	ps = GetProjectStatus(dir)
	if !ps.Dirty || ps.DirtyCount != 1 {
		t.Errorf("expected dirty with 1 change: %+v", ps)
	}
}

func TestIsGitRepo(t *testing.T) {
	if isGitRepo(t.TempDir()) {
		t.Error("plain dir reported as git repo")
	}
	if !isGitRepo(initRepo(t)) {
		t.Error("git repo not detected")
	}
}

func TestGetBranches(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "branch", "feature-x")
	branches := GetBranches(dir)
	if len(branches) != 2 {
		t.Fatalf("got %v, want 2 branches", branches)
	}
	found := map[string]bool{}
	for _, b := range branches {
		found[b] = true
	}
	if !found["main"] || !found["feature-x"] {
		t.Errorf("missing expected branches: %v", branches)
	}
}

func TestGetBranchesNonRepo(t *testing.T) {
	if got := GetBranches(t.TempDir()); got != nil {
		t.Errorf("non-repo branches = %v, want nil", got)
	}
}

func TestCheckoutBranchCreate(t *testing.T) {
	dir := initRepo(t)
	if err := CheckoutBranch(dir, "feature", true, ""); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if ps := GetProjectStatus(dir); ps.Branch != "feature" {
		t.Errorf("branch = %q, want feature", ps.Branch)
	}
}

func TestCheckoutBranchMissingErrors(t *testing.T) {
	dir := initRepo(t)
	err := CheckoutBranch(dir, "does-not-exist", false, "")
	if err == nil {
		t.Fatal("checkout of missing branch should error")
	}
	if err.Error() == "" {
		t.Error("gitError should carry a message")
	}
}

func TestGitErrorMessage(t *testing.T) {
	withMsg := &gitError{msg: "boom", err: errors.New("exit 1")}
	if withMsg.Error() != "boom" {
		t.Errorf("Error() = %q, want git output", withMsg.Error())
	}
	noMsg := &gitError{msg: "", err: errors.New("exit 1")}
	if noMsg.Error() != "exit 1" {
		t.Errorf("Error() = %q, want underlying error", noMsg.Error())
	}
}

func TestProjectStatusCacheAndInvalidate(t *testing.T) {
	dir := initRepo(t)
	first := GetProjectStatusCached(dir)
	second := GetProjectStatusCached(dir) // served from cache (same fingerprint)
	if first.Branch != second.Branch {
		t.Error("cached status should be stable")
	}
	InvalidateProjectCache(dir)
	// After invalidation it recomputes; result should still be correct.
	again := GetProjectStatusCached(dir)
	if !again.IsGitRepo {
		t.Error("recomputed status wrong after invalidation")
	}
}
