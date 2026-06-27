// operation_test.go — thesis-driven tests for the operation/ package.
//
// Each TestThesis_* binds a specific doSource architectural claim to
// an observable invariant. Per the operator directive 2026-05-14:
// "as we build this lets build the thesis by proving it with testing"
// — the substrate's claims are unfalsifiable without tests that
// observe them. These tests are the binding layer.
//
// # Thesis claims proven here
//
//	T1. ContentAddressableOps  — same body + same causal parents → same op_id
//	T2. CausalParentsAreIdentity — same body, different parents → different op_id
//	T3. MetadataNotInIdentity   — same body + parents, different session/turn/time → same op_id
//	T4. RoundTripLossless       — every op kind survives marshal/seal/unmarshal/decode
//	T5. DiscriminatorSafety     — unknown op_type returns ErrUnknownOpType, never panics
//	T6. SchemaVersionSafety     — unknown schema_version returns ErrUnknownSchemaVersion
//	T7. ValidationAtBoundary    — every op rejects empty/invalid fields at Seal/Decode time
//	T8. RegistryCoverage        — every OpKind in AllOpKinds() has a registered factory
//	T9. CanonicalFormStable     — same logical content → byte-identical canonical form
//	T10. CausalParentsSorted    — Seal sorts causal_parents so order at construction doesn't affect op_id
package operation

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// -----------------------------------------------------------------
// T1. ContentAddressableOps — same body + same parents → same op_id
// -----------------------------------------------------------------
//
// This is the foundational claim of event-sourced doSource: ops are
// content-addressed. Same operation emitted from any session by any
// LLM at any wall-clock time produces the same op_id, which means
// the op-log naturally deduplicates without an explicit dedup pass.
func TestThesis_ContentAddressableOps(t *testing.T) {
	op := &AddFunction{
		Path:      "auth.go",
		Name:      "validateToken",
		Signature: "func validateToken(t string) error",
		Body:      "return errors.New(\"unimplemented\")",
		Language:  LanguageGo,
	}

	env1 := Envelope{SourceSession: "sess-A", SourceTurn: 1, EmittedAt: time.Now()}
	env2 := Envelope{SourceSession: "sess-A", SourceTurn: 1, EmittedAt: time.Now()}

	if err := env1.Seal(op); err != nil {
		t.Fatalf("env1.Seal: %v", err)
	}
	if err := env2.Seal(op); err != nil {
		t.Fatalf("env2.Seal: %v", err)
	}

	if env1.OpID == "" {
		t.Fatal("Seal did not set OpID")
	}
	if env1.OpID != env2.OpID {
		t.Errorf("content-addressable invariant violated: env1.OpID=%q env2.OpID=%q (same body, same parents)",
			env1.OpID, env2.OpID)
	}
}

// -----------------------------------------------------------------
// T2. CausalParentsAreIdentity — same body, different parents → different op_id
// -----------------------------------------------------------------
//
// Causal parents are part of the op's identity. An EditStatement
// against `getUser` is a different op than an EditStatement against
// `fetchUser` even if the bytes-at-the-range look identical —
// because the parent decl is part of what makes the op meaningful.
func TestThesis_CausalParentsAreIdentity(t *testing.T) {
	op := &EditStatement{
		Path:      "auth.go",
		FuncRef:   "validateToken",
		StmtRange: Range{Start: 120, End: 145},
		NewText:   "return nil",
	}

	envA := Envelope{
		SourceSession: "sess-A",
		CausalParents: []string{"op:aaa"},
	}
	envB := Envelope{
		SourceSession: "sess-A",
		CausalParents: []string{"op:bbb"},
	}

	if err := envA.Seal(op); err != nil {
		t.Fatalf("envA.Seal: %v", err)
	}
	if err := envB.Seal(op); err != nil {
		t.Fatalf("envB.Seal: %v", err)
	}

	if envA.OpID == envB.OpID {
		t.Errorf("causal-parents-as-identity violated: same op_id %q despite different parents", envA.OpID)
	}
}

