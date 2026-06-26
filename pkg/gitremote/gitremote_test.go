// gitremote_test.go — proves both directions of the remote-helper protocol:
//   - import: capabilities/list responses + a VALID fast-import stream (piped
//     through real `git fast-import`, reconstructing the tree incl. binary bytes);
//   - export: git's fast-export stream is parsed into recorded ops, droppped files
//     are deleted, an empty push is a no-op, and a merge commit fails loud.
//
// The full clone↔push↔clone-back round-trip against real git is exercised by the
// scratchpad e2e (see commit body); these are the deterministic unit guards.
package gitremote_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/gitremote"
)

// fakeSource is an import-only (read) source: a fixed tree + commit metadata.
type fakeSource struct {
	tree map[string][]byte
	info gitremote.CommitInfo
	has  bool
}

func (f fakeSource) Tree() (map[string][]byte, error) { return f.tree, nil }
func (f fakeSource) Head() (gitremote.CommitInfo, bool, error) {
	return f.info, f.has, nil
}

func newSource() fakeSource {
	return fakeSource{
		tree: map[string][]byte{
			"main.go":  []byte("package main\n\nfunc main() {}\n"),
			"pkg/x.go": []byte("package pkg\n"),
			"data.bin": {0x00, 0x01, 0x02, 0xff, 0x0a, 0x00}, // binary, embedded NUL + LF
		},
		info: gitremote.CommitInfo{
			Committer: "gitevolved <noreply@gitevolved.ai>",
			WhenUnix:  1782475200,
			TZ:        "+0000",
			Subject:   "doSource import test",
		},
		has: true,
	}
}

func TestCapabilities(t *testing.T) {
	var out bytes.Buffer
	if err := gitremote.Serve(strings.NewReader("capabilities\n"), &out, newSource()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "import") {
		t.Fatalf("capabilities must advertise import, got %q", got)
	}
	// Private tracking namespace — NOT refs/heads/*:refs/heads/* (that alias breaks push).
	if !strings.Contains(got, "refspec refs/heads/*:refs/dosource/heads/*") {
		t.Fatalf("capabilities must advertise the private-namespace refspec, got %q", got)
	}
}

func TestList_advertisesMain(t *testing.T) {
	var out bytes.Buffer
	if err := gitremote.Serve(strings.NewReader("list\n"), &out, newSource()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "? refs/heads/main") {
		t.Fatalf("list must advertise the remote ref refs/heads/main, got %q", got)
	}
	if !strings.Contains(got, "@refs/heads/main HEAD") {
		t.Fatalf("list must advertise the HEAD symref, got %q", got)
	}
}

func TestList_emptySource_noRefs(t *testing.T) {
	var out bytes.Buffer
	empty := fakeSource{tree: map[string][]byte{}, has: false}
	if err := gitremote.Serve(strings.NewReader("list\n"), &out, empty); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if strings.Contains(out.String(), "refs/heads") {
		t.Fatalf("empty source must advertise no refs, got %q", out.String())
	}
}

// TestImportStream_validViaGitFastImport is the real proof for import: the stream
// the helper emits, fed to `git fast-import`, must produce the tree at the tracking
// ref — including a binary blob with an embedded NUL and LF.
func TestImportStream_validViaGitFastImport(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := newSource()

	var stream bytes.Buffer
	if err := gitremote.Serve(strings.NewReader("import refs/heads/main\n\n"), &stream, src); err != nil {
		t.Fatalf("Serve(import): %v", err)
	}
	if !bytes.HasSuffix(stream.Bytes(), []byte("done\n")) {
		t.Fatalf("stream must end with `done`, got tail %q", tail(stream.Bytes(), 20))
	}

	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	fi := exec.Command("git", "-C", repo, "fast-import", "--quiet")
	fi.Stdin = bytes.NewReader(stream.Bytes())
	if out, err := fi.CombinedOutput(); err != nil {
		t.Fatalf("git fast-import rejected the stream: %v\n%s\n--- stream ---\n%s", err, out, stream.String())
	}

	// The stream writes the TRACKING ref (refspec RHS), not HEAD.
	const ref = "refs/dosource/heads/main"
	for path, want := range src.tree {
		out, err := exec.Command("git", "-C", repo, "show", ref+":"+path).Output()
		if err != nil {
			t.Errorf("git show %s:%s: %v", ref, path, err)
			continue
		}
		if !bytes.Equal(out, want) {
			t.Errorf("%s: imported %q != source %q", path, out, want)
		}
	}
	subj, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%s", ref).Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if strings.TrimSpace(string(subj)) != "doSource import test" {
		t.Fatalf("commit subject = %q, want %q", strings.TrimSpace(string(subj)), "doSource import test")
	}
}

