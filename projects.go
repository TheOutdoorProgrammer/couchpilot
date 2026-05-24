package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type ProjectStatus struct {
	Path       string `json:"path"`
	Expanded   string `json:"expanded"`
	Group      string `json:"group,omitempty"`
	Exists     bool   `json:"exists"`
	IsGitRepo  bool   `json:"isGitRepo"`
	Branch     string `json:"branch,omitempty"`
	Upstream   string `json:"upstream,omitempty"`
	Ahead      int    `json:"ahead"`
	Behind     int    `json:"behind"`
	Dirty      bool   `json:"dirty"`
	DirtyCount int    `json:"dirtyCount"`
}

func ListSubdirs(rawRoot string) []string {
	expanded := expandPath(rawRoot)
	entries, err := os.ReadDir(expanded)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, filepath.Join(rawRoot, e.Name()))
	}
	sort.Strings(dirs)
	return dirs
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	return p
}

type statusCacheEntry struct {
	fingerprint string
	status      ProjectStatus
}

var (
	statusCacheMu sync.RWMutex
	statusCache   = map[string]statusCacheEntry{}
)

func projectFingerprint(expanded string) string {
	var b strings.Builder
	if info, err := os.Stat(expanded); err == nil {
		fmt.Fprintf(&b, "d=%d-%d|", info.ModTime().UnixNano(), info.Size())
	} else {
		b.WriteString("d=missing|")
	}
	for _, rel := range []string{".git/HEAD", ".git/index", ".git/packed-refs"} {
		if info, err := os.Stat(filepath.Join(expanded, rel)); err == nil {
			fmt.Fprintf(&b, "%s=%d|", rel, info.ModTime().UnixNano())
		}
	}
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func GetProjectStatusCached(rawPath string) ProjectStatus {
	expanded := expandPath(rawPath)
	fp := projectFingerprint(expanded)

	statusCacheMu.RLock()
	if e, ok := statusCache[rawPath]; ok && e.fingerprint == fp {
		statusCacheMu.RUnlock()
		return e.status
	}
	statusCacheMu.RUnlock()

	status := GetProjectStatus(rawPath)

	statusCacheMu.Lock()
	statusCache[rawPath] = statusCacheEntry{fingerprint: fp, status: status}
	statusCacheMu.Unlock()

	return status
}

func InvalidateProjectCache(rawPath string) {
	statusCacheMu.Lock()
	delete(statusCache, rawPath)
	statusCacheMu.Unlock()
}

func GetProjectStatus(rawPath string) ProjectStatus {
	ps := ProjectStatus{Path: rawPath, Expanded: expandPath(rawPath)}

	info, err := os.Stat(ps.Expanded)
	if err != nil || !info.IsDir() {
		return ps
	}
	ps.Exists = true

	if err := exec.Command("git", "-C", ps.Expanded, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return ps
	}
	ps.IsGitRepo = true

	if out, err := exec.Command("git", "-C", ps.Expanded, "branch", "--show-current").Output(); err == nil {
		ps.Branch = strings.TrimSpace(string(out))
	}

	if out, err := exec.Command("git", "-C", ps.Expanded, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").Output(); err == nil {
		ps.Upstream = strings.TrimSpace(string(out))
		if cnt, err := exec.Command("git", "-C", ps.Expanded, "rev-list", "--left-right", "--count", "@{u}...HEAD").Output(); err == nil {
			fields := strings.Fields(strings.TrimSpace(string(cnt)))
			if len(fields) == 2 {
				fmt.Sscanf(fields[0], "%d", &ps.Behind)
				fmt.Sscanf(fields[1], "%d", &ps.Ahead)
			}
		}
	}

	if out, err := exec.Command("git", "-C", ps.Expanded, "status", "--porcelain").Output(); err == nil {
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		count := 0
		for _, l := range lines {
			if l != "" {
				count++
			}
		}
		ps.DirtyCount = count
		ps.Dirty = count > 0
	}

	return ps
}

func isGitRepo(dir string) bool {
	return exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Run() == nil
}

func GetBranches(rawPath string) []string {
	dir := expandPath(rawPath)
	out, err := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(refname:short)", "refs/heads").Output()
	if err != nil {
		return nil
	}
	branches := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}

func CheckoutBranch(rawPath, branch string, create bool, base string) error {
	dir := expandPath(rawPath)
	args := []string{"-C", dir, "checkout"}
	if create {
		args = append(args, "-b", branch)
		if base != "" {
			args = append(args, base)
		}
	} else {
		args = append(args, branch)
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return &gitError{msg: strings.TrimSpace(string(out)), err: err}
	}
	return nil
}

type gitError struct {
	msg string
	err error
}

func (e *gitError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return e.err.Error()
}
