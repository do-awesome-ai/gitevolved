// memory.go — in-memory Store implementation for sessionstate.
//
// Same pattern as platform/internal/dosource/storage/memory.go: real
// implementation behind the Store interface, parameterized by the
// runContract test helper so a future DDBStore impl drops in via the
// same contract test suite.
//
// Concurrency: single sync.RWMutex around the maps. Production DDB
// has finer-grained concurrency through partition isolation; the
// MemoryStore's coarse lock is fine for hermetic tests + dev mode.
//
// Heartbeat semantics: Heartbeat() updates LastHeartbeat to time.Now()
// — tests that need deterministic timestamps inject a clock via
// NewMemoryStoreWithClock.

package sessionstate

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotFound is returned by Get when no session has been registered
// for the given (tenantID, sessionID).
var ErrNotFound = errors.New("dosource/sessionstate: not found")

// MemoryStore is the in-memory Store. Construct via NewMemoryStore()
// for production-clock behavior, or NewMemoryStoreWithClock(now) for
// deterministic tests.
type MemoryStore struct {
	mu    sync.RWMutex
	now   func() time.Time
	rows  map[string]*State // key: tenantID + ":" + sessionID
}

// NewMemoryStore returns a fresh empty MemoryStore using time.Now.
func NewMemoryStore() *MemoryStore {
	return NewMemoryStoreWithClock(time.Now)
}

// NewMemoryStoreWithClock returns a MemoryStore whose Heartbeat()
// timestamps come from the supplied clock. Used by tests for
// determinism.
func NewMemoryStoreWithClock(now func() time.Time) *MemoryStore {
	return &MemoryStore{
		now:  now,
		rows: make(map[string]*State),
	}
}

func key(tenantID, sessionID string) string {
	return tenantID + ":" + sessionID
}

// Get returns the State for one session, or ErrNotFound.
func (m *MemoryStore) Get(tenantID, sessionID string) (*State, error) {
	if tenantID == "" || sessionID == "" {
		return nil, fmt.Errorf("dosource/sessionstate: Get: empty tenantID or sessionID")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.rows[key(tenantID, sessionID)]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	cp.PredictedConflicts = append([]ConflictPair(nil), s.PredictedConflicts...)
	return &cp, nil
}

// Put writes the full state for a session. Idempotent — the latest
// write wins.
func (m *MemoryStore) Put(s *State) error {
	if s == nil {
		return fmt.Errorf("dosource/sessionstate: Put: nil state")
	}
	if s.TenantID == "" || s.SessionID == "" {
		return fmt.Errorf("dosource/sessionstate: Put: empty tenantID or sessionID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	cp.PredictedConflicts = append([]ConflictPair(nil), s.PredictedConflicts...)
	m.rows[key(s.TenantID, s.SessionID)] = &cp
	return nil
}

// SetStatus updates only the status / bundleHead / parentMain fields.
func (m *MemoryStore) SetStatus(tenantID, sessionID string, status Status, bundleHead, parentMain string) error {
	if tenantID == "" || sessionID == "" {
		return fmt.Errorf("dosource/sessionstate: SetStatus: empty tenantID or sessionID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[key(tenantID, sessionID)]
	if !ok {
		return ErrNotFound
	}
	s.Status = status
	s.BundleHead = bundleHead
	s.ParentMain = parentMain
	return nil
}

// BumpTurn atomically increments lastTurn and returns the new value. The
// mutex makes the increment atomic for the in-memory substrate, matching the
// DDB ADD semantics so tests exercise the same contract.
func (m *MemoryStore) BumpTurn(tenantID, sessionID string) (int, error) {
	if tenantID == "" || sessionID == "" {
		return 0, fmt.Errorf("dosource/sessionstate: BumpTurn: empty tenantID or sessionID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[key(tenantID, sessionID)]
	if !ok {
		return 0, ErrNotFound
	}
	s.LastTurn++
	return s.LastTurn, nil
}

// Heartbeat updates lastHeartbeat to the configured clock's now.
func (m *MemoryStore) Heartbeat(tenantID, sessionID string) error {
	if tenantID == "" || sessionID == "" {
		return fmt.Errorf("dosource/sessionstate: Heartbeat: empty tenantID or sessionID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[key(tenantID, sessionID)]
	if !ok {
		return ErrNotFound
	}
	s.LastHeartbeat = m.now()
	return nil
}

// SetShadowSummary updates the denormalized fields.
func (m *MemoryStore) SetShadowSummary(tenantID, sessionID string, stagedFileCount int, predictedConflicts []ConflictPair) error {
	if tenantID == "" || sessionID == "" {
		return fmt.Errorf("dosource/sessionstate: SetShadowSummary: empty args")
	}
	if stagedFileCount < 0 {
		return fmt.Errorf("dosource/sessionstate: SetShadowSummary: negative stagedFileCount")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[key(tenantID, sessionID)]
	if !ok {
		return ErrNotFound
	}
	s.StagedFileCount = stagedFileCount
	s.PredictedConflicts = append([]ConflictPair(nil), predictedConflicts...)
	return nil
}

// ListByRepo returns all sessions on one repo. Order is undefined.
func (m *MemoryStore) ListByRepo(tenantID, repoID string) ([]*State, error) {
	if tenantID == "" || repoID == "" {
		return nil, fmt.Errorf("dosource/sessionstate: ListByRepo: empty args")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*State, 0)
	for _, s := range m.rows {
		if s.TenantID == tenantID && s.RepoID == repoID {
			cp := *s
			cp.PredictedConflicts = append([]ConflictPair(nil), s.PredictedConflicts...)
			out = append(out, &cp)
		}
	}
	return out, nil
}

// ListByTenant returns every session for the tenant across all repos.
func (m *MemoryStore) ListByTenant(tenantID string) ([]*State, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("dosource/sessionstate: ListByTenant: empty tenantID")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*State, 0)
	for _, s := range m.rows {
		if s.TenantID == tenantID {
			cp := *s
			cp.PredictedConflicts = append([]ConflictPair(nil), s.PredictedConflicts...)
			out = append(out, &cp)
		}
	}
	// scan dosource-access-F2: bound the tenant-wide listing (parity with DDBStore).
	if len(out) > SessionsListMaxRows {
		out = out[:SessionsListMaxRows]
	}
	return out, nil
}

// ListStale returns sessions whose lastHeartbeat is older than cutoff.
func (m *MemoryStore) ListStale(cutoff time.Time) ([]*State, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*State, 0)
	for _, s := range m.rows {
		// Skip already-stale rows (idempotent reaping).
		if s.Status == StatusStale {
			continue
		}
		if s.LastHeartbeat.Before(cutoff) {
			cp := *s
			cp.PredictedConflicts = append([]ConflictPair(nil), s.PredictedConflicts...)
			out = append(out, &cp)
		}
	}
	return out, nil
}

// Len reports the row count. Helper for tests.
func (m *MemoryStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.rows)
}

// Compile-time check: MemoryStore implements Store.
var _ Store = (*MemoryStore)(nil)
