// Package cloudsrc is the cloud read backend for the git-remote-dosource helper:
// it implements gitremote.Source by materializing a repo's tip tree from the
// doSource cloud HTTP API, so `git clone dosource://cloud/<repo>` produces a real
// git repo from the hosted op-log. It is the funnel's on-ramp #1 (discover →
// clone → use) — the free local client reaching the paid cloud.
//
// # Why this exists
//
// The local Source projects a local op-log; this one projects the CLOUD repo over
// the same gitremote.Source seam, so gitremote.Serve is untouched. It is a THIN
// ADAPTER to verbs that already exist (consistent with the platform's
// "protocol-adapter, not a new store/auth" rule): no new auth model, no new
// endpoints — it reuses the doSource cloud API a doSource API key already grants.
//
// # The contract (audited + live-verified 2026-06-26 against api.doapps.cloud)
//
//	tip       GET  /commits?repoId=<repo>&branch=main&limit=1   → {commits:[{sha}]}
//	manifest  GET  /commit-detail?repoId=<repo>&sha=<sha>&bodies=false → {files:[{path,contentHash}]}
//	presign   POST /blob-urls {hashes:["sha256:…"]}             → {urls:{hash:presignedS3URL}}  (≤2000/call)
//	download  GET  <presignedS3URL>                             → raw bytes (sha256-verified)
//
// Auth: Authorization: Bearer <doSource API key>; the server derives the tenant
// from the bearer (no X-Tenant-Id needed). The empty-content blob is never stored
// in S3 — it is assembled directly as zero bytes.
//
// # Boundary
//
// OPEN component: pure Go standard library — net/http for the API + the
// presigned-S3 GET (NO AWS SDK), crypto/sha256 for integrity, encoding/json. No
// do-core import. The open/closed boundary stays intact. The HTTP client is injected so
// the whole flow unit-tests against httptest without touching the network.
package cloudsrc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/do-awesome-ai/gitevolved/pkg/gitremote"
	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// emptyBlobSHA256 is sha256("") — the empty file's content hash. It is never
// stored in S3 (a 0-byte object is skipped on write), so it is excluded from the
// presign/download set and assembled directly as zero bytes.
const emptyBlobSHA256 = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// maxHashesPerPresignCall mirrors the server's per-call cap for /blob-urls.
const maxHashesPerPresignCall = 2000

// downloadConcurrency bounds parallel presigned-S3 GETs.
const downloadConcurrency = 16

// Client materializes a doSource cloud repo's tip tree (read) and pushes changes
// to it (write). Construct with New. The write side accumulates staged adds (via
// RecordFile) and deletes (via RecordDelete) and commits them as one bundle on
// Flush (attach → add files + add DeleteFile typed-ops → push), so it satisfies the
// gitremote push-capable backend (Source + pushSink + Flush).
type Client struct {
	base   string // e.g. https://api.doapps.cloud/v1/source
	apiKey string
	tenant string // optional; empty → server derives tenant from the bearer
	repo   string // repoId / slug
	hc     *http.Client

	staged        map[string][]byte // push side: accumulated tip tree (adds/modifies)
	stagedDeletes map[string]bool   // push side: paths to remove relative to the remote tip
}

// New returns a Client. hc may be nil (http.DefaultClient is used). base is the
// /v1/source API root; tenant is optional (pass "" to let the server derive it).
func New(base, apiKey, tenant, repo string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(base, "/"), apiKey: apiKey, tenant: tenant, repo: repo, hc: hc, staged: map[string][]byte{}, stagedDeletes: map[string]bool{}}
}

// RecordFile stages a file add/modify for the next push (the pushed tip tree is
// accumulated here, then committed by Flush). Clears any pending delete for the
// same path — a re-add cancels a delete (last-writer-wins). Part of the gitremote
// push-capable backend.
func (c *Client) RecordFile(path string, content []byte) (bool, error) {
	c.staged[path] = content
	delete(c.stagedDeletes, path)
	return true, nil
}