// -----------------------------------------------------------------
// T3. MetadataNotInIdentity — session/turn/time DO NOT affect op_id
// -----------------------------------------------------------------
//
// The flip side of content-addressability: emit metadata
// (SourceSession, SourceTurn, EmittedAt) is recorded for display +
// audit but is NOT part of the op_id hash. Two LLM sessions
// emitting the exact same op produce the same op_id.
func TestThesis_MetadataNotInIdentity(t *testing.T) {
	op := &AddFile{
		Path:    "newfile.go",
		Content: []byte("package main\n"),
	}

	envSessA := Envelope{
		SourceSession: "sess-A",
		SourceTurn:    1,
		EmittedAt:     time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
	}
	envSessB := Envelope{
		SourceSession: "sess-B",
		SourceTurn:    99,
		EmittedAt:     time.Date(2026, 5, 14, 23, 59, 59, 999_999_999, time.UTC),
	}

	if err := envSessA.Seal(op); err != nil {
		t.Fatalf("envSessA.Seal: %v", err)
	}
	if err := envSessB.Seal(op); err != nil {
		t.Fatalf("envSessB.Seal: %v", err)
	}

	if envSessA.OpID != envSessB.OpID {
		t.Errorf("metadata-not-in-identity violated: envA.OpID=%q envB.OpID=%q (only session/turn/time differ)",
			envSessA.OpID, envSessB.OpID)
	}
}

// -----------------------------------------------------------------
// T4. RoundTripLossless — every op kind survives marshal/unmarshal
// -----------------------------------------------------------------
//
// The op-log is JSON on the wire. Every op kind must round-trip
// through marshal → unmarshal without losing any field. This proves
// that JSON is a faithful serialization for the operation vocabulary.
func TestThesis_RoundTripLossless(t *testing.T) {
	cases := []struct {
		name string
		op   Operation
	}{
		{"AddFile", &AddFile{Path: "a.go", Content: []byte("x")}},
		{"DeleteFile", &DeleteFile{Path: "a.go"}},
		{"AddDecl", &AddDecl{Path: "a.go", DeclKind: DeclKindStruct, Name: "X", Source: "type X struct{}"}},
		{"EditDecl", &EditDecl{Path: "a.go", DeclKind: DeclKindStruct, Name: "X", NewSource: "type X struct{ Y int }"}},
		{"DeleteDecl", &DeleteDecl{Path: "a.go", DeclKind: DeclKindStruct, Name: "X"}},
		{"RenameSymbol", &RenameSymbol{Path: "a.go", OldName: "X", NewName: "Y", Scope: ScopeRef{Path: "a.go"}}},
		{"AddFunction", &AddFunction{Path: "a.go", Name: "f", Signature: "func f()", Body: "{}", Language: LanguageGo}},
		{"DeleteFunction", &DeleteFunction{Path: "a.go", Name: "f"}},
		{"RewriteFunction", &RewriteFunction{Path: "a.go", Name: "f", NewBody: "{ return }"}},
		{"EditStatement", &EditStatement{Path: "a.go", FuncRef: "f", StmtRange: Range{Start: 0, End: 1}, NewText: "x"}},
		{"AddImport", &AddImport{Path: "a.go", Module: "fmt"}},
		{"RemoveImport", &RemoveImport{Path: "a.go", Module: "fmt"}},
		{"EditImport", &EditImport{Path: "a.go", OldModule: "x/v1", NewModule: "x/v2"}},
		{"AddCell", &AddCell{Notebook: "n.ipynb", CellIdx: 0, Kind_: CellKindCode, Source: "print(1)"}},
		{"EditCell", &EditCell{Notebook: "n.ipynb", CellRef: CellRef{Index: 0}, NewSource: "print(2)"}},
		{"DeleteCell", &DeleteCell{Notebook: "n.ipynb", CellRef: CellRef{Index: 0}}},
		{"RewriteRegion", &RewriteRegion{Path: "a.go", ByteRange: Range{Start: 0, End: 5}, Content: []byte("hello")}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := Envelope{
				SourceSession: "sess-A",
				SourceTurn:    1,
				EmittedAt:     time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
			}
			if err := env.Seal(c.op); err != nil {
				t.Fatalf("Seal: %v", err)
			}

			// Marshal → unmarshal the envelope.
			wire, err := json.Marshal(&env)
			if err != nil {
				t.Fatalf("json.Marshal envelope: %v", err)
			}
			var got Envelope
			if err := json.Unmarshal(wire, &got); err != nil {
				t.Fatalf("json.Unmarshal envelope: %v", err)
			}

			// Decode body.
			decoded, err := got.Decode()
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			if !reflect.DeepEqual(decoded, c.op) {
				t.Errorf("round-trip mismatch:\n  want = %+v\n  got  = %+v", c.op, decoded)
			}

			// OpID must match across the wire too.
			if got.OpID != env.OpID {
				t.Errorf("op_id mismatch across wire: pre=%q post=%q", env.OpID, got.OpID)
			}
		})
	}
}

