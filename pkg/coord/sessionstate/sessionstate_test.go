// sessionstate_test.go — runs the shared conformance harness against the open
// MemoryStore, plus MemoryStore-specific tests. The closed DDB store runs the
// SAME harness platform-side (internal/dosource/sessionstate/ddb_live_test.go),
// so the free local store and the paid cloud store are proven identical.
package sessionstate_test

import (
	"sync"
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/coord/sessionstate"
	"github.com/do-awesome-ai/gitevolved/pkg/coord/sessionstate/sessionstatetest"
)

func TestMemoryStore_Contract(t *testing.T) {
	sessionstatetest.RunContract(t, func(now func() time.Time) sessionstate.Store {
		return sessionstate.NewMemoryStoreWithClock(now)
	})
}

// ---- MemoryStore-specific tests ----

func TestMemoryStore_ConcurrentHeartbeats(t *testing.T) {
	fakeNow := time.Date(2026, 5, 6, 18, 0, 0, 0, time.UTC)
	s := sessionstate.NewMemoryStoreWithClock(func() time.Time { return fakeNow })
	_ = s.Put(sessionstatetest.NewTestState("t1", "r1", "sess1"))

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = s.Heartbeat("t1", "sess1")
			}
		}()
	}
	wg.Wait()
	got, _ := s.Get("t1", "sess1")
	if !got.LastHeartbeat.Equal(fakeNow) {
		t.Fatalf("concurrent heartbeats produced unexpected LastHeartbeat: %v", got.LastHeartbeat)
	}
}

func TestMemoryStore_Len(t *testing.T) {
	s := sessionstate.NewMemoryStore()
	if s.Len() != 0 {
		t.Fatalf("expected empty store Len=0, got %d", s.Len())
	}
	_ = s.Put(sessionstatetest.NewTestState("t1", "r1", "sess1"))
	_ = s.Put(sessionstatetest.NewTestState("t1", "r1", "sess2"))
	if s.Len() != 2 {
		t.Fatalf("expected Len=2 after 2 Puts, got %d", s.Len())
	}
}
