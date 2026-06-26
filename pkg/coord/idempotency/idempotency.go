// Package idempotency provides a durable, cross-instance idempotency fence for
// doSource write verbs.
//
// # Why this exists
//
// Before this package, idempotency was an in-process map on the api.Service
// (idemCache). Two problems made it unable to fence concurrent duplicates
// (DSQS scored attach/merge concurrency=1 because of it):
//
//   - It was CACHE-AFTER: handlers checked the cache at the top and wrote it at
//     the end. Two concurrent callers both miss the empty cache, both execute,
//     both write — no dedup at all.
//   - It was per-Lambda-instance memory: N warm instances shared nothing, so the
//     same idempotency key fanned out to N executions across the fleet.
//
// # The model: claim-before-act
//
// Claim atomically reserves (tenant, key) via a single DDB conditional write
// BEFORE the operation runs. Exactly one caller wins; it executes and then calls
// Complete to record the response. Losers either get the winner's stored
// response (true idempotent replay) or a retryable pending signal — never a
// silent duplicate execution.
//
// The claim record carries a bodyHash so a key reused with a DIFFERENT request
// payload is rejected (ErrKeyConflict) rather than returning a wrong cached
// response. A crashed winner does not wedge the key forever: the claim has an
// expiresAt and Claim's condition allows re-claiming an expired record.
//
// Pure S3+DDB+Lambda: the DDB impl is one conditional PutItem + (on conflict)
// one GetItem, on the existing single multi-tenant table. No new substrate, no
// new auth primitive.
//
// This is the OPEN half of the doSource idempotency seam — the open in-memory
// implementation (the closed platform provides the durable one). The MemoryStore here is
// the FREE, offline, single-machine referee that runs in the local gitevolved
// daemon. The closed platform ships the DDB-backed DDBStore — the PAID,
// cross-machine, CAS-fenced cloud referee — which imports this package for the
// shared contract (the Store interface + the error/TTL constants) and satisfies
// it. Both back-ends are proven identical by RunContract in the idempotencytest
// subpackage, run on both sides of the module boundary so they cannot drift.
package idempotency

import (
	"errors"
	"sync"
	"time"
)

// Store is the claim-before-act idempotency contract. Both the open MemoryStore
// and the closed DDBStore implement it; RunContract (idempotencytest) exercises
// any implementation against the reference semantics.
type Store interface {
	// Claim atomically reserves (tenantID, key) before the operation runs.
	// Exactly one concurrent caller wins (won=true, prior=nil); a replay of a
	// completed claim returns won=false with the stored prior response; a
	// concurrent in-flight claim returns ErrPending; a reuse of the key with a
	// different bodyHash returns ErrKeyConflict.
	Claim(tenantID, key, bodyHash string) (won bool, prior []byte, err error)
	// Complete records the winner's response so replays return it.
	Complete(tenantID, key string, response []byte) error
	// Release frees a won claim WITHOUT recording a response, so a retryable
	// execution failure re-executes on the next attempt instead of waiting out
	// the pending-claim TTL.
	Release(tenantID, key string) error
}

// ErrPending means a concurrent request holds an unexpired claim on (tenant,
// key) but has not Completed yet. The caller should surface a retryable
// conflict (HTTP 409) — the client retries and, once the winner Completes, gets
// the stored response.
var ErrPending = errors.New("dosource/idempotency: claim pending")

// ErrKeyConflict means (tenant, key) was already claimed with a DIFFERENT
// bodyHash — the idempotency key was reused for a different request. The caller
// should surface an invalid-request error (HTTP 400). Returning the prior
// response here would be a wrong-result-for-a-different-input bug.
var ErrKeyConflict = errors.New("dosource/idempotency: key reused with a different request body")

// DefaultTTL is how long a claim record lives before it may be re-claimed (and
// is eligible for DDB TTL cleanup). Matches the prior in-process idempotencyTTL.
const DefaultTTL = 10 * time.Minute

// record is the stored claim. Shared shape across both backends.
type record struct {
	bodyHash  string
	done      bool
	response  []byte
	expiresAt time.Time
}

// MemoryStore is the in-process implementation (dev + tests). It is the
// reference semantics the DDBStore must match.
type MemoryStore struct {
	mu  sync.Mutex
	now func() time.Time
	ttl time.Duration
	rec map[string]*record // key: tenantID + "\x00" + key
}

// NewMemoryStore returns a MemoryStore with the real clock and DefaultTTL.
func NewMemoryStore() *MemoryStore { return NewMemoryStoreWithClock(time.Now) }

// NewMemoryStoreWithClock lets tests control time (for expiry).
func NewMemoryStoreWithClock(now func() time.Time) *MemoryStore {
	return &MemoryStore{now: now, ttl: DefaultTTL, rec: make(map[string]*record)}
}

func memKey(tenantID, key string) string { return tenantID + "\x00" + key }

// Claim implements the api.IdempotencyStore contract; see the package doc.
func (m *MemoryStore) Claim(tenantID, key, bodyHash string) (bool, []byte, error) {
	if tenantID == "" || key == "" {
		return false, nil, errors.New("dosource/idempotency: Claim: empty tenantID or key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(tenantID, key)
	now := m.now()
	r, ok := m.rec[k]
	if !ok || now.After(r.expiresAt) {
		// Free, or the prior claim expired (crashed winner) — (re)claim it.
		m.rec[k] = &record{bodyHash: bodyHash, expiresAt: now.Add(m.ttl)}
		return true, nil, nil
	}
	if r.bodyHash != bodyHash {
		return false, nil, ErrKeyConflict
	}
	if r.done {
		return false, r.response, nil
	}
	return false, nil, ErrPending
}

// Complete implements the api.IdempotencyStore contract; see the package doc.
func (m *MemoryStore) Complete(tenantID, key string, response []byte) error {
	if tenantID == "" || key == "" {
		return errors.New("dosource/idempotency: Complete: empty tenantID or key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(tenantID, key)
	r, ok := m.rec[k]
	if !ok {
		// Winner's claim expired before Complete (very long op). Recreate as
		// done so a subsequent replay still returns the response.
		r = &record{expiresAt: m.now().Add(m.ttl)}
		m.rec[k] = r
	}
	r.done = true
	r.response = append([]byte(nil), response...)
	r.expiresAt = m.now().Add(m.ttl)
	return nil
}

// Release deletes the claim for (tenantID, key) so the next attempt re-claims
// immediately. Used on a retryable execution failure (don't make a transient
// failure wait out the pending-claim TTL).
func (m *MemoryStore) Release(tenantID, key string) error {
	if tenantID == "" || key == "" {
		return errors.New("dosource/idempotency: Release: empty tenantID or key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rec, memKey(tenantID, key))
	return nil
}