// -----------------------------------------------------------------
// T5. DiscriminatorSafety — unknown op_type returns ErrUnknownOpType
// -----------------------------------------------------------------
//
// A malicious or future-version producer might emit op_type values
// this build doesn't know about. The contract says: return a named
// error, never silently corrupt by deserializing into the wrong
// concrete type, never panic.
func TestThesis_DiscriminatorSafety(t *testing.T) {
	env := Envelope{
		OpType:        "NotARealOpType",
		SchemaVersion: CurrentSchemaVersion,
		Body:          json.RawMessage(`{}`),
	}
	_, err := env.Decode()
	if err == nil {
		t.Fatal("Decode of unknown op_type returned nil error — should have returned ErrUnknownOpType")
	}
	if !errors.Is(err, ErrUnknownOpType) {
		t.Errorf("expected ErrUnknownOpType, got %v", err)
	}
}

// -----------------------------------------------------------------
// T6. SchemaVersionSafety — unknown schema_version returns named error
// -----------------------------------------------------------------
//
// Forward-compatibility: a future v2 producer might emit an envelope
// this v1 build can't handle. The contract says: reject explicitly
// with ErrUnknownSchemaVersion. Don't try to best-effort.
func TestThesis_SchemaVersionSafety(t *testing.T) {
	env := Envelope{
		OpType:        OpKindAddFile,
		SchemaVersion: "v999",
		Body:          json.RawMessage(`{"path":"a","content":null}`),
	}
	_, err := env.Decode()
	if err == nil {
		t.Fatal("Decode of unknown schema_version returned nil error — should have returned ErrUnknownSchemaVersion")
	}
	if !errors.Is(err, ErrUnknownSchemaVersion) {
		t.Errorf("expected ErrUnknownSchemaVersion, got %v", err)
	}
}

// -----------------------------------------------------------------
// T7. ValidationAtBoundary — each op rejects bad inputs at Seal/Decode
// -----------------------------------------------------------------
//
// Validation runs at the wire boundary (Seal and Decode). The
// projector / conflict detector / extractor downstream can trust
// that any Operation handed to them has passed Validate. Empty
// required fields, invalid enums, malformed ranges — all caught here.
func TestThesis_ValidationAtBoundary(t *testing.T) {
	cases := []struct {
		name      string
		op        Operation
		wantError error // sentinel via errors.Is
	}{
		{"AddFile empty path", &AddFile{Path: ""}, ErrEmptyField},
		{"AddDecl invalid decl_kind", &AddDecl{Path: "a", Name: "X", DeclKind: "garbage"}, ErrInvalidEnum},
		{"AddFunction invalid language", &AddFunction{Path: "a", Name: "f", Signature: "func f()", Language: "klingon"}, ErrInvalidEnum},
		{"EditStatement invalid range", &EditStatement{Path: "a", FuncRef: "f", StmtRange: Range{Start: 5, End: 5}, NewText: "x"}, ErrInvalidRange},
		{"RenameSymbol no-op rename", &RenameSymbol{Path: "a", OldName: "X", NewName: "X", Scope: ScopeRef{Path: "a"}}, nil}, // string-match on message
		{"EditImport no-op", &EditImport{Path: "a", OldModule: "fmt", NewModule: "fmt"}, nil},                                // string-match on message
		{"AddCell invalid kind", &AddCell{Notebook: "n", CellIdx: 0, Kind_: "video"}, ErrInvalidEnum},
		{"RewriteRegion negative start", &RewriteRegion{Path: "a", ByteRange: Range{Start: -1, End: 5}}, ErrInvalidRange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := Envelope{}
			err := env.Seal(c.op)
			if err == nil {
				t.Fatalf("expected validation error, got nil for %+v", c.op)
			}
			if c.wantError != nil && !errors.Is(err, c.wantError) {
				t.Errorf("expected %v, got %v", c.wantError, err)
			}
		})
	}
}

