// git_test.go — hermetic thesis-driven tests for the doSource ->
// git exporter.
//
// # Pattern
//
// Each test stages a tiny git repo in a tempdir, runs ExportCommit
// with a known State + op-log + fixed dates, then asserts the
// observable invariants the thesis claims:
//
//	T1. ForwardOnlyFidelity        export -> fresh clone -> checkout -> State byte-equal
//	T2. TrailersIncludeAllOps      every envelope produces an X-DoSource-Operation trailer
//	T3. DeterministicSHA            same input + fixed dates -> same commit SHA
//	T4. StateDeletesRemovesFile    file in HEAD but not in new state is removed
//	T5. RejectsNonRepoPath         non-git path -> ErrNotAGitRepo
//	T6. RejectsPathTraversalKeys   "../escape" key -> ErrPathTraversal
//	T7. RejectsMissingDates        zero AuthorDate -> ErrMissingDates
//
// # Why hermetic real-git
//
// We use the real git binary in tempdirs (same pattern as
// dosource/conflictpred/conflictpred_test.go). The contract is "we
// drive real git correctly" — mocking exec.Cmd would test our wrapper
// but not our git invocation, which is the whole interesting part.
package export

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// initRepo creates a fresh git repo in a tempdir and returns the path.
// Skips the test if git isn't available.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not on PATH; skipping export tests")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// Set local user.name / user.email so commits work without global config.
	for _, kv := range [][]string{
		{"user.name", "doSource Test"},
		{"user.email", "test@dosource.local"},
	} {
		cmd := exec.Command("git", "-C", dir, "config", kv[0], kv[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v: %s", kv[0], err, out)
		}
	}
	return dir
}

// fixedDates returns a deterministic AuthorDate/CommitterDate pair.
func fixedDates() (time.Time, time.Time) {
	d := time.Date(2026, 5, 14, 18, 30, 0, 0, time.UTC)
	return d, d
}

// sealOps wraps each Operation in an Envelope sealed with deterministic
// metadata so OpIDs are stable across runs.
func sealOps(t *testing.T, ops ...operation.Operation) []*operation.Envelope {
	t.Helper()
	envs := make([]*operation.Envelope, 0, len(ops))
	for i, op := range ops {
		env := &operation.Envelope{
			SourceSession: "test-sess",
			SourceTurn:    i + 1,
			EmittedAt:     time.Date(2026, 5, 14, 18, 30, i, 0, time.UTC),
		}
		if err := env.Seal(op); err != nil {
			t.Fatalf("Seal op %d: %v", i, err)
		}
		envs = append(envs, env)
	}
	return envs
}

// -----------------------------------------------------------------
// T1. ForwardOnlyFidelity — export → fresh clone → State byte-equal
// -----------------------------------------------------------------
//
// The headline claim of the export package: a doSource commit, when
// applied to a fresh clone, materializes byte-equal to the input
// State. This is what makes git a faithful serialization for
// non-doSource consumers (GitHub, GitLab, CI).
func TestThesis_ForwardOnlyFidelity(t *testing.T) {
	repo := initRepo(t)
	state := projector.State{
		"auth.go":    []byte("package auth\n\nfunc validateToken() error { return nil }\n"),
		"sub/dir.go": []byte("package sub\n"),
		"empty.txt":  []byte(""),
	}
	envs := sealOps(t,
		&operation.AddFile{Path: "auth.go", Content: state["auth.go"]},
		&operation.AddFile{Path: "sub/dir.go", Content: state["sub/dir.go"]},
		&operation.AddFile{Path: "empty.txt", Content: state["empty.txt"]},
	)
	authorDate, committerDate := fixedDates()
	sha, err := ExportCommit(repo, state, envs, CommitOptions{
		Subject:       "thesis test commit",
		AuthorDate:    authorDate,
		CommitterDate: committerDate,
	})
	if err != nil {
		t.Fatalf("ExportCommit: %v", err)
	}
	if sha == "" {
		t.Fatal("ExportCommit returned empty SHA")
	}

	// Clone the repo to a fresh path and verify State byte-equal.
	clone := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	for path, want := range state {
		got, err := os.ReadFile(filepath.Join(clone, path))
		if err != nil {
			t.Errorf("read %s from clone: %v", path, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("byte-equal violated for %s:\n  want = %q\n  got  = %q", path, want, got)
		}
	}
}

// -----------------------------------------------------------------
// T8. ExecutableBitFromShebang — a materialized script stays executable
// -----------------------------------------------------------------
//
// doSource state carries NO file mode, so the exporter infers the
// executable bit from a leading shebang. Without it every file lands
// 0644 and a DR rebuild from the lifeboat fails the instant a build step
// execs a script directly (caught 2026-06-24: the Swift "Bootstrap
// bundled Node.js toolchain" phase hit Permission denied; 251 of 255
// tracked-executable files are shebang scripts). git records the bit in
// the tree mode, so a fresh clone — what a true DR reconstruction gets —
// must show 100755 for the script and 100644 for plain data.
func TestThesis_ExecutableBitFromShebang(t *testing.T) {
	repo := initRepo(t)
	state := projector.State{
		"run.sh":   []byte("#!/usr/bin/env bash\necho hi\n"),
		"data.txt": []byte("not executable\n"),
	}
	envs := sealOps(t,
		&operation.AddFile{Path: "run.sh", Content: state["run.sh"]},
		&operation.AddFile{Path: "data.txt", Content: state["data.txt"]},
	)
	authorDate, committerDate := fixedDates()
	if _, err := ExportCommit(repo, state, envs, CommitOptions{
		Subject: "exec-bit test", AuthorDate: authorDate, CommitterDate: committerDate,
	}); err != nil {
		t.Fatalf("ExportCommit: %v", err)
	}

	clone := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	modeOf := func(path string) string {
		out, err := exec.Command("git", "-C", clone, "ls-files", "-s", path).Output()
		if err != nil {
			t.Fatalf("ls-files %s: %v", path, err)
		}
		f := strings.Fields(string(out))
		if len(f) == 0 {
			t.Fatalf("%s not tracked in clone", path)
		}
		return f[0]
	}
	if m := modeOf("run.sh"); m != "100755" {
		t.Errorf("shebang script run.sh: git mode = %s, want 100755 (DR rebuild would fail to exec it)", m)
	}
	if m := modeOf("data.txt"); m != "100644" {
		t.Errorf("plain data.txt: git mode = %s, want 100644", m)
	}
}

func TestFileModeForContent(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want os.FileMode
	}{
		{"shebang bash", []byte("#!/bin/bash\necho\n"), 0o755},
		{"shebang env python", []byte("#!/usr/bin/env python3\n"), 0o755},
		{"plain go", []byte("package main\n"), 0o644},
		{"empty", []byte(""), 0o644},
		{"single hash byte", []byte("#"), 0o644},
		{"hash but not bang", []byte("# a comment\n"), 0o644},
	}
	for _, c := range cases {
		if got := fileModeForContent(c.data); got != c.want {
			t.Errorf("%s: fileModeForContent = %o, want %o", c.name, got, c.want)
		}
	}
}