// RecordDelete stages a file removal relative to the remote tip. It is shipped as
// a sealed DeleteFile typed-operation envelope via /add's typedOp field (the
// add/modify path uses the snapshot path+body shape; the two are mutually exclusive
// per file). Clears any pending add for the same path — a delete cancels a pending
// add (last-writer-wins). gitremote.handleExport computes deletes as the set
// difference (remote tip ∖ pushed tree), so this fires only on an incremental push
// that removes a file; a fresh-repo push has an empty remote tip and never deletes.
func (c *Client) RecordDelete(path string) (bool, error) {
	c.stagedDeletes[path] = true
	delete(c.staged, path)
	return true, nil
}

// Flush commits the staged changes to the cloud as ONE bundle: attach a session,
// /add every staged file (snapshot) AND every staged deletion (typedOp DeleteFile
// envelope), then /push. No-op if nothing was staged.
func (c *Client) Flush() error {
	if len(c.staged) == 0 && len(c.stagedDeletes) == 0 {
		return nil
	}
	session, err := c.attach()
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(c.staged))
	for p := range c.staged {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := c.add(session, p, c.staged[p]); err != nil {
			return err
		}
	}
	dels := make([]string, 0, len(c.stagedDeletes))
	for p := range c.stagedDeletes {
		dels = append(dels, p)
	}
	sort.Strings(dels)
	for _, p := range dels {
		if err := c.addDelete(session, p); err != nil {
			return err
		}
	}
	return c.push(session)
}

// attach creates/attaches a session for the repo and returns its id. The cloud
// /attach verb requires an X-Idempotency-Key (its claim-before-act fence); we
// derive it deterministically from the repo + the staged tip-tree fingerprint so
// a network retry of the same push reuses the session, while two genuinely
// different pushes get distinct keys. TenantID is NOT sent — the auth middleware
// rewrites X-Tenant-Id from the bearer, so the OSS client never needs to know it.
func (c *Client) attach() (string, error) {
	hdrs := map[string]string{"X-Idempotency-Key": "gitevolved-attach-" + c.repo + "-" + c.stagedFingerprint()}
	var resp struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.callWithHeaders(http.MethodPost, "/attach", map[string]any{"projectId": c.repo}, hdrs, &resp); err != nil {
		return "", fmt.Errorf("cloudsrc: attach: %w", err)
	}
	if resp.SessionID == "" {
		return "", fmt.Errorf("cloudsrc: attach returned no sessionId")
	}
	return resp.SessionID, nil
}

