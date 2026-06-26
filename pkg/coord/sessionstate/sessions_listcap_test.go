// sessions_listcap_test.go — regression for scan dosource-access-F2: ListByTenant
// must bound the tenant-wide session listing so a huge self-tenant session set
// can't drain unbounded into Lambda memory. Exercised against MemoryStore (the
// DDBStore applies the identical SessionsListMaxRows cap to its pagination loop).
package sessionstate

import "testing"

func TestListByTenant_BoundedBySessionsListMaxRows(t *testing.T) {
	orig := SessionsListMaxRows
	SessionsListMaxRows = 3
	defer func() { SessionsListMaxRows = orig }()

	m := NewMemoryStore()
	const tenant = "t-cap"
	for i := 0; i < 10; i++ {
		if err := m.Put(&State{SessionID: id(i), TenantID: tenant, RepoID: "r1"}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	got, err := m.ListByTenant(tenant)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListByTenant returned %d sessions, want it capped at SessionsListMaxRows=3 (dosource-access-F2)", len(got))
	}
}

func id(i int) string { return "sess-" + string(rune('a'+i)) }
