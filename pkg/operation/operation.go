// Package operation defines the typed operation vocabulary that is
// the unit of source control in event-sourced doSource.
//
// # Why this package exists
//
// An earlier design stored file diffs as the unit of source control and
// coordinated parallel-session collisions via an in-process referee —
// "solving git's problem with git's tools".
//
// In this design, files are projections from a typed-operation log.
// Each operation is a discriminated record: RenameSymbol /
// AddFunction / EditStatement / RewriteRegion / AddCell / etc.
// Operations are content-addressed, append-only, causally-ordered.
//
// This package is the bottom of the dependency stack — every other
// component (projector / conflict / extractor / export)
// imports it but it imports nothing from sibling packages.
//
// # File layout
//
//   - operation.go (this file) — Envelope, Operation interface, OpKind enum,
//     registry, canonical-form serializer, OpID derivation.
//   - types.go — DeclKind, Language, CellKind, Range, ScopeRef, CellRef.
//   - ops.go — the 19 concrete op types grouped by domain (file / decl /
//     func / sub-func / import / notebook / region).
//   - operation_test.go — round-trip, op-id determinism, discriminator safety.
//
// # Wire-format contract
//
// Envelope serializes as JSON with a top-level `op_type` discriminator.
// `op_id` is sha256(canonical(body) || canonical(causal_parents)) —
// metadata fields (emitted_at, source_session, source_turn) are NOT
// included in the hash so semantically-identical ops emitted from
// different sessions deduplicate naturally. Same idiom as Sapling /
// git's commit IDs but on the operation layer instead of the snapshot
// layer.
//
// # Schema versioning
//
// `schema_version` on the envelope is the wire-format version. v1
// readers reject unknown schema versions explicitly (via
// ErrUnknownSchemaVersion) rather than silently best-effort. Adding
// a new op kind or new value to an existing enum (DeclKind /
// Language / CellKind) bumps the schema version.
package operation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// CurrentSchemaVersion is the wire-format version this package
// emits. Readers compare against this and reject unknown versions.
const CurrentSchemaVersion = "v1"

// OpKind discriminates the concrete operation type inside an
// envelope. The string value is the wire-format discriminator —
// changing a value is a breaking schema change.
type OpKind string

const (
	// File-level
	OpKindAddFile    OpKind = "AddFile"
	OpKindDeleteFile OpKind = "DeleteFile"

	// Declaration-level (struct / enum / type / const / var)
	OpKindAddDecl      OpKind = "AddDecl"
	OpKindEditDecl     OpKind = "EditDecl"
	OpKindDeleteDecl   OpKind = "DeleteDecl"
	OpKindRenameSymbol OpKind = "RenameSymbol"

	// Function-level
	OpKindAddFunction     OpKind = "AddFunction"
	OpKindDeleteFunction  OpKind = "DeleteFunction"
	OpKindRewriteFunction OpKind = "RewriteFunction"

	// Sub-function-level
	OpKindEditStatement OpKind = "EditStatement"

	// Import-level
	OpKindAddImport    OpKind = "AddImport"
	OpKindRemoveImport OpKind = "RemoveImport"
	OpKindEditImport   OpKind = "EditImport"

	// Notebook-level
	OpKindAddCell    OpKind = "AddCell"
	OpKindEditCell   OpKind = "EditCell"
	OpKindDeleteCell OpKind = "DeleteCell"

	// Honest fallback (NOT a failure mode — first-class op)
	OpKindRewriteRegion OpKind = "RewriteRegion"
)

// AllOpKinds returns every OpKind known to this schema version.
// Used by the registry init + by tests asserting registry coverage.
func AllOpKinds() []OpKind {
	return []OpKind{
		OpKindAddFile, OpKindDeleteFile,
		OpKindAddDecl, OpKindEditDecl, OpKindDeleteDecl, OpKindRenameSymbol,
		OpKindAddFunction, OpKindDeleteFunction, OpKindRewriteFunction,
		OpKindEditStatement,
		OpKindAddImport, OpKindRemoveImport, OpKindEditImport,
		OpKindAddCell, OpKindEditCell, OpKindDeleteCell,
		OpKindRewriteRegion,
	}
}

// Operation is the common interface every concrete op type in ops.go
// implements. Kind() returns the discriminator; Validate() runs
// schema + semantic self-checks (non-empty path, valid enum value,
// well-formed range); canonicalBody() returns the deterministic
// JSON serialization used for op-id derivation.
//
// canonicalBody is unexported because callers should always go
// through Envelope.Seal() — direct hash derivation would skip
// validation.
type Operation interface {
	Kind() OpKind
	Validate() error
	canonicalBody() ([]byte, error)
}

// Envelope wraps a concrete Operation with metadata, discriminator,
// and the content-addressed OpID. Envelopes are what land in the
// op-log; the concrete Operation is accessed via Decode().
type Envelope struct {
	// OpID is sha256(canonical(body) || canonical(causal_parents)) —
	// derived by Seal(). Empty before Seal() and never written
	// directly by callers.
	OpID string `json:"op_id,omitempty"`

	// OpType discriminates the concrete op type. Set by Seal() from
	// the Operation passed in; immutable after seal.
	OpType OpKind `json:"op_type"`

	// SchemaVersion is the wire-format version. Set by Seal() to
	// CurrentSchemaVersion; readers reject unknown versions.
	SchemaVersion string `json:"schema_version"`

	// EmittedAt is wall-clock at emit time. Display + tiebreaker
	// only — NOT part of the op-id hash.
	EmittedAt time.Time `json:"emitted_at"`

	// SourceSession is the session UUID that emitted this op.
	// Not part of the op-id hash (same body from different sessions
	// dedupes to same op_id).
	SourceSession string `json:"source_session"`

	// SourceTurn is the turn index within the source session.
	SourceTurn int `json:"source_turn"`

	// CausalParents is the list of op_ids this op depends on.
	// EditStatement carries the containing function's op_id here.
	// PART of the op-id hash (sorted for canonical form).
	CausalParents []string `json:"causal_parents,omitempty"`

	// Body is the canonical-form JSON of the concrete op. Set by
	// Seal(); decoded by Decode().
	Body json.RawMessage `json:"body"`
}