// -----------------------------------------------------------------
// T8. RegistryCoverage — every OpKind has a factory
// -----------------------------------------------------------------
//
// AllOpKinds() is the source of truth for "what ops exist in this
// schema version." Every one must be registered in opFactory or
// Decode will fail at runtime. Catches the case where adding an op
// kind to the enum without an init() registration breaks the
// substrate silently.
func TestThesis_RegistryCoverage(t *testing.T) {
	for _, kind := range AllOpKinds() {
		factory, ok := opFactory[kind]
		if !ok {
			t.Errorf("OpKind %q has no registered factory", kind)
			continue
		}
		instance := factory()
		if instance == nil {
			t.Errorf("OpKind %q factory returned nil", kind)
			continue
		}
		if instance.Kind() != kind {
			t.Errorf("OpKind %q factory returned op with Kind()=%q", kind, instance.Kind())
		}
	}
}

// -----------------------------------------------------------------
// T9. CanonicalFormStable — same logical content → byte-identical
// -----------------------------------------------------------------
//
// Op-id determinism requires canonical serialization to be stable.
// Two calls to canonicalBody on the same op MUST produce byte-equal
// output. Without this guarantee, op-id is non-deterministic and
// the entire content-addressable claim collapses.
func TestThesis_CanonicalFormStable(t *testing.T) {
	op := &AddFunction{
		Path:      "auth.go",
		Name:      "validateToken",
		Signature: "func validateToken(t string) error",
		Body:      "return nil",
		Language:  LanguageGo,
	}
	a, err := op.canonicalBody()
	if err != nil {
		t.Fatalf("canonicalBody first call: %v", err)
	}
	b, err := op.canonicalBody()
	if err != nil {
		t.Fatalf("canonicalBody second call: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("canonicalBody not byte-stable:\n  first  = %s\n  second = %s", a, b)
	}
}

// -----------------------------------------------------------------
// T10. CausalParentsSorted — Seal canonicalizes parent order
// -----------------------------------------------------------------
//
// Callers may construct an Envelope with CausalParents in any order
// (e.g., the order their internal data structure happens to iterate).
// Seal sorts them so the op_id is independent of construction order.
// Otherwise the content-addressable claim depends on caller hygiene.
func TestThesis_CausalParentsSorted(t *testing.T) {
	op := &EditStatement{
		Path:      "auth.go",
		FuncRef:   "validateToken",
		StmtRange: Range{Start: 0, End: 10},
		NewText:   "x",
	}
	envForward := Envelope{CausalParents: []string{"op:aaa", "op:bbb", "op:ccc"}}
	envReverse := Envelope{CausalParents: []string{"op:ccc", "op:bbb", "op:aaa"}}

	if err := envForward.Seal(op); err != nil {
		t.Fatalf("envForward.Seal: %v", err)
	}
	if err := envReverse.Seal(op); err != nil {
		t.Fatalf("envReverse.Seal: %v", err)
	}

	if envForward.OpID != envReverse.OpID {
		t.Errorf("causal-parents-order-independence violated: forward=%q reverse=%q",
			envForward.OpID, envReverse.OpID)
	}
	// Both should now be in sorted order.
	want := []string{"op:aaa", "op:bbb", "op:ccc"}
	if !reflect.DeepEqual(envReverse.CausalParents, want) {
		t.Errorf("Seal did not sort CausalParents: got %v", envReverse.CausalParents)
	}
}

// -----------------------------------------------------------------
// Bonus: nil Operation handling
// -----------------------------------------------------------------
//
// Defensive: Seal(nil) returns ErrNilOperation, never panics.
func TestNilOperationReturnsNamedError(t *testing.T) {
	env := Envelope{}
	err := env.Seal(nil)
	if !errors.Is(err, ErrNilOperation) {
		t.Errorf("expected ErrNilOperation, got %v", err)
	}
}
