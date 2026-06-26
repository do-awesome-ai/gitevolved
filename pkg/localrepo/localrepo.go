// Package localrepo is the composition root of the gitevolved local client: it
// wires the four open primitives — extract, oplog, projector, export — into the
// laptop-usable engine that the local daemon and the git-remote-dosource helper
// drive.
//
// # Why this exists
//
// The open primitives each do one thing (turn an edit into a typed op; persist
// ops; project ops into files; export a projection to stock git). localrepo is
// the thin seam that composes them into the three operations a local doSource
// client actually performs:
//
//	RecordEdit   an editor/agent changed a file       → append a typed op
//	Materialize  what does the working tree look like? → project the op-log
//	ExportToGit  make `git log` / `git push origin`    → mint a real git commit
//	             see this work
//
// With this seam, the daemon (FSEvents watch → RecordEdit) and the remote-helper
// (`git push dosource://` → ExportToGit / Materialize) are thin shells over a
// tested engine, not bespoke pipelines.
//
// This is an OPEN component of the free local client. It
// depends only on the four open gitevolved packages + the Go standard library —
// zero cloud, runs fully offline. The cloud sync transport (closed) is layered
// ON TOP of this by the helper; localrepo itself never talks to the network.
package localrepo

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/export"
	"github.com/do-awesome-ai/gitevolved/pkg/extract"
	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/oplog"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// Repo is a local doSource repository: an append-only op-log plus the monotonic
// turn counter and session identity used to stamp emitted operations. Safe for
// concurrent use.
type Repo struct {
	mu      sync.Mutex
	log     *oplog.Log
	session string
	turn    int
	clock   func() time.Time
}

// Open returns a Repo backed by the op-log JSONL file at oplogPath. session is
// the identity stamped on every emitted operation (Envelope.SourceSession). The
// turn counter starts after the highest turn already in the log, so re-opening
// an existing repo continues its turn sequence monotonically.
func Open(oplogPath, session string) (*Repo, error) {
	if session == "" {
		return nil, errors.New("localrepo: Open: empty session")
	}
	log, err := oplog.Open(oplogPath)
	if err != nil {
		return nil, err
	}
	r := &Repo{log: log, session: session, clock: time.Now}
	// Resume the turn sequence past whatever is already logged.
	envs, err := log.Envelopes()
	if err != nil {
		return nil, fmt.Errorf("localrepo: Open: read existing log: %w", err)
	}
	for _, env := range envs {
		if env.SourceTurn > r.turn {
			r.turn = env.SourceTurn
		}
	}
	return r, nil
}

// SetClock overrides the wall clock (tests use a fixed clock for deterministic
// export SHAs/dates). Not safe to call concurrently with other methods.
func (r *Repo) SetClock(now func() time.Time) { r.clock = now }

// RecordEdit turns a pre/post file-content pair into a typed operation and
// appends it to the op-log. pre==nil means the file is new (AddFile); post==nil
// means it was deleted (DeleteFile); otherwise a minimal RewriteRegion.
//
// It returns recorded=false (and a nil error) when pre and post are byte-equal —
// a no-op save, which the daemon produces constantly and must not log. Any other
// extraction or append failure is returned.
func (r *Repo) RecordEdit(path string, pre, post []byte) (recorded bool, err error) {
	op, err := extract.Extract(path, pre, post)
	if err != nil {
		if errors.Is(err, extract.ErrNoChange) {
			return false, nil
		}
		return false, fmt.Errorf("localrepo: RecordEdit %s: %w", path, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.turn++
	env := &operation.Envelope{
		SourceSession: r.session,
		SourceTurn:    r.turn,
		EmittedAt:     r.clock().UTC(),
	}
	if err := env.Seal(op); err != nil {
		r.turn-- // rolled back: nothing was logged for this turn
		return false, fmt.Errorf("localrepo: RecordEdit %s: seal: %w", path, err)
	}
	if err := r.log.Append(env); err != nil {
		r.turn--
		return false, fmt.Errorf("localrepo: RecordEdit %s: append: %w", path, err)
	}
	return true, nil
}

// RecordFile records the on-disk content of path against the op-log's CURRENT
// projection of that path — i.e. it derives the pre-state from the operations
// themselves (Materialize), so callers that only have the new content (a CLI
// `record <file>`, or a watcher that just saw a file change) don't have to track
// the previous bytes. A new path projects to nil → AddFile; unchanged content is
// a no-op (recorded=false). It re-materializes per call, which is fine for
// single-shot CLI use; a high-frequency watcher should cache the projection or
// use RecordEdit with its own snapshot.
func (r *Repo) RecordFile(path string, content []byte) (recorded bool, err error) {
	state, err := r.Materialize()
	if err != nil {
		return false, err
	}
	return r.RecordEdit(path, state[path], content)
}

// RecordDelete records the deletion of path. The pre-state is taken from the
// current projection; if the path isn't present (nothing to delete) it is a
// no-op (recorded=false).
func (r *Repo) RecordDelete(path string) (recorded bool, err error) {
	state, err := r.Materialize()
	if err != nil {
		return false, err
	}
	pre, ok := state[path]
	if !ok {
		return false, nil
	}
	return r.RecordEdit(path, pre, nil)
}

// Materialize projects the full op-log into the current working-tree state
// (path → content). This is what the editor preview / remote-helper `list`
// renders, computed on demand from the operations rather than stored.
func (r *Repo) Materialize() (projector.State, error) {
	ops, err := r.log.Operations()
	if err != nil {
		return nil, err
	}
	state, err := projector.Project(projector.State{}, ops)
	if err != nil {
		return nil, fmt.Errorf("localrepo: Materialize: project: %w", err)
	}
	return state, nil
}

// ExportToGit projects the op-log and writes a single real git commit to the
// repository at repoPath (which must already be `git init`-ed), returning the
// commit SHA. This is the boundary that makes `git log` and `git push origin`
// work on doSource-tracked work — stock git via os/exec, no fork. subject is the
// commit-message first line.
func (r *Repo) ExportToGit(repoPath, subject string) (string, error) {
	state, err := r.Materialize()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	envs, err := r.log.Envelopes()
	now := r.clock().UTC()
	r.mu.Unlock()
	if err != nil {
		return "", err
	}
	sha, err := export.ExportCommit(repoPath, state, envs, export.CommitOptions{
		Subject:       subject,
		AuthorDate:    now,
		CommitterDate: now,
	})
	if err != nil {
		return "", fmt.Errorf("localrepo: ExportToGit: %w", err)
	}
	return sha, nil
}

// Turn returns the current (highest-emitted) turn index. Useful for the daemon's
// heartbeat/state reporting.
func (r *Repo) Turn() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turn
}
