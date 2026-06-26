// Package sessionstate is the session-state contract for dosource: each live
// LLM session has one record tracking its lifecycle (status transitions
// editing → bundling → pushed → merged | rolled-back, heartbeat, turn counter,
// shadow conflict summary), used by the live-sessions pane, the session-sweeper,
// the bundle saga, and the conflict-prediction layer.
//
// # Open/closed split (D4)
//
// This is the OPEN half of the doSource session-state seam — the open half (the
// closed platform has the durable one): the FREE, in-memory, single-machine
// MemoryStore plus the shared contract — the Store interface, the State / Status
// / ConflictPair types, ErrNotFound, and the SessionsListMaxRows cap. It runs
// offline in the gitevolved local daemon.
//
// The closed platform ships the DDB-backed store (the PAID, cross-machine,
// durable cloud store: pk=tenantId#repoId, sk=SESS#<sid>, heartbeat-TTL sweep on
// the unified docore-source-entities table). That store imports this package for
// the contract and satisfies Store (inverted dep: closed→open). Cross-machine
// durability — the live-sessions pane is read on different machines than the
// agents, and phantom-delete recovery needs state to survive process death — is
// exactly why the closed half is DDB-backed; the open MemoryStore covers the
// offline single-machine case.
//
// Both back-ends are proven identical by RunContract in the sessionstatetest
// subpackage, run on both sides of the module boundary so the seam cannot drift.
package sessionstate

import "time"

// Status is the lifecycle state of a session.
type Status string

const (
	StatusEditing    Status = "editing"
	StatusBundling   Status = "bundling"
	StatusPushed     Status = "pushed"
	StatusMerged     Status = "merged"
	StatusRolledBack Status = "rolled-back"
	StatusStale      Status = "stale"
)

// State is the DDB row for one session.
type State struct {
	TenantID        string
	RepoID          string
	SessionID       string
	Status          Status
	BundleHead      string
	ParentMain      string
	LastTurn        int
	MergeQueuePos   int
	Hostname        string
	PID             int
	LastHeartbeat   time.Time
	StagedFileCount int
	// AgentName is the CLI/agent label driving this session (e.g.
	// "claude-A"). Optional; "" for unlabeled sessions.
	AgentName string
	// OwnerUserID is the platform Customer/user id of the HUMAN who owns
	// this session, captured at attach time. "" = anonymous/legacy (the
	// pre-identity model). Joins doSource sessions to the Org/OrgMember
	// identity layer (domain/org.go) WITHOUT a new auth model — additive
	// metadata only. See PLAN glittery-wiggling-widget Phase 1.
	OwnerUserID string
	// OwnerDisplayName is the resolved human name for portal display.
	// "" → callers fall back to the session id.
	OwnerDisplayName string
	// OwnerOrgRole is the owner's Org role (owner/admin/member/viewer)
	// at attach time, for display chips. Optional.
	OwnerOrgRole string
	// DeliverableID links this session to a deliverable (the Work board's
	// grouping node). Additive metadata — "" = unassigned (the session falls
	// into the auto-derived bucket). Mirrors the OwnerUserID precedent: no new
	// auth model, no migration, absent on legacy rows. Set at attach/contribute
	// time when the agent tags its work; see internal/dosource/deliverable.
	DeliverableID      string
	PredictedConflicts []ConflictPair
}

// ConflictPair names a predicted conflict against a target.
type ConflictPair struct {
	Path string
	Vs   string // "main" | "session/<sid>"
}

// SessionsListMaxRows bounds how many sessions ListByTenant materializes
// (scan dosource-access-F2). The tenant-wide listing paginates the whole
// gsi_tenant_sessions partition into Lambda memory; an unbounded session set
// is a self-tenant read-amplification / memory drain. 1000 is far above any
// real concurrent-session count for one tenant; beyond it the result is
// truncated (logged) rather than draining without limit. Cursor-based full
// pagination is a separate feature, not this resource bound. Var (not const)
// so tests can exercise the truncation deterministically.
var SessionsListMaxRows = 1000

// Store is the interface dosource consumers use for session-state
// R/W. Implementations:
//
//   - MemoryStore  — tests + dev mode; in-memory only. (memory.go)
//   - DDBStore     — production; reads/writes the docore-source-entities
//     DDB table. (Phase-3 wire — TODO when the parallel-
//     session merge-gate clears.)
//
// All write paths are idempotent on (tenantID, sessionID) — the same
// session can call Heartbeat or SetStatus repeatedly with the same
// inputs and the row converges to the latest write.
type Store interface {
	// Get returns the State for one session. Returns ErrNotFound if
	// the session has never been registered.
	Get(tenantID, sessionID string) (*State, error)

	// Put writes the full state for a session. Used by Attach
	// (initial registration). Subsequent updates SHOULD use the
	// targeted methods (SetStatus, Heartbeat, etc.) for efficiency
	// + DDB partial-update semantics — but Put is allowed.
	Put(s *State) error

	// SetStatus is a focused update — flips the status field and
	// optionally bumps bundleHead/parentMain on the same write.
	// Used by the bundle saga's status transitions.
	SetStatus(tenantID, sessionID string, status Status, bundleHead, parentMain string) error

	// Heartbeat updates lastHeartbeat to now. Called by every active
	// agent on a regular cadence; the session-sweeper Lambda flips
	// status→stale on rows whose lastHeartbeat is older than the
	// configured threshold.
	Heartbeat(tenantID, sessionID string) error

	// BumpTurn atomically increments lastTurn and returns the NEW value.
	// Concurrent callers each get a distinct, monotonic turn — this is the
	// fence that replaces Add's unfenced Get→compute→Put read-modify-write
	// (which let racing adds compute the same turnIdx and clobber each other,
	// DSQS add concurrency=1). DDB backs it with a single atomic UpdateItem
	// "ADD lastTurn :one"; the in-memory store uses its mutex.
	BumpTurn(tenantID, sessionID string) (int, error)

	// SetShadowSummary updates stagedFileCount + predictedConflicts.
	// Called when shadow staging changes. Denormalized fields exist
	// so the live-sessions UI can render without a join.
	SetShadowSummary(tenantID, sessionID string, stagedFileCount int, predictedConflicts []ConflictPair) error

	// ListByRepo returns all sessions on one repo. Used by the
	// multi-agent live-sessions pane.
	ListByRepo(tenantID, repoID string) ([]*State, error)

	// ListByTenant returns the tenant's sessions across all repos, bounded at
	// SessionsListMaxRows. Powers the portal's "all projects this tenant has
	// touched" surface.
	ListByTenant(tenantID string) ([]*State, error)

	// ListStale returns sessions whose lastHeartbeat is older than
	// the cutoff. Used by the session-sweeper Lambda.
	ListStale(cutoff time.Time) ([]*State, error)
}
