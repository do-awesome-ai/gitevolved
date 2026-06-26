// idempotency_test.go — runs the shared conformance harness against the open
// MemoryStore. The closed DDBStore runs the SAME harness platform-side
// (internal/dosource/idempotency/ddb_contract_test.go), so the free local
// referee and the paid cloud referee are proven to behave identically.
package idempotency_test

import (
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/coord/idempotency"
	"github.com/do-awesome-ai/gitevolved/pkg/coord/idempotency/idempotencytest"
)

func TestMemoryStore_Contract(t *testing.T) {
	idempotencytest.RunContract(t, func(now func() time.Time) idempotency.Store {
		return idempotency.NewMemoryStoreWithClock(now)
	})
}
