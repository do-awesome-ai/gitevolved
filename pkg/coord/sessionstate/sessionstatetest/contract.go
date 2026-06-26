// Package sessionstatetest is the shared conformance harness for the
// sessionstate.Store contract.
//
// # Why this exists
//
// The doSource session-state seam splits across the module boundary: the open
// MemoryStore
// lives in the gitevolved module; the closed DDB store lives in the platform.
// Both must produce IDENTICAL behavior. Exporting the harness (the stdlib
// testing/fstest + net/http/httptest pattern) keeps that guarantee STRUCTURAL:
// the open MemoryStore runs RunContract in the gitevolved module's test; the
// closed DDB store runs the SAME RunContract platform-side. A behavioral drift
// between the free local store and the paid cloud store fails a test rather than
// slipping through. It lives in its own subpackage so the `testing` import never
// enters the main sessionstate package's import graph.
package sessionstatetest

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/coord/sessionstate"
)

// NewTestState builds a baseline editing-state record for contract + store tests.
func NewTestState(tenantID, repoID, sessionID string) *sessionstate.State {
	return &sessionstate.State{
		TenantID:        tenantID,
		RepoID:          repoID,
		SessionID:       sessionID,
		Status:          sessionstate.StatusEditing,
		LastTurn:        0,
		LastHeartbeat:   time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		StagedFileCount: 0,
	}
}

