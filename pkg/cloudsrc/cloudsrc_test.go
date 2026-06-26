// cloudsrc_test.go — proves the cloud materialize flow end-to-end against an
// httptest fake of the doSource API (commits → commit-detail → blob-urls →
// presigned GET): the tree assembles with correct content incl. an empty file and
// a shared blob, Bearer auth is sent, an empty mainline yields no tree, and a
// hash-mismatched blob fails CLOSED (never a silently-incomplete clone).
package cloudsrc_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/cloudsrc"
	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

func h(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeCloud serves the four verbs over httptest. blobs maps content-hash → bytes;
// tamper, if set, corrupts the body returned for that hash (integrity test).
type fakeCloud struct {
	tipSHA  string
	files   []map[string]string // {path, contentHash}
	blobs   map[string]string
	tamper  string
	gotAuth string
}

func (f *fakeCloud) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)

	mux.HandleFunc("/v1/source/commits", func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		if f.tipSHA == "" {
			writeJSON(w, map[string]any{"commits": []any{}})
			return
		}
		writeJSON(w, map[string]any{"commits": []any{map[string]any{"sha": f.tipSHA}}})
	})
	mux.HandleFunc("/v1/source/commit-detail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"files": f.files})
	})
	mux.HandleFunc("/v1/source/blob-urls", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Hashes []string `json:"hashes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		urls := map[string]string{}
		for _, hh := range req.Hashes {
			urls[hh] = srv.URL + "/blob/" + strings.TrimPrefix(hh, "sha256:")
		}
		writeJSON(w, map[string]any{"urls": urls})
	})
	mux.HandleFunc("/blob/", func(w http.ResponseWriter, r *http.Request) {
		hexHash := strings.TrimPrefix(r.URL.Path, "/blob/")
		full := "sha256:" + hexHash
		body := f.blobs[full]
		if f.tamper == full {
			body = body + "-corrupted"
		}
		_, _ = w.Write([]byte(body))
	})
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestTree_materializesFullTree(t *testing.T) {
	mainGo := "package main\n\nfunc main() {}\n"
	shared := "package shared\n"
	f := &fakeCloud{
		tipSHA: "sha-bundle_test",
		files: []map[string]string{
			{"path": "main.go", "contentHash": h(mainGo)},
			{"path": "a/shared.go", "contentHash": h(shared)},
			{"path": "b/shared.go", "contentHash": h(shared)}, // same blob, two paths
			{"path": "EMPTY", "contentHash": h("")},           // empty file
		},
		blobs: map[string]string{h(mainGo): mainGo, h(shared): shared},
	}
	srv := f.server(t)

	c := cloudsrc.New(srv.URL+"/v1/source", "test-key", "", "example-repo", nil)
	tree, err := c.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if got := string(tree["main.go"]); got != mainGo {
		t.Errorf("main.go = %q", got)
	}
	if got := string(tree["a/shared.go"]); got != shared {
		t.Errorf("a/shared.go = %q", got)
	}
	if got := string(tree["b/shared.go"]); got != shared {
		t.Errorf("b/shared.go = %q (shared blob must resolve for both paths)", got)
	}
	if v, ok := tree["EMPTY"]; !ok || len(v) != 0 {
		t.Errorf("EMPTY = %v (want present, zero bytes)", v)
	}
	if len(tree) != 4 {
		t.Fatalf("tree has %d files, want 4", len(tree))
	}
	if !strings.HasPrefix(f.gotAuth, "Bearer ") {
		t.Errorf("API call missing Bearer auth, got %q", f.gotAuth)
	}
}

func TestHead_emptyMainline_noTree(t *testing.T) {
	f := &fakeCloud{tipSHA: ""} // empty mainline
	srv := f.server(t)
	c := cloudsrc.New(srv.URL+"/v1/source", "k", "", "repo", nil)

	if _, ok, err := c.Head(); err != nil || ok {
		t.Fatalf("Head on empty mainline = ok %v err %v, want false/nil", ok, err)
	}
	tree, err := c.Tree()
	if err != nil || len(tree) != 0 {
		t.Fatalf("Tree on empty mainline = %v (err %v), want empty", tree, err)
	}
}

func TestTree_hashMismatch_failsClosed(t *testing.T) {
	body := "package x\n"
	f := &fakeCloud{
		tipSHA: "sha-x",
		files:  []map[string]string{{"path": "x.go", "contentHash": h(body)}},
		blobs:  map[string]string{h(body): body},
		tamper: h(body), // server returns corrupted bytes
	}
	srv := f.server(t)
	c := cloudsrc.New(srv.URL+"/v1/source", "k", "", "repo", nil)

	if _, err := c.Tree(); err == nil {
		t.Fatal("a hash-mismatched blob must fail closed, got nil error")
	} else if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("error should name the integrity mismatch, got %v", err)
	}
}

// pushRecorder captures the write-side calls the client makes.
type pushRecorder struct {
	attached     bool
	attachIdemOK bool
	added        map[string]string // path → decoded body (snapshot adds)
	deleted      []string          // paths shipped as typedOp DeleteFile envelopes
	pushed       bool
	sessionOK    bool
}

func (p *pushRecorder) server(t *testing.T) *httptest.Server {
	p.added = map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/source/attach", func(w http.ResponseWriter, r *http.Request) {
		p.attached = true
		// The live /attach verb requires an X-Idempotency-Key (claim-before-act
		// fence) — the gap that caused the first scratch-repo prod-smoke to 400.
		if r.Header.Get("X-Idempotency-Key") != "" {
			p.attachIdemOK = true
		}
		writeJSON(w, map[string]any{"sessionId": "sess-test-123"})
	})
	mux.HandleFunc("/v1/source/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Session-Id") == "sess-test-123" {
			p.sessionOK = true
		}
		var req struct {
			Path    string              `json:"path"`
			Body    []byte              `json:"body"` // JSON base64 → []byte
			TypedOp *operation.Envelope `json:"typedOp"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.TypedOp != nil {
			// A deletion: decode the sealed envelope and record the DeleteFile path.
			op, err := req.TypedOp.Decode()
			if err != nil {
				t.Errorf("typedOp failed to decode: %v", err)
			} else if del, ok := op.(*operation.DeleteFile); ok {
				p.deleted = append(p.deleted, del.Path)
			} else {
				t.Errorf("typedOp decoded to %T, want *operation.DeleteFile", op)
			}
		} else {
			p.added[req.Path] = string(req.Body)
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/v1/source/push", func(w http.ResponseWriter, r *http.Request) {
		p.pushed = true
		writeJSON(w, map[string]any{"ok": true, "bundleId": "bundle_test"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPush_stageAddPush(t *testing.T) {
	p := &pushRecorder{}
	srv := p.server(t)
	c := cloudsrc.New(srv.URL+"/v1/source", "k", "", "scratch-repo", nil)

	if _, err := c.RecordFile("main.go", []byte("package main\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordFile("pkg/x.go", []byte("package pkg\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if !p.attached || !p.pushed || !p.sessionOK || !p.attachIdemOK {
		t.Fatalf("flush did attach=%v attachIdemKey=%v push=%v sessionHeader=%v, want all true", p.attached, p.attachIdemOK, p.pushed, p.sessionOK)
	}
	if p.added["main.go"] != "package main\n" {
		t.Errorf("main.go body = %q (base64 round-trip via /add)", p.added["main.go"])
	}
	if p.added["pkg/x.go"] != "package pkg\n" {
		t.Errorf("pkg/x.go body = %q", p.added["pkg/x.go"])
	}
}

func TestPush_emptyStage_noOp(t *testing.T) {
	p := &pushRecorder{}
	srv := p.server(t)
	c := cloudsrc.New(srv.URL+"/v1/source", "k", "", "scratch-repo", nil)
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush(empty): %v", err)
	}
	if p.attached || p.pushed {
		t.Fatalf("empty flush hit the network (attach=%v push=%v), want no-op", p.attached, p.pushed)
	}
}

func TestPush_deleteShipsTypedOp(t *testing.T) {
	p := &pushRecorder{}
	srv := p.server(t)
	c := cloudsrc.New(srv.URL+"/v1/source", "k", "", "scratch-repo", nil)

	// An incremental push that modifies one file and removes another.
	if _, err := c.RecordFile("keep.go", []byte("package keep\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordDelete("gone.go"); err != nil {
		t.Fatalf("RecordDelete should stage, not fail: %v", err)
	}
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if p.added["keep.go"] != "package keep\n" {
		t.Errorf("keep.go add body = %q", p.added["keep.go"])
	}
	if len(p.deleted) != 1 || p.deleted[0] != "gone.go" {
		t.Fatalf("deleted = %v, want [gone.go] shipped as a typedOp DeleteFile", p.deleted)
	}
	if !p.pushed {
		t.Fatal("bundle was not pushed")
	}
}

func TestRecordDelete_thenRecordFile_cancels(t *testing.T) {
	p := &pushRecorder{}
	srv := p.server(t)
	c := cloudsrc.New(srv.URL+"/v1/source", "k", "", "scratch-repo", nil)

	// A delete followed by a re-add of the same path: the add wins, no deletion.
	if _, err := c.RecordDelete("x.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordFile("x.go", []byte("package x\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(p.deleted) != 0 {
		t.Fatalf("re-add must cancel the pending delete, got deleted=%v", p.deleted)
	}
	if p.added["x.go"] != "package x\n" {
		t.Errorf("x.go should be added, got %q", p.added["x.go"])
	}
}

func TestHead_reportsTip(t *testing.T) {
	f := &fakeCloud{tipSHA: "sha-bundle_abc", files: nil, blobs: map[string]string{}}
	srv := f.server(t)
	c := cloudsrc.New(srv.URL+"/v1/source", "k", "", "example-repo", nil)
	info, ok, err := c.Head()
	if err != nil || !ok {
		t.Fatalf("Head = ok %v err %v, want true/nil", ok, err)
	}
	if !strings.Contains(info.Subject, "sha-bundle_abc") || !strings.Contains(info.Subject, "example-repo") {
		t.Fatalf("Head subject = %q, want repo + tip sha", info.Subject)
	}
}
