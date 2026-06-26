// Package idempotencytest is the shared conformance harness for the
// idempotency.Store contract.
//
// # Why this exists
//
// The doSource idempotency seam splits across the module boundary: the open
// MemoryStore
// lives in the gitevolved module; the closed DDBStore lives in the platform.
// The whole point of the original shared test was that both back-ends produce
// IDENTICAL claim-before-act behavior. Exporting the harness (the stdlib
// testing/fstest + net/http/httptest pattern) keeps that guarantee STRUCTURAL:
// both sides import and run RunContract, so a behavioral drift between the free
// local referee and the paid cloud referee fails a test rather than slipping
// through. Duplicating the harness on each side would re-open exactly the
// seam-drift this test exists to prevent.
//
// It lives in its own subpackage so the `testing` import never enters the main
// idempotency package's import graph.
package idempotencytest

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/coord/idempotency"
)

// RunContract exercises an idempotency.Store implementation against the reference
// claim-before-act semantics. makeStore builds a fresh store bound to the given
// clock (so expiry is deterministic). Both the open MemoryStore and the closed
// DDBStore must pass.
func RunContract(t *testing.T, makeStore func(now func() time.Time) idempotency.Store) {
	t.Helper()
	fixed := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return fixed }

	t.Run("first_claim_wins_then_completes_replay_returns_response", func(t *testing.T) {
		s := makeStore(clock)
		won, prior, err := s.Claim("t", "k", "bh1")
		if err != nil || !won || prior != nil {
			t.Fatalf("first claim: won=%v prior=%v err=%v, want won=true", won, prior, err)
		}
		if err := s.Complete("t", "k", []byte(`{"resp":1}`)); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		won, prior, err = s.Claim("t", "k", "bh1")
		if err != nil {
			t.Fatalf("replay claim err: %v", err)
		}
		if won {
			t.Fatalf("replay must NOT win")
		}
		if string(prior) != `{"resp":1}` {
			t.Fatalf("replay prior = %q, want stored response", prior)
		}
	})

	t.Run("pending_claim_returns_ErrPending", func(t *testing.T) {
		s := makeStore(clock)
		if won, _, err := s.Claim("t", "k", "bh1"); !won || err != nil {
			t.Fatalf("first claim: won=%v err=%v", won, err)
		}
		// Second claim before Complete — winner is still in-flight.
		won, _, err := s.Claim("t", "k", "bh1")
		if won {
			t.Fatalf("second claim must not win while pending")
		}
		if !errors.Is(err, idempotency.ErrPending) {
			t.Fatalf("expected ErrPending, got %v", err)
		}
	})

	t.Run("same_key_different_body_returns_ErrKeyConflict", func(t *testing.T) {
		s := makeStore(clock)
		if won, _, err := s.Claim("t", "k", "bh1"); !won || err != nil {
			t.Fatalf("first claim: won=%v err=%v", won, err)
		}
		won, _, err := s.Claim("t", "k", "DIFFERENT")
		if won {
			t.Fatalf("claim with a different body must not win")
		}
		if !errors.Is(err, idempotency.ErrKeyConflict) {
			t.Fatalf("expected ErrKeyConflict, got %v", err)
		}
	})

	t.Run("expired_pending_is_reclaimable", func(t *testing.T) {
		now := fixed
		movingClock := func() time.Time { return now }
		s := makeStore(movingClock)
		if won, _, err := s.Claim("t", "k", "bh1"); !won || err != nil {
			t.Fatalf("first claim: won=%v err=%v", won, err)
		}
		// Advance past the TTL — the prior (uncompleted) claim is now stale.
		now = fixed.Add(idempotency.DefaultTTL + time.Minute)
		won, _, err := s.Claim("t", "k", "bh2")
		if !won {
			t.Fatalf("expired claim must be re-claimable, got won=false err=%v", err)
		}
	})

	t.Run("release_frees_the_key_for_reclaim", func(t *testing.T) {
		s := makeStore(clock)
		if won, _, err := s.Claim("t", "k", "bh1"); !won || err != nil {
			t.Fatalf("first claim: won=%v err=%v", won, err)
		}
		// Without Release a second claim would be ErrPending; after Release the
		// key is free, so a fresh claim wins (the retry-after-failure path).
		if err := s.Release("t", "k"); err != nil {
			t.Fatalf("Release: %v", err)
		}
		won, _, err := s.Claim("t", "k", "bh1")
		if !won || err != nil {
			t.Fatalf("after Release the key must be re-claimable, got won=%v err=%v", won, err)
		}
	})

	t.Run("concurrent_claims_exactly_one_winner", func(t *testing.T) {
		s := makeStore(clock)
		const n = 30
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		pendings := 0
		wg.Add(n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				<-start
				won, _, err := s.Claim("t", "race", "bh")
				mu.Lock()
				switch {
				case won:
					wins++
				case errors.Is(err, idempotency.ErrPending):
					pendings++
				default:
					t.Errorf("unexpected claim outcome: won=%v err=%v", won, err)
				}
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 {
			t.Fatalf("expected exactly 1 winner, got %d (pendings=%d)", wins, pendings)
		}
		if pendings != n-1 {
			t.Fatalf("expected %d pendings, got %d", n-1, pendings)
		}
	})
}