// stagedFingerprint is a stable sha256 hex over the sorted staged changes
// (path + content hash per added file, plus each deleted path). Identical staged
// changes → identical fingerprint (idempotent retry); any change → a fresh
// fingerprint.
func (c *Client) stagedFingerprint() string {
	paths := make([]string, 0, len(c.staged))
	for p := range c.staged {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		sum := sha256.Sum256(c.staged[p])
		fmt.Fprintf(h, "A %s\x00%x\n", p, sum)
	}
	dels := make([]string, 0, len(c.stagedDeletes))
	for p := range c.stagedDeletes {
		dels = append(dels, p)
	}
	sort.Strings(dels)
	for _, p := range dels {
		fmt.Fprintf(h, "D %s\n", p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// add stages one file (body JSON-base64-encoded by the []byte marshal).
func (c *Client) add(session, path string, content []byte) error {
	sum := sha256.Sum256(content)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	hdrs := map[string]string{
		"X-Session-Id":      session,
		"X-Idempotency-Key": "gitevolved-add-" + session + "-" + hash,
	}
	body := map[string]any{"path": path, "contentHash": hash, "fromTool": "git-remote-dosource", "body": content}
	if err := c.callWithHeaders(http.MethodPost, "/add", body, hdrs, nil); err != nil {
		return fmt.Errorf("cloudsrc: add %s: %w", path, err)
	}
	return nil
}

// addDelete stages a deletion as a sealed DeleteFile typed-operation envelope via
// /add's typedOp field (mutually exclusive with the snapshot path+body shape). The
// envelope is built from the OSS operation package — the SAME type the server
// decodes, so the wire JSON matches by construction. ClaimedContentHash is empty:
// a deletion carries no post-edit content.
func (c *Client) addDelete(session, path string) error {
	var env operation.Envelope
	if err := env.Seal(&operation.DeleteFile{Path: path}); err != nil {
		return fmt.Errorf("cloudsrc: seal delete %s: %w", path, err)
	}
	hdrs := map[string]string{
		"X-Session-Id":      session,
		"X-Idempotency-Key": "gitevolved-del-" + session + "-" + env.OpID,
	}
	body := map[string]any{"typedOp": env, "fromTool": "git-remote-dosource"}
	if err := c.callWithHeaders(http.MethodPost, "/add", body, hdrs, nil); err != nil {
		return fmt.Errorf("cloudsrc: add-delete %s: %w", path, err)
	}
	return nil
}

// push commits the staged adds as a bundle.
func (c *Client) push(session string) error {
	hdrs := map[string]string{
		"X-Session-Id":      session,
		"X-Idempotency-Key": "gitevolved-push-" + session,
	}
	if err := c.callWithHeaders(http.MethodPost, "/push", map[string]any{"intentString": "git push via git-remote-dosource"}, hdrs, nil); err != nil {
		return fmt.Errorf("cloudsrc: push: %w", err)
	}
	return nil
}

// Head reports the tip commit metadata and whether the mainline has any commit.
func (c *Client) Head() (gitremote.CommitInfo, bool, error) {
	sha, err := c.tip()
	if err != nil {
		return gitremote.CommitInfo{}, false, err
	}
	if sha == "" {
		return gitremote.CommitInfo{}, false, nil
	}
	return gitremote.CommitInfo{
		Committer: "doSource <noreply@gitevolved.ai>",
		TZ:        "+0000",
		Subject:   "doSource clone (" + c.repo + " @ " + sha + ")",
	}, true, nil
}

// Tree materializes the full tip tree: repo-relative path → content. Fail-closed —
// a missing presigned URL or a hash-mismatched blob is an error, never a silently
// incomplete tree (which would be data loss for a clone).
func (c *Client) Tree() (map[string][]byte, error) {
	sha, err := c.tip()
	if err != nil {
		return nil, err
	}
	if sha == "" {
		return map[string][]byte{}, nil
	}
	entries, err := c.manifest(sha)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return map[string][]byte{}, nil
	}

	// Unique non-empty content hashes — a blob shared by many paths is fetched once.
	uniq := map[string]struct{}{}
	for _, e := range entries {
		if e.hash != emptyBlobSHA256 {
			uniq[e.hash] = struct{}{}
		}
	}
	hashes := make([]string, 0, len(uniq))
	for h := range uniq {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)

	urlByHash, err := c.presign(hashes)
	if err != nil {
		return nil, err
	}
	for _, h := range hashes {
		if urlByHash[h] == "" {
			return nil, fmt.Errorf("cloudsrc: no presigned URL minted for %s", h)
		}
	}

	blobByHash, err := c.downloadAll(urlByHash)
	if err != nil {
		return nil, err
	}

	tree := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.hash == emptyBlobSHA256 {
			tree[e.path] = []byte{}
			continue
		}
		b, ok := blobByHash[e.hash]
		if !ok {
			return nil, fmt.Errorf("cloudsrc: blob missing for %s (%s)", e.path, e.hash)
		}
		tree[e.path] = b
	}
	return tree, nil
}

// tip resolves the mainline tip sha ("" if the mainline is empty).
func (c *Client) tip() (string, error) {
	var resp struct {
		Commits []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
	}
	if err := c.getJSON("/commits?repoId="+c.repo+"&branch=main&limit=1", &resp); err != nil {
		return "", fmt.Errorf("cloudsrc: resolve tip: %w", err)
	}
	if len(resp.Commits) == 0 {
		return "", nil
	}
	return resp.Commits[0].SHA, nil
}

type manifestEntry struct {
	path string
	hash string // "sha256:<hex>"
}

// manifest returns the path→contentHash manifest at sha (no blob bodies).
func (c *Client) manifest(sha string) ([]manifestEntry, error) {
	var resp struct {
		Files []struct {
			Path        string `json:"path"`
			ContentHash string `json:"contentHash"`
		} `json:"files"`
	}
	if err := c.getJSON("/commit-detail?repoId="+c.repo+"&sha="+sha+"&bodies=false", &resp); err != nil {
		return nil, fmt.Errorf("cloudsrc: manifest at %s: %w", sha, err)
	}
	out := make([]manifestEntry, 0, len(resp.Files))
	for i, f := range resp.Files {
		h := f.ContentHash
		if !strings.HasPrefix(h, "sha256:") {
			h = "sha256:" + h
		}
		// Fail closed on a malformed entry — a silently-dropped file is a
		// quietly-incomplete clone.
		if f.Path == "" || h == "sha256:" {
			return nil, fmt.Errorf("cloudsrc: manifest file[%d] has empty path or hash at %s", i, sha)
		}
		out = append(out, manifestEntry{path: f.Path, hash: h})
	}
	return out, nil
}

// presign mints presigned-GET URLs for the given content hashes, chunked under
// the server's per-call cap.
func (c *Client) presign(hashes []string) (map[string]string, error) {
	urlByHash := make(map[string]string, len(hashes))
	for start := 0; start < len(hashes); start += maxHashesPerPresignCall {
		end := start + maxHashesPerPresignCall
		if end > len(hashes) {
			end = len(hashes)
		}
		var resp struct {
			URLs map[string]string `json:"urls"`
		}
		if err := c.postJSON("/blob-urls", map[string]any{"hashes": hashes[start:end]}, &resp); err != nil {
			return nil, fmt.Errorf("cloudsrc: mint blob-urls: %w", err)
		}
		for h, u := range resp.URLs {
			urlByHash[h] = u
		}
	}
	return urlByHash, nil
}

// downloadAll fetches every presigned URL with bounded concurrency, verifying each
// blob's content against its hash.
func (c *Client) downloadAll(urlByHash map[string]string) (map[string][]byte, error) {
	type result struct {
		hash string
		body []byte
		err  error
	}
	sem := make(chan struct{}, downloadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := make(map[string][]byte, len(urlByHash))
	var firstErr error

	for h, u := range urlByHash {
		wg.Add(1)
		sem <- struct{}{}
		go func(hash, url string) {
			defer wg.Done()
			defer func() { <-sem }()
			body, err := c.download(url)
			if err == nil {
				sum := sha256.Sum256(body)
				if got := "sha256:" + hex.EncodeToString(sum[:]); got != hash {
					err = fmt.Errorf("cloudsrc: blob integrity mismatch: %s hashed to %s", hash, got)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out[hash] = body
		}(h, u)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (c *Client) download(url string) ([]byte, error) {
	resp, err := c.hc.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("S3 GET status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// getJSON / postJSON are the authenticated API helpers. They set the Bearer (and
// optional tenant) and decode a JSON response, surfacing a non-2xx as an error.
func (c *Client) getJSON(path string, out any) error {
	return c.call(http.MethodGet, path, nil, out)
}

func (c *Client) postJSON(path string, body, out any) error {
	return c.call(http.MethodPost, path, body, out)
}

func (c *Client) call(method, path string, body, out any) error {
	return c.callWithHeaders(method, path, body, nil, out)
}

func (c *Client) callWithHeaders(method, path string, body any, extra map[string]string, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.tenant != "" {
		req.Header.Set("X-Tenant-Id", c.tenant)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}
