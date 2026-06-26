// crossfile.go — advisory cross-file conflict heuristic (Phase 1d).
//
// # What this is
//
// detectCrossFile spots one specific class of cross-file hazard that
// the same-file detector in conflict.go cannot see: op A changes the
// IDENTITY of a definition (renames it, deletes a function, deletes a
// decl) in file X, while op B in a DIFFERENT file Y still references
// that definition's name in its body text. After A lands, B's
// reference dangles.
//
// # Why it exists
//
// conflict.go:119-123 short-circuits any two ops on different paths to
// VerdictIndependent ("cross-file resolution is Phase 2"). That is the
// right default — most cross-file pairs ARE independent — but it is a
// blind spot for the rename-invalidates-remote-callsite case the
// package header itself calls out (auth.go renames getUser; session.go
// calls getUser). Phase 1d closes that blind spot with a cheap,
// purely-textual heuristic, WITHOUT pulling in LSP-class symbol
// resolution (still Phase 2).
//
// # Advisory, NOT blocking — by design
//
// The result is VerdictCrossFileSuspect. Unlike VerdictSemanticConflict
// it does NOT block a merge. It is a hint for operator-facing surfaces
// and telemetry: "these two ops MIGHT clash across files; a human or
// the projector should double-check." This honors the conflict.go:289
// philosophy: default to Independent, never false-positive-block
// legitimate parallel work. A heuristic that blocked merges on a
// textual name match would be intolerable (every common identifier
// would collide); a heuristic that merely flags is safe.
//
// # Matching is identifier-boundary, not substring
//
// We match A's symbol name in B's body text with whole-word
// (identifier-boundary) semantics via a \bNAME\b regexp. "getUser"
// must NOT match inside "getUserProfile". strings.Contains would
// false-positive on every shared prefix/suffix, defeating the point.
//
// # Symmetry
//
// detectCrossFile(a,b) == detectCrossFile(b,a). The function is a
// logical OR over BOTH orderings internally: it checks (A-is-identity-
// changer AND B-references-A) AND (B-is-identity-changer AND
// A-references-B). Either firing yields a suspect. Because the OR is
// symmetric in a/b, the verdict is order-independent by construction.
// Preserves the Detect symmetry contract (TestThesis_Symmetric).
//
// # Documented limits (NOT tractable without LSP — kept deferred)
//
//   - Scope resolution: a local variable, parameter, or shadowing
//     binding in B that happens to share A's name will match. We
//     accept these name-collision false positives as the documented
//     ceiling of a textual heuristic — acceptable BECAUSE the verdict
//     is advisory, never blocking. Real scope resolution is Phase 2.
//   - Type-change-without-name-change: if A alters a definition's TYPE
//     or contract but keeps its name, no textual signal exists; not
//     detected.
//   - Precise arity / signature-shape change: detecting that A changed
//     a function's parameter list (so B's call site is now wrong even
//     though the NAME still resolves) needs parse-level analysis. A
//     single op carries no before-state, so we cannot infer a
//     signature change from one op alone — hence AddFunction and
//     RewriteFunction are deliberately NOT treated as identity
//     changers here (RewriteFunction preserves its signature by
//     contract per operation/ops.go; AddFunction introduces a new
//     symbol rather than mutating an existing one). Including them
//     would add false-positive noise with no reliable signal.
//
// The identity-changing set is therefore exactly: RenameSymbol,
// DeleteFunction, DeleteDecl — the three ops that provably remove or
// rebind an EXISTING symbol's name, which is the only thing a remote
// textual reference can dangle against.
package conflict