func TestImportStream_emptySource_terminatorOnly(t *testing.T) {
	var out bytes.Buffer
	empty := fakeSource{tree: map[string][]byte{}, has: false}
	if err := gitremote.Serve(strings.NewReader("import refs/heads/main\n\n"), &out, empty); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if strings.TrimSpace(out.String()) != "done" {
		t.Fatalf("empty import should emit just `done`, got %q", out.String())
	}
}

func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

// fakeBackend is a push-capable Source: it records what export wrote.
type fakeBackend struct {
	cur      map[string][]byte
	recorded map[string][]byte
	deleted  []string
}

func newBackend(cur map[string][]byte) *fakeBackend {
	return &fakeBackend{cur: cur, recorded: map[string][]byte{}}
}

func (f *fakeBackend) Tree() (map[string][]byte, error) { return f.cur, nil }
func (f *fakeBackend) Head() (gitremote.CommitInfo, bool, error) {
	return gitremote.CommitInfo{Committer: "x <x@x>", TZ: "+0000"}, len(f.cur) > 0, nil
}
func (f *fakeBackend) RecordFile(p string, c []byte) (bool, error) {
	f.recorded[p] = c
	return true, nil
}
func (f *fakeBackend) RecordDelete(p string) (bool, error) {
	f.deleted = append(f.deleted, p)
	return true, nil
}