// RunContract exercises every Store invariant. Called once per implementation —
// the open MemoryStore (gitevolved) and the closed DDB store (platform).
func RunContract(t *testing.T, makeStore func(now func() time.Time) sessionstate.Store) {
	t.Helper()

	t.Run("Put_then_Get_roundtrip", func(t *testing.T) {
		s := makeStore(time.Now)
		if err := s.Put(NewTestState("t1", "r1", "sess1")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, err := s.Get("t1", "sess1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.SessionID != "sess1" || got.Status != sessionstate.StatusEditing {
			t.Fatalf("Get returned unexpected state: %+v", got)
		}
	})

	t.Run("Get_unknown_returns_NotFound", func(t *testing.T) {
		s := makeStore(time.Now)
		_, err := s.Get("t1", "missing")
		if !errors.Is(err, sessionstate.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Get_returns_defensive_copy", func(t *testing.T) {
		s := makeStore(time.Now)
		st := NewTestState("t1", "r1", "sess1")
		st.PredictedConflicts = []sessionstate.ConflictPair{{Path: "a.go", Vs: "main"}}
		if err := s.Put(st); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, _ := s.Get("t1", "sess1")
		// Mutate the returned copy — must not affect the store.
		got.Status = sessionstate.StatusBundling
		got.PredictedConflicts[0].Path = "MUTATED"

		got2, _ := s.Get("t1", "sess1")
		if got2.Status != sessionstate.StatusEditing {
			t.Fatalf("Get-mutation leaked: status now %s", got2.Status)
		}
		if got2.PredictedConflicts[0].Path != "a.go" {
			t.Fatalf("Get-mutation leaked into PredictedConflicts: %v",
				got2.PredictedConflicts)
		}
	})

	t.Run("SetStatus_updates_only_named_fields", func(t *testing.T) {
		s := makeStore(time.Now)
		st := NewTestState("t1", "r1", "sess1")
		st.LastTurn = 14
		st.StagedFileCount = 7
		if err := s.Put(st); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := s.SetStatus("t1", "sess1", sessionstate.StatusBundling, "sha-bundle", "sha-parent"); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		got, _ := s.Get("t1", "sess1")
		if got.Status != sessionstate.StatusBundling || got.BundleHead != "sha-bundle" || got.ParentMain != "sha-parent" {
			t.Fatalf("SetStatus updated state: %+v", got)
		}
		// Untouched fields preserved.
		if got.LastTurn != 14 || got.StagedFileCount != 7 {
			t.Fatalf("SetStatus clobbered untouched fields: %+v", got)
		}
	})

	t.Run("SetStatus_unknown_session_returns_NotFound", func(t *testing.T) {
		s := makeStore(time.Now)
		err := s.SetStatus("t1", "missing", sessionstate.StatusBundling, "", "")
		if !errors.Is(err, sessionstate.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Heartbeat_updates_LastHeartbeat", func(t *testing.T) {
		fakeNow := time.Date(2026, 5, 6, 18, 0, 0, 0, time.UTC)
		s := makeStore(func() time.Time { return fakeNow })
		_ = s.Put(NewTestState("t1", "r1", "sess1"))
		if err := s.Heartbeat("t1", "sess1"); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		got, _ := s.Get("t1", "sess1")
		if !got.LastHeartbeat.Equal(fakeNow) {
			t.Fatalf("expected LastHeartbeat=%v, got %v", fakeNow, got.LastHeartbeat)
		}
	})

	t.Run("BumpTurn_sequential_is_monotonic", func(t *testing.T) {
		s := makeStore(time.Now)
		_ = s.Put(NewTestState("t1", "r1", "sess1"))
		for want := 1; want <= 3; want++ {
			got, err := s.BumpTurn("t1", "sess1")
			if err != nil {
				t.Fatalf("BumpTurn: %v", err)
			}
			if got != want {
				t.Fatalf("BumpTurn returned %d, want %d", got, want)
			}
		}
		row, _ := s.Get("t1", "sess1")
		if row.LastTurn != 3 {
			t.Fatalf("persisted LastTurn=%d, want 3", row.LastTurn)
		}
	})

	t.Run("BumpTurn_unknown_returns_NotFound", func(t *testing.T) {
		s := makeStore(time.Now)
		if _, err := s.BumpTurn("t1", "nope"); !errors.Is(err, sessionstate.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("BumpTurn_concurrent_yields_distinct_values", func(t *testing.T) {
		s := makeStore(time.Now)
		_ = s.Put(NewTestState("t1", "r1", "sess1"))
		const n = 50
		var wg sync.WaitGroup
		var mu sync.Mutex
		seen := make(map[int]int, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				got, err := s.BumpTurn("t1", "sess1")
				if err != nil {
					t.Errorf("BumpTurn: %v", err)
					return
				}
				mu.Lock()
				seen[got]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		if len(seen) != n {
			t.Fatalf("expected %d distinct turn values, got %d (a duplicate means the increment was not atomic)", n, len(seen))
		}
		for turn := 1; turn <= n; turn++ {
			if seen[turn] != 1 {
				t.Fatalf("turn %d appeared %d times, want exactly 1 (values must be exactly {1..%d})", turn, seen[turn], n)
			}
		}
	})

	t.Run("SetShadowSummary_updates_denormalized_fields", func(t *testing.T) {
		s := makeStore(time.Now)
		_ = s.Put(NewTestState("t1", "r1", "sess1"))
		conflicts := []sessionstate.ConflictPair{
			{Path: "cmd/api/main.go", Vs: "main"},
			{Path: "CLAUDE.md", Vs: "session/auto-x"},
		}
		if err := s.SetShadowSummary("t1", "sess1", 5, conflicts); err != nil {
			t.Fatalf("SetShadowSummary: %v", err)
		}
		got, _ := s.Get("t1", "sess1")
		if got.StagedFileCount != 5 {
			t.Fatalf("StagedFileCount=%d, want 5", got.StagedFileCount)
		}
		if len(got.PredictedConflicts) != 2 || got.PredictedConflicts[0].Path != "cmd/api/main.go" {
			t.Fatalf("PredictedConflicts wrong: %+v", got.PredictedConflicts)
		}
	})

	t.Run("SetShadowSummary_rejects_negative_count", func(t *testing.T) {
		s := makeStore(time.Now)
		_ = s.Put(NewTestState("t1", "r1", "sess1"))
		err := s.SetShadowSummary("t1", "sess1", -1, nil)
		if err == nil {
			t.Fatalf("expected error for negative stagedFileCount")
		}
	})

	t.Run("ListByRepo_filters_by_tenant_and_repo", func(t *testing.T) {
		s := makeStore(time.Now)
		// Two sessions on the same repo.
		_ = s.Put(NewTestState("t1", "r1", "sess1"))
		_ = s.Put(NewTestState("t1", "r1", "sess2"))
		// One session on a different repo, same tenant.
		_ = s.Put(NewTestState("t1", "r2", "sess3"))
		// One session on the same-named repo, DIFFERENT tenant.
		_ = s.Put(NewTestState("t2", "r1", "sess4"))

		list, err := s.ListByRepo("t1", "r1")
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("expected 2 sessions on t1/r1, got %d (%+v)", len(list), list)
		}
		seen := map[string]bool{}
		for _, s := range list {
			seen[s.SessionID] = true
		}
		if !seen["sess1"] || !seen["sess2"] {
			t.Fatalf("expected sess1+sess2 in list, got %+v", seen)
		}
	})

	t.Run("ListStale_returns_only_old_active_sessions", func(t *testing.T) {
		s := makeStore(time.Now)
		// One session with a fresh heartbeat, one stale.
		fresh := NewTestState("t1", "r1", "fresh")
		fresh.LastHeartbeat = time.Date(2026, 5, 6, 18, 0, 0, 0, time.UTC)
		stale := NewTestState("t1", "r1", "stale")
		stale.LastHeartbeat = time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
		// One already-marked-stale (must not appear again — idempotent reaping).
		alreadyStale := NewTestState("t1", "r1", "already")
		alreadyStale.LastHeartbeat = time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
		alreadyStale.Status = sessionstate.StatusStale

		_ = s.Put(fresh)
		_ = s.Put(stale)
		_ = s.Put(alreadyStale)

		cutoff := time.Date(2026, 5, 6, 17, 0, 0, 0, time.UTC)
		list, err := s.ListStale(cutoff)
		if err != nil {
			t.Fatalf("ListStale: %v", err)
		}
		if len(list) != 1 || list[0].SessionID != "stale" {
			t.Fatalf("expected only 'stale' in list, got %+v", list)
		}
	})

	t.Run("input_validation", func(t *testing.T) {
		s := makeStore(time.Now)
		if err := s.Put(nil); err == nil {
			t.Fatalf("Put(nil) should error")
		}
		if _, err := s.Get("", "sess"); err == nil {
			t.Fatalf("Get with empty tenantID should error")
		}
		if err := s.Heartbeat("t", ""); err == nil {
			t.Fatalf("Heartbeat with empty sessionID should error")
		}
		if _, err := s.ListByRepo("", "r"); err == nil {
			t.Fatalf("ListByRepo with empty tenantID should error")
		}
	})
}
