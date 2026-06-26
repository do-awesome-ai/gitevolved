// enqueuer.go — the durable-enqueue seam the Merge handler depends on.
//
// Service.MergeQueue is typed as Enqueuer so production injects the
// DDB-backed DDBStore (fleet-global, durable) while tests/dev inject the
// in-process MemEnqueuer. Both return a 0-based queue position. (The in-memory
// operation-model Queue in queue.go is a separate, not-yet-wired path for the
// future conflict-rich Drain.)
package mergequeue

import (
	"context"
	"errors"
	"sync"
)

// Enqueuer durably appends a bundle to a repo's merge queue and returns its
// 0-based position. DDBStore (prod) and MemEnqueuer (tests/dev) implement it.
type Enqueuer interface {
	Enqueue(ctx context.Context, tenantID, repoDir string, b BundleRef) (int, error)
}

// MemEnqueuer is an in-process Enqueuer for tests + dev. Position is a
// per-(tenant,repo) counter under a mutex — correct within one process, which
// is all dev/tests need (prod uses the durable, fleet-global DDBStore).
type MemEnqueuer struct {
	mu     sync.Mutex
	counts map[string]int // key: tenantID + "#" + repoDir
}

// NewMemEnqueuer returns an empty in-process Enqueuer.
func NewMemEnqueuer() *MemEnqueuer { return &MemEnqueuer{counts: map[string]int{}} }

// Enqueue implements Enqueuer.
func (m *MemEnqueuer) Enqueue(_ context.Context, tenantID, repoDir string, b BundleRef) (int, error) {
	if tenantID == "" || repoDir == "" || b.BundleID == "" {
		return 0, errors.New("dosource/mergequeue: Enqueue: empty tenantID, repoDir, or bundleID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := tenantID + "#" + repoDir
	pos := m.counts[k]
	m.counts[k] = pos + 1
	return pos, nil
}
