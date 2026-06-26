// main_test.go — drives the gitevolved CLI through run() (no process spawn) to
// prove the record → materialize → export → log pipeline works end-to-end over a
// real op-log file, plus the no-change and missing-flag error paths.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI invokes a subcommand and returns stdout + any error.
func runCLI(t *testing.T, cmd string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(cmd, args, &out, &out)
	return out.String(), err
}

func TestPipeline_record_materialize_export_log(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Run from the repo root (git-like): record takes repo-relative paths.
	dir := t.TempDir()
	t.Chdir(dir)
	logPath := "ops.jsonl"

	// Two source files on disk (relative to the repo root we cd'd into).
	if err := os.WriteFile("a.go", []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("b.go", []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// record both.
	if out, err := runCLI(t, "record", "--log", logPath, "--session", "s1", "a.go", "b.go"); err != nil {
		t.Fatalf("record: %v\n%s", err, out)
	} else if !strings.Contains(out, "recorded") {
		t.Fatalf("record output missing 'recorded': %q", out)
	}

	// re-record a.go unchanged → no-op.
	out, err := runCLI(t, "record", "--log", logPath, "a.go")
	if err != nil {
		t.Fatalf("record(no-change): %v", err)
	}
	if !strings.Contains(out, "no change") {
		t.Fatalf("re-record of unchanged file should print 'no change', got %q", out)
	}

	// materialize (list form) shows both files.
	out, err = runCLI(t, "materialize", "--log", logPath)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Fatalf("materialize list missing files: %q", out)
	}

	// export mints a git commit.
	gitRepo := filepath.Join(dir, "gitout")
	if e := exec.Command("git", "init", "-q", "-b", "main", gitRepo).Run(); e != nil {
		t.Fatalf("git init: %v", e)
	}
	out, err = runCLI(t, "export", "--log", logPath, "--git", gitRepo, "--subject", "seed export")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		t.Fatal("export printed no SHA")
	}
	// The committed tree must contain the recorded file content.
	show, e := exec.Command("git", "-C", gitRepo, "show", "HEAD:a.go").Output()
	if e != nil {
		t.Fatalf("git show: %v", e)
	}
	if string(show) != "package a\n" {
		t.Fatalf("exported tree a.go = %q, want %q", show, "package a\n")
	}

	// log lists the recorded ops (2 records: a + b; the no-op was dropped).
	out, err = runCLI(t, "log", "--log", logPath)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("log shows %d entries, want 2 (no-op record must not be logged):\n%s", len(lines), out)
	}
}

func TestRm_recordsDeletion(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	logPath := "ops.jsonl"
	if err := os.WriteFile("x.go", []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "record", "--log", logPath, "x.go"); err != nil {
		t.Fatalf("record: %v", err)
	}
	// rm a tracked path → removed.
	out, err := runCLI(t, "rm", "--log", logPath, "x.go")
	if err != nil {
		t.Fatalf("rm: %v", err)
	}
	if !strings.Contains(out, "removed") {
		t.Fatalf("rm of tracked path should print 'removed', got %q", out)
	}
	// rm an untracked path → not tracked, no error.
	out, err = runCLI(t, "rm", "--log", logPath, "nope.go")
	if err != nil {
		t.Fatalf("rm(untracked): %v", err)
	}
	if !strings.Contains(out, "not tracked") {
		t.Fatalf("rm of untracked path should print 'not tracked', got %q", out)
	}
	// After delete, materialize no longer lists the file.
	out, _ = runCLI(t, "materialize", "--log", logPath)
	if strings.Contains(out, "x.go") {
		t.Fatalf("materialize still lists deleted file: %q", out)
	}
}

func TestMissingLogFlag_errors(t *testing.T) {
	for _, cmd := range []string{"record", "rm", "materialize", "export", "log"} {
		if _, err := runCLI(t, cmd, "somefile"); err == nil {
			t.Errorf("%s without --log should error", cmd)
		}
	}
}

func TestUnknownCommand_errors(t *testing.T) {
	if _, err := runCLI(t, "frobnicate"); err == nil {
		t.Error("unknown command should error")
	}
}