import (
	"regexp"
	"sync"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// identityChange describes an op that removes or rebinds an existing
// definition's name. The Name is what a remote reference would dangle
// against after the op lands.
type identityChange struct {
	Path string
	Name string
}

// asIdentityChange returns the (path, name) an op rebinds/removes, and
// true, if op is one of the identity-changing kinds. RenameSymbol uses
// OldName (the name that disappears). AddFunction / RewriteFunction are
// intentionally excluded — see the package header's "Documented limits."
func asIdentityChange(op operation.Operation) (identityChange, bool) {
	switch o := op.(type) {
	case *operation.RenameSymbol:
		return identityChange{Path: o.Path, Name: o.OldName}, true
	case *operation.DeleteFunction:
		return identityChange{Path: o.Path, Name: o.Name}, true
	case *operation.DeleteDecl:
		return identityChange{Path: o.Path, Name: o.Name}, true
	}
	return identityChange{}, false
}

// referenceText returns the op's body-text field (the text in which a
// remote symbol reference would appear) plus its path, and true if the
// op carries such a field. These are the fields a cross-file callsite
// lives in: EditStatement.NewText, AddFunction.Body, EditDecl.NewSource,
// RewriteFunction.NewBody, AddDecl.Source.
func referenceText(op operation.Operation) (path, text string, ok bool) {
	switch o := op.(type) {
	case *operation.EditStatement:
		return o.Path, o.NewText, true
	case *operation.AddFunction:
		return o.Path, o.Body, true
	case *operation.EditDecl:
		return o.Path, o.NewSource, true
	case *operation.RewriteFunction:
		return o.Path, o.NewBody, true
	case *operation.AddDecl:
		return o.Path, o.Source, true
	}
	return "", "", false
}

// boundaryCache memoizes compiled \bNAME\b regexps. Conflict detection
// runs hot (every op pair in a contribution batch), and the symbol set
// is small and repeats, so caching the compiled matcher avoids
// recompiling the same pattern on every call.
var (
	boundaryCacheMu sync.Mutex
	boundaryCache   = map[string]*regexp.Regexp{}
)

// referencesName reports whether text contains name at an identifier
// boundary (whole word) — NOT as a substring. "getUser" does not match
// inside "getUserProfile". Empty name never matches.
func referencesName(text, name string) bool {
	if name == "" || text == "" {
		return false
	}
	re := boundaryRegexp(name)
	return re.MatchString(text)
}

func boundaryRegexp(name string) *regexp.Regexp {
	boundaryCacheMu.Lock()
	defer boundaryCacheMu.Unlock()
	if re, ok := boundaryCache[name]; ok {
		return re
	}
	// \b is an ASCII identifier boundary: it sits between a word char
	// [0-9A-Za-z_] and a non-word char (or string edge). QuoteMeta
	// escapes any regex metacharacters in the symbol name so an
	// operator-supplied name can never be interpreted as a pattern.
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	boundaryCache[name] = re
	return re
}

// detectCrossFile applies the advisory cross-file heuristic. It returns
// a VerdictCrossFileSuspect Result when one op changes a definition's
// identity in one file and the OTHER op (in a DIFFERENT file) textually
// references that definition's name at an identifier boundary. Otherwise
// it returns a zero Result (Verdict == "") — the caller treats that as
// "no cross-file suspicion, fall through."
//
// Symmetric by construction: it ORs both orderings (a-changes/b-refs and
// b-changes/a-refs).
func detectCrossFile(a, b operation.Operation) Result {
	if a == nil || b == nil {
		return Result{}
	}
	if r, ok := crossFileSuspect(a, b); ok {
		return r
	}
	if r, ok := crossFileSuspect(b, a); ok {
		return r
	}
	return Result{}
}

// crossFileSuspect checks the single ordering "changer is the
// identity-changing op, ref is the op that might reference it." Returns
// a suspect Result + true when changer rebinds/removes a name that ref
// references at an identifier boundary in a DIFFERENT file.
func crossFileSuspect(changer, ref operation.Operation) (Result, bool) {
	ic, ok := asIdentityChange(changer)
	if !ok || ic.Name == "" || ic.Path == "" {
		return Result{}, false
	}
	refPath, refText, ok := referenceText(ref)
	if !ok || refPath == "" {
		return Result{}, false
	}
	// Cross-FILE only: same-path pairs are the same-file detector's job.
	if refPath == ic.Path {
		return Result{}, false
	}
	if !referencesName(refText, ic.Name) {
		return Result{}, false
	}
	return Result{
		Verdict: VerdictCrossFileSuspect,
		Reason:  "advisory: op changes symbol identity in one file while another file textually references that name (cross-file callsite may dangle; not merge-blocking)",
	}, true
}