// Seal computes the op-id, sets discriminator + schema version, and
// canonicalizes the body. Callers MUST call Seal on a fresh
// Envelope before persisting or transmitting.
//
// Idempotent: calling Seal twice on the same (Envelope, Operation)
// pair yields byte-identical output (Op-id determinism contract).
func (env *Envelope) Seal(op Operation) error {
	if op == nil {
		return ErrNilOperation
	}
	if err := op.Validate(); err != nil {
		return fmt.Errorf("operation.Seal: %w", err)
	}
	body, err := op.canonicalBody()
	if err != nil {
		return fmt.Errorf("operation.Seal: canonicalize body: %w", err)
	}

	env.OpType = op.Kind()
	env.SchemaVersion = CurrentSchemaVersion
	env.Body = body
	sort.Strings(env.CausalParents) // canonical form

	env.OpID = deriveOpID(body, env.CausalParents)
	return nil
}

// Decode returns the concrete Operation for the envelope's OpType.
// The returned Operation has been Validate()-checked. Errors:
//   - ErrUnknownOpType if OpType is not in the registry.
//   - ErrUnknownSchemaVersion if SchemaVersion ≠ CurrentSchemaVersion.
//   - wrapped json.Unmarshal error on body shape mismatch.
//   - wrapped Validate error on semantic mismatch.
func (env *Envelope) Decode() (Operation, error) {
	if env.SchemaVersion != CurrentSchemaVersion {
		return nil, fmt.Errorf("%w: %q (this build supports %q)",
			ErrUnknownSchemaVersion, env.SchemaVersion, CurrentSchemaVersion)
	}
	factory, ok := opFactory[env.OpType]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownOpType, env.OpType)
	}
	op := factory()
	if err := json.Unmarshal(env.Body, op); err != nil {
		return nil, fmt.Errorf("operation.Decode %q: %w", env.OpType, err)
	}
	if err := op.Validate(); err != nil {
		return nil, fmt.Errorf("operation.Decode %q: %w", env.OpType, err)
	}
	return op, nil
}

// opFactory maps OpKind → factory function returning a zero-value
// concrete pointer. Populated by init() at package load. Used by
// Envelope.Decode() to dispatch deserialization.
var opFactory = map[OpKind]func() Operation{}

// register associates an OpKind with its factory. Called from each
// concrete op's init(). Panics on double-registration so collisions
// fail loudly at package load, never at runtime.
func register(kind OpKind, factory func() Operation) {
	if _, dup := opFactory[kind]; dup {
		panic(fmt.Sprintf("operation: double registration of %q", kind))
	}
	opFactory[kind] = factory
}

// deriveOpID computes the content-addressed op-id.
//
// Formula: sha256( canonicalJSON(body) || 0x00 || canonicalJSON(causalParents) )
//
// The 0x00 separator prevents adversarial body+parents collisions
// (a body ending with `]` followed by an empty parents `[]` could
// otherwise collide with a body ending with `]` followed by a
// non-empty parents — the separator removes that ambiguity).
func deriveOpID(body []byte, causalParents []string) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte{0x00})
	parentsCanonical, _ := json.Marshal(causalParents) // []string is always JSON-safe
	h.Write(parentsCanonical)
	return "op:" + hex.EncodeToString(h.Sum(nil))
}

// marshalCanonical serializes v to canonical JSON: no whitespace,
// stable struct field order (json package preserves declaration
// order), map keys sorted alphabetically (json package default).
//
// Used by every concrete op's canonicalBody() — concentrated here
// so any future tweak to canonical-form (e.g., explicit UTF-8
// normalization) lands in one place.
func marshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // canonical form should not HTML-escape
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder appends a trailing newline; trim it so two
	// canonicalizations of the same value are byte-identical
	// regardless of which path produced them.
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

// Named errors.
var (
	// ErrNilOperation is returned by Seal when called with a nil op.
	ErrNilOperation = errors.New("operation: nil Operation")
	// ErrUnknownOpType is returned by Decode when the envelope's
	// OpType is not in the registry.
	ErrUnknownOpType = errors.New("operation: unknown op_type")
	// ErrUnknownSchemaVersion is returned by Decode when the
	// envelope's SchemaVersion is unsupported by this build.
	ErrUnknownSchemaVersion = errors.New("operation: unknown schema_version")
	// ErrEmptyField is returned by validators when a required field
	// is the zero value. Wrap with %w + the specific field name.
	ErrEmptyField = errors.New("operation: required field empty")
	// ErrInvalidEnum is returned by validators when an enum field
	// carries a value not in IsValid(). Wrap with %w + the value.
	ErrInvalidEnum = errors.New("operation: invalid enum value")
	// ErrInvalidRange is returned when a Range or byte-offset pair
	// fails IsValid (negative offsets, start >= end).
	ErrInvalidRange = errors.New("operation: invalid range")
)
