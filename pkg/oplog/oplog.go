// Package oplog is the local, append-only log of sealed typed operations — the
// durable persistence layer of the gitevolved local client.
//
// # Why this exists
//
// doSource is event-sourced: an edit is a typed operation (AddFunction,
// RewriteRegion, …), not a text diff. On the laptop, the pipeline is
//
//	edit → extract.Extract → operation.Envelope.Seal → oplog.Append
//	                                                       │
//	     export ← projector.Project ← oplog.Operations ←──┘
//
// This package is the append-only log in the middle: the extractor's sealed
// operations land here, and the projector reads them back to materialize files
// on demand. It is the local equivalent of the cloud staging/storage layer, but
// pure — file-backed JSONL, no DDB, no S3, no cloud. Offline edits simply extend
// the log; reconnecting replays the suffix through the cloud merge queue.
//
// # Format
//
// One sealed operation.Envelope per line, JSON-encoded (JSONL). Append order is
// preserved; causal ordering across sessions is the projector/merge-queue's job
// (via Envelope.CausalParents), not this log's. Each Append is mutex-guarded and
// fsync'd so a crash can lose at most the in-flight write, never an acknowledged
// one.
//
// # Fail-loud reads
//
// A torn trailing line (process killed mid-Append) is reported as an error naming
// the byte offset — NEVER silently dropped. For a source-control log, silently
// returning a prefix of the operations is data loss that surfaces as a corrupt
// projection much later; failing loudly at read time is the safe contract.
//
// This is an OPEN component of the free local client. It depends only on
// pkg/operation + the Go standard library.
package oplog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// ErrUnsealed is returned by Append when the envelope has no OpID — callers must
// Seal before appending, or the log would hold un-content-addressed entries.
var ErrUnsealed = errors.New("oplog: envelope is not sealed (empty OpID) — call Envelope.Seal before Append")

// Log is an append-only log of sealed operation envelopes backed by a JSONL file.
// All methods are safe for concurrent use.
type Log struct {
	mu   sync.Mutex
	path string
}

// Open returns a Log backed by the JSONL file at path. The file is created if it
// does not exist; an existing file is preserved (appends extend it). The parent
// directory must already exist.
func Open(path string) (*Log, error) {
	if path == "" {
		return nil, errors.New("oplog: Open: empty path")
	}
	// Touch the file so a fresh log is readable (empty) immediately, and so Open
	// fails loudly now if the path is unwritable rather than at first Append.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("oplog: Open %s: %w", path, err)
	}
	if cerr := f.Close(); cerr != nil {
		return nil, fmt.Errorf("oplog: Open %s: %w", path, cerr)
	}
	return &Log{path: path}, nil
}

// Append writes one sealed envelope as a JSONL line and fsyncs. It rejects an
// unsealed envelope (empty OpID). Concurrent Appends are serialized; each
// returns only after the bytes are durably on disk.
func (l *Log) Append(env *operation.Envelope) error {
	if env == nil {
		return errors.New("oplog: Append: nil envelope")
	}
	if env.OpID == "" {
		return ErrUnsealed
	}
	line, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("oplog: Append: marshal: %w", err)
	}
	if i := indexOfNewline(line); i >= 0 {
		// json.Marshal of an Envelope never emits a raw newline (compact form),
		// so this is a defensive invariant check, not an expected path.
		return fmt.Errorf("oplog: Append: marshaled envelope contains a newline at %d — would corrupt JSONL framing", i)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("oplog: Append: open: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("oplog: Append: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("oplog: Append: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("oplog: Append: close: %w", err)
	}
	return nil
}

// Envelopes reads every sealed envelope in append order. A torn trailing record
// (crash mid-Append) is returned as an error naming the byte offset — the
// already-parsed prefix is NOT returned, so callers never mistake a truncated
// log for a complete one. json.Decoder streams consecutive JSON values
// (newline-framed JSONL is whitespace-separated to it), so arbitrarily large
// AddFile bodies are handled without a line-length cap.
func (l *Log) Envelopes() ([]*operation.Envelope, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		return nil, fmt.Errorf("oplog: Envelopes: open: %w", err)
	}
	defer f.Close()

	var out []*operation.Envelope
	dec := json.NewDecoder(f)
	for {
		var env operation.Envelope
		derr := dec.Decode(&env)
		if derr == io.EOF {
			// Clean end on a record boundary — the log is intact.
			return out, nil
		}
		if derr != nil {
			// io.ErrUnexpectedEOF here means the final record was half-written
			// (process killed mid-Append). InputOffset points at the start of the
			// torn record. Fail loud — never return the prefix as if complete.
			return nil, fmt.Errorf("oplog: Envelopes: malformed/torn entry at byte offset %d: %w", dec.InputOffset(), derr)
		}
		out = append(out, &env)
	}
}

// Operations reads every envelope and decodes it to its concrete Operation, in
// append order. Convenience for feeding projector.Project.
func (l *Log) Operations() ([]operation.Operation, error) {
	envs, err := l.Envelopes()
	if err != nil {
		return nil, err
	}
	ops := make([]operation.Operation, 0, len(envs))
	for i, env := range envs {
		op, derr := env.Decode()
		if derr != nil {
			return nil, fmt.Errorf("oplog: Operations: decode entry %d (op_id %s): %w", i, env.OpID, derr)
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// Len returns the number of envelopes in the log. It reads the whole log; for a
// hot path prefer caching the count from Append.
func (l *Log) Len() (int, error) {
	envs, err := l.Envelopes()
	if err != nil {
		return 0, err
	}
	return len(envs), nil
}

func indexOfNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}