func dataBlock(content string) string {
	return "data " + itoa(len(content)) + "\n" + content + "\n"
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestExport_recordsTipTreeAndDeletesDropped(t *testing.T) {
	blob1 := "package main\n\nfunc main(){}\n"
	blob2 := "new\n"
	// Mirrors git's real fast-export framing: feature/reset/from lines tolerated.
	stream := "export\n" +
		"feature done\n" +
		"blob\nmark :1\n" + dataBlock(blob1) +
		"blob\nmark :2\n" + dataBlock(blob2) +
		"reset refs/heads/main\n" +
		"commit refs/heads/main\nmark :3\ncommitter t <t@t.t> 100 +0000\n" + dataBlock("second") +
		"from :0\nM 100644 :1 main.go\nM 100644 :2 c.go\ndone\n"

	be := newBackend(map[string][]byte{
		"main.go": []byte("package main\n"),
		"old.go":  []byte("package old\n"),
	})
	var out bytes.Buffer
	if err := gitremote.Serve(strings.NewReader(stream), &out, be); err != nil {
		t.Fatalf("Serve(export): %v", err)
	}

	if got := string(be.recorded["main.go"]); got != blob1 {
		t.Errorf("main.go recorded = %q, want %q", got, blob1)
	}
	if got := string(be.recorded["c.go"]); got != blob2 {
		t.Errorf("c.go recorded = %q, want %q", got, blob2)
	}
	if len(be.deleted) != 1 || be.deleted[0] != "old.go" {
		t.Errorf("deleted = %v, want [old.go]", be.deleted)
	}
	if !strings.Contains(out.String(), "ok refs/heads/main") {
		t.Errorf("export report missing `ok refs/heads/main`: %q", out.String())
	}
}

func TestExport_nonMainRef_failsLoudAndRecordsNothing(t *testing.T) {
	// A push to refs/heads/feature must be REJECTED before any record — doSource is
	// single-mainline by design, so silently landing feature's content on main would
	// be a wrong-target data write.
	stream := "export\n" +
		"blob\nmark :1\n" + dataBlock("package x\n") +
		"commit refs/heads/feature\nmark :2\ncommitter t <t@t.t> 100 +0000\n" + dataBlock("on feature") +
		"M 100644 :1 x.go\ndone\n"
	be := newBackend(map[string][]byte{"main.go": []byte("package main\n")})
	var out bytes.Buffer
	if err := gitremote.Serve(strings.NewReader(stream), &out, be); err != nil {
		t.Fatalf("Serve should report the rejection inline, not return an error: %v", err)
	}
	if len(be.recorded) != 0 || len(be.deleted) != 0 {
		t.Fatalf("a non-main push must record NOTHING, got recorded=%v deleted=%v", be.recorded, be.deleted)
	}
	if !strings.Contains(out.String(), "error refs/heads/feature") {
		t.Fatalf("must report `error refs/heads/feature`, got %q", out.String())
	}
	if !strings.Contains(out.String(), "single-mainline") {
		t.Errorf("error should explain the single-mainline limitation, got %q", out.String())
	}
}

func TestExport_multiRef_failsLoud(t *testing.T) {
	// Commits targeting two distinct refs in one stream is a multi-ref push we reject.
	stream := "export\n" +
		"blob\nmark :1\n" + dataBlock("a\n") +
		"commit refs/heads/main\nmark :2\ncommitter t <t@t.t> 100 +0000\n" + dataBlock("c1") +
		"M 100644 :1 a.go\n" +
		"commit refs/heads/other\nmark :3\ncommitter t <t@t.t> 101 +0000\n" + dataBlock("c2") +
		"M 100644 :1 b.go\ndone\n"
	be := newBackend(nil)
	var out bytes.Buffer
	err := gitremote.Serve(strings.NewReader(stream), &out, be)
	if err == nil {
		t.Fatal("a multi-ref push must fail loud, got nil error")
	}
	if !strings.Contains(err.Error(), "multiple refs") {
		t.Fatalf("error should name the multi-ref push, got %v", err)
	}
}

func TestExport_advertisedInCapabilities(t *testing.T) {
	var out bytes.Buffer
	if err := gitremote.Serve(strings.NewReader("capabilities\n"), &out, newBackend(nil)); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !strings.Contains(out.String(), "export") {
		t.Fatalf("push-capable backend must advertise export, got %q", out.String())
	}
}

func TestExport_importOnlySource_noExportCapability(t *testing.T) {
	var out bytes.Buffer
	if err := gitremote.Serve(strings.NewReader("capabilities\n"), &out, newSource()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if strings.Contains(out.String(), "export") {
		t.Fatalf("import-only source must NOT advertise export, got %q", out.String())
	}
}

func TestExport_noCommit_isNoOp(t *testing.T) {
	be := newBackend(map[string][]byte{"keep.go": []byte("x")})
	var out bytes.Buffer
	if err := gitremote.Serve(strings.NewReader("export\nfeature done\nreset refs/heads/main\ndone\n"), &out, be); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if len(be.recorded) != 0 || len(be.deleted) != 0 {
		t.Fatalf("empty push must be a no-op, got recorded=%v deleted=%v (must NOT delete-all)", be.recorded, be.deleted)
	}
	if !strings.Contains(out.String(), "ok refs/heads/main") {
		t.Fatalf("empty push should still report ok, got %q", out.String())
	}
}

func TestExport_mergeCommit_failsLoud(t *testing.T) {
	stream := "export\n" +
		"blob\nmark :1\n" + dataBlock("x\n") +
		"commit refs/heads/main\nmark :2\ncommitter t <t@t.t> 100 +0000\n" + dataBlock("m") +
		"from :1\nmerge :1\nM 100644 :1 a.go\ndone\n"
	be := newBackend(nil)
	var out bytes.Buffer
	err := gitremote.Serve(strings.NewReader(stream), &out, be)
	if err == nil {
		t.Fatal("a merge commit must fail loud (linear-only), got nil error")
	}
	if !strings.Contains(err.Error(), "merge") && !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error should name the unsupported merge, got %v", err)
	}
}
