// main_test.go — proves the dosource:// URL parse, that the localSource projects
// a real op-log, and (the load-bearing one) the full bidirectional transport
// against REAL git: build the helper, `git push dosource://`, then
// `git clone dosource://` back and assert the tree round-trips (incl. a deletion).
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/localrepo"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in        string
		wantCloud bool
		wantVal   string
		wantErr   bool
	}{
		{"dosource:///abs/path/ops.jsonl", false, "/abs/path/ops.jsonl", false},
		{"dosource://rel/ops.jsonl", false, "rel/ops.jsonl", false},
		{"dosource://cloud/example-repo", true, "example-repo", false},
		{"dosource://cloud/", false, "", true}, // cloud with no repo
		{"dosource://", false, "", true},
		{"https://example.com", false, "", true},
		{"ops.jsonl", false, "", true},
	}
	for _, c := range cases {
		isCloud, val, err := parseTarget(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseTarget(%q) = (%v,%q), want error", c.in, isCloud, val)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTarget(%q) errored: %v", c.in, err)
			continue
		}
		if isCloud != c.wantCloud || val != c.wantVal {
			t.Errorf("parseTarget(%q) = (%v,%q), want (%v,%q)", c.in, isCloud, val, c.wantCloud, c.wantVal)
		}
	}
}

func TestLocalSource_projectsOpLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ops.jsonl")

	// An empty/absent op-log: Head reports ok=false (empty clone).
	s := &localSource{oplogPath: logPath}
	if _, ok, err := s.Head(); err != nil || ok {
		t.Fatalf("Head on empty log = ok %v err %v, want false/nil", ok, err)
	}
	tree, err := s.Tree()
	if err != nil {
		t.Fatalf("Tree on empty log: %v", err)
	}
	if len(tree) != 0 {
		t.Fatalf("empty log projected %d files, want 0", len(tree))
	}

	// Record two files through the engine, then the source must project them.
	repo, err := localrepo.Open(logPath, "writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecordFile("main.go", []byte("package main\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecordFile("pkg/x.go", []byte("package pkg\n")); err != nil {
		t.Fatal(err)
	}

	tree, err = s.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if got := string(tree["main.go"]); got != "package main\n" {
		t.Fatalf("Tree main.go = %q", got)
	}
	if _, ok := tree["pkg/x.go"]; !ok {
		t.Fatalf("Tree missing pkg/x.go: %v", tree)
	}

	info, ok, err := s.Head()
	if err != nil || !ok {
		t.Fatalf("Head after writes = ok %v err %v, want true/nil", ok, err)
	}
	if info.Committer == "" || info.TZ == "" {
		t.Fatalf("Head returned incomplete CommitInfo: %+v", info)
	}
	if info.WhenUnix == 0 {
		t.Fatalf("Head WhenUnix is zero, want the latest op's timestamp")
	}
}

// gitEnv returns an env with the helper's dir prepended to PATH plus deterministic
// git identity, so `git push/clone dosource://` finds git-remote-dosource.
func gitEnv(binDir string) []string {
	env := append([]string{}, os.Environ()...)
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + binDir + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
		}
	}
	return append(env,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t",
	)
}

func git(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestRoundTrip_realGit is the seam test that matters: it builds the actual helper
// binary and exercises the FULL transport through real git — push a repo (with a
// deletion) to a dosource:// op-log, then clone it back and assert the tip tree
// round-trips exactly. A mock cannot catch a remote-helper protocol break; this can.
func TestRoundTrip_realGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Build the helper under the exact name git derives from the scheme.
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, "git-remote-dosource"), ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	env := gitEnv(binDir)
	oplog := filepath.Join(tmp, "ops.jsonl")

	// A source repo with history including a deletion.
	repo := filepath.Join(tmp, "repo")
	git(t, env, "", "init", "-q", "-b", "main", repo)
	mustWrite(t, filepath.Join(repo, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(repo, "sub/old.txt"), "temp\n")
	git(t, env, repo, "add", "-A")
	git(t, env, repo, "commit", "-qm", "first")
	mustWrite(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() {}\n")
	git(t, env, repo, "rm", "-q", "sub/old.txt")
	mustWrite(t, filepath.Join(repo, "pkg.go"), "package pkg\n")
	git(t, env, repo, "add", "-A")
	git(t, env, repo, "commit", "-qm", "second")

	// PUSH to the op-log.
	git(t, env, repo, "push", "dosource://"+oplog, "main")

	// CLONE back.
	back := filepath.Join(tmp, "back")
	git(t, env, "", "clone", "-q", "dosource://"+oplog, back)

	files := strings.Fields(git(t, env, back, "ls-files"))
	got := strings.Join(files, " ")
	if want := "main.go pkg.go"; got != want {
		t.Fatalf("round-trip tree = %q, want %q (deletion must propagate)", got, want)
	}
	if mainGo := git(t, env, back, "show", "HEAD:main.go"); mainGo != "package main\n\nfunc main() {}\n" {
		t.Fatalf("round-trip main.go = %q", mainGo)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