// -----------------------------------------------------------------
// T2. TrailersIncludeAllOps — one trailer per envelope
// -----------------------------------------------------------------
//
// The structured trailer is what preserves typed-op intent across the
// git export boundary. Phase 2 may consume these for bidirectional
// sync; for v1, the test just asserts presence + correct format.
func TestThesis_TrailersIncludeAllOps(t *testing.T) {
	repo := initRepo(t)
	envs := sealOps(t,
		&operation.AddFile{Path: "a.go", Content: []byte("x")},
		&operation.RenameSymbol{Path: "a.go", OldName: "getUser", NewName: "fetchUser", Scope: operation.ScopeRef{Path: "a.go"}},
		&operation.AddFunction{Path: "a.go", Name: "validateToken", Signature: "func validateToken()", Language: operation.LanguageGo},
	)
	state := projector.State{"a.go": []byte("x")}
	authorDate, committerDate := fixedDates()
	sha, err := ExportCommit(repo, state, envs, CommitOptions{
		Subject:       "trailer test",
		AuthorDate:    authorDate,
		CommitterDate: committerDate,
	})
	if err != nil {
		t.Fatalf("ExportCommit: %v", err)
	}

	// Read the commit message back.
	out, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%B", sha).Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	msg := string(out)

	// Each op type must appear in a trailer.
	wantSubstrings := []string{
		"X-DoSource-Operation: AddFile(",
		"X-DoSource-Operation: RenameSymbol(",
		"X-DoSource-Operation: AddFunction(",
		"getUser->fetchUser",
		"name=\"validateToken\"",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(msg, s) {
			t.Errorf("commit message missing %q\n--- full message ---\n%s", s, msg)
		}
	}

	// Exactly N trailers for N envelopes.
	got := strings.Count(msg, "X-DoSource-Operation:")
	if got != len(envs) {
		t.Errorf("trailer count = %d, want %d\n--- message ---\n%s", got, len(envs), msg)
	}
}

// -----------------------------------------------------------------
// T3. DeterministicSHA — same input + fixed dates -> same SHA
// -----------------------------------------------------------------
//
// Determinism is what makes git export reproducible for testing,
// caching, and audit. Wall-clock dates would make every export
// produce a different SHA.
func TestThesis_DeterministicSHA(t *testing.T) {
	makeRepo := func() string {
		dir := initRepo(t)
		return dir
	}
	state := projector.State{"a.go": []byte("package a\n")}
	envs := sealOps(t, &operation.AddFile{Path: "a.go", Content: state["a.go"]})
	authorDate, committerDate := fixedDates()
	opts := CommitOptions{
		Subject:       "deterministic test",
		Author:        "Test <test@dosource.local>",
		AuthorDate:    authorDate,
		Committer:     "Test <test@dosource.local>",
		CommitterDate: committerDate,
	}

	repoA := makeRepo()
	shaA, err := ExportCommit(repoA, state, envs, opts)
	if err != nil {
		t.Fatalf("export A: %v", err)
	}

	repoB := makeRepo()
	shaB, err := ExportCommit(repoB, state, envs, opts)
	if err != nil {
		t.Fatalf("export B: %v", err)
	}

	if shaA != shaB {
		t.Errorf("determinism violated: shaA=%s shaB=%s", shaA, shaB)
	}
}

// -----------------------------------------------------------------
// T4. StateDeletesRemovesFile
// -----------------------------------------------------------------
//
// A file present in HEAD but absent from the new State must be
// removed from the next commit. Tests that materializeState handles
// the deletion path correctly.
func TestThesis_StateDeletesRemovesFile(t *testing.T) {
	repo := initRepo(t)
	authorDate, committerDate := fixedDates()
	opts := CommitOptions{
		Subject:       "first",
		AuthorDate:    authorDate,
		CommitterDate: committerDate,
	}

	// First commit: a.go + b.go.
	state1 := projector.State{
		"a.go": []byte("aaa"),
		"b.go": []byte("bbb"),
	}
	envs1 := sealOps(t,
		&operation.AddFile{Path: "a.go", Content: state1["a.go"]},
		&operation.AddFile{Path: "b.go", Content: state1["b.go"]},
	)
	if _, err := ExportCommit(repo, state1, envs1, opts); err != nil {
		t.Fatalf("first export: %v", err)
	}

	// Second commit: only a.go (b.go should be deleted).
	state2 := projector.State{"a.go": []byte("aaa")}
	envs2 := sealOps(t, &operation.DeleteFile{Path: "b.go"})
	opts.Subject = "second"
	opts.AuthorDate = opts.AuthorDate.Add(time.Hour)
	opts.CommitterDate = opts.CommitterDate.Add(time.Hour)

	sha2, err := ExportCommit(repo, state2, envs2, opts)
	if err != nil {
		t.Fatalf("second export: %v", err)
	}

	// b.go must not exist in working tree.
	if _, err := os.Stat(filepath.Join(repo, "b.go")); !os.IsNotExist(err) {
		t.Errorf("b.go still exists after export with state lacking it")
	}
	// b.go must not be tracked in HEAD.
	out, _ := exec.Command("git", "-C", repo, "ls-tree", "--name-only", sha2).Output()
	tracked := string(out)
	if strings.Contains(tracked, "b.go") {
		t.Errorf("b.go still tracked in HEAD: %s", tracked)
	}
}

// -----------------------------------------------------------------
// T5. RejectsNonRepoPath
// -----------------------------------------------------------------
func TestThesis_RejectsNonRepoPath(t *testing.T) {
	notARepo := t.TempDir() // tempdir, no git init
	authorDate, committerDate := fixedDates()
	_, err := ExportCommit(notARepo, projector.State{}, nil, CommitOptions{
		Subject:       "x",
		AuthorDate:    authorDate,
		CommitterDate: committerDate,
	})
	if !errors.Is(err, ErrNotAGitRepo) {
		t.Errorf("expected ErrNotAGitRepo, got %v", err)
	}
}

// -----------------------------------------------------------------
// T6. RejectsPathTraversalKeys
// -----------------------------------------------------------------
//
// Malicious or buggy extractor output that produces "../escape" keys
// must be rejected before any disk write. This is the defensive
// boundary against path-traversal attacks.
func TestThesis_RejectsPathTraversalKeys(t *testing.T) {
	repo := initRepo(t)
	cases := []string{
		"../escape.go",
		"/abs/escape.go",
		"sub/../../escape.go",
	}
	authorDate, committerDate := fixedDates()
	opts := CommitOptions{Subject: "x", AuthorDate: authorDate, CommitterDate: committerDate}

	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			state := projector.State{key: []byte("evil")}
			_, err := ExportCommit(repo, state, nil, opts)
			if !errors.Is(err, ErrPathTraversal) {
				t.Errorf("expected ErrPathTraversal for %q, got %v", key, err)
			}
		})
	}
}

// -----------------------------------------------------------------
// T7. RejectsMissingDates
// -----------------------------------------------------------------
//
// Without fixed dates, commits are non-reproducible. The function
// requires them up front.
func TestThesis_RejectsMissingDates(t *testing.T) {
	repo := initRepo(t)
	_, err := ExportCommit(repo, projector.State{}, nil, CommitOptions{
		Subject: "x",
		// AuthorDate + CommitterDate intentionally omitted.
	})
	if !errors.Is(err, ErrMissingDates) {
		t.Errorf("expected ErrMissingDates, got %v", err)
	}
}

// -----------------------------------------------------------------
// Defensive: missing subject is named
// -----------------------------------------------------------------

func TestRejectsMissingSubject(t *testing.T) {
	repo := initRepo(t)
	authorDate, committerDate := fixedDates()
	_, err := ExportCommit(repo, projector.State{}, nil, CommitOptions{
		AuthorDate:    authorDate,
		CommitterDate: committerDate,
	})
	if !errors.Is(err, ErrMissingSubject) {
		t.Errorf("expected ErrMissingSubject, got %v", err)
	}
}
