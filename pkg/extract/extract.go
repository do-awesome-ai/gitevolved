// Package extract derives a typed Operation from the pre/post content
// of an LLM Edit/Write tool call.
//
// # Why this exists
//
// In event-sourced doSource, every LLM Edit/Write/NotebookEdit tool
// call must produce a typed operation that lands in the op-log. The
// PostToolUse hook captures pre-edit and post-edit file content;
// this package is what turns that pair into an Operation.
//
// # v1.0 scope
//
// Three op kinds at this tier:
//
//   - AddFile      — pre==nil, post!=nil
//   - DeleteFile   — pre!=nil, post==nil
//   - RewriteRegion — pre!=nil, post!=nil, computed via minimal-range diff
//                     (longest common prefix + suffix)
//
// AST-based extraction (AddFunction / EditFunction / RenameSymbol /
// EditStatement / AddDecl / etc.) ships in Phase 1.1 ALONGSIDE the
// matching projector AST handlers. The two layers MUST advance
// together: extracting a typed op the projector can't apply produces
// silent corruption. v1.0 ships the conservative shape — every edit
// to an existing file becomes a RewriteRegion. That's the honest
// fallback per D5 (extraction-confidence policy) in the locked design.
//
// # The load-bearing thesis
//
// The architect review flagged extraction CORRECTNESS (not just
// coverage) as ship-blocker 1.1: an extractor that emits a wrong
// typed op produces a falsified op-log, and every downstream
// consumer (conflict detector, keepgate, bug-pattern-scan) builds
// on a lie.
//
// The structural defense is the round-trip invariant proven by
// TestThesis_ExtractionProjectsBack:
//
//	∀ (path, pre, post) →
//	  let op = Extract(path, pre, post)
//	  in   project(pre_state, op) == post_state
//
// Phase 1.1 AST extraction will gain the same invariant per AST-based
// op kind — never silently emit a typed op without proving it
// round-trips through the projector first. The D5 policy formalizes
// this as a runtime check too: production callers MUST validate
// project(baseline, candidate) == post before recording a candidate
// typed op, falling back to RewriteRegion + a diagnostic-stream
// entry on mismatch.
package extract

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// Named errors.
var (
	// ErrNoChange is returned when pre == post (byte-equal). Callers
	// should treat this as a no-op signal — no op enters the log.
	ErrNoChange = errors.New("extract: no change detected (pre == post)")

	// ErrEmpty is returned when both pre and post are nil. Callers
	// should treat this as a bug — there's nothing to extract.
	ErrEmpty = errors.New("extract: both pre and post are nil")

	// ErrEmptyPath is returned when path is empty.
	ErrEmptyPath = errors.New("extract: path is empty")
)

// Extract derives the typed Operation that describes the change from
// pre to post for the file at path.
//
//	pre  | post | result
//	-----+------+------------------------------------
//	nil  | nil  | ErrEmpty
//	nil  | bs   | AddFile{Path: path, Content: bs}
//	bs   | nil  | DeleteFile{Path: path}
//	a    | b    | RewriteRegion covering minimal-range diff
//	      |      | (or ErrNoChange if a == b byte-equal)
//
// For v1.0, all "edit existing file" cases produce RewriteRegion.
// Phase 1.1 will sniff the diff for AddFunction / RenameSymbol /
// EditStatement patterns and emit those typed ops instead, with the
// round-trip-equality invariant enforced at extraction time.
func Extract(path string, pre, post []byte) (operation.Operation, error) {
	if path == "" {
		return nil, ErrEmptyPath
	}
	if pre == nil && post == nil {
		return nil, ErrEmpty
	}
	if pre == nil {
		// AddFile — copy content so caller mutations don't leak.
		content := make([]byte, len(post))
		copy(content, post)
		return &operation.AddFile{Path: path, Content: content}, nil
	}
	if post == nil {
		return &operation.DeleteFile{Path: path}, nil
	}
	if bytes.Equal(pre, post) {
		return nil, ErrNoChange
	}

	// Phase 1.1: attempt typed-op extraction before falling back to
	// RewriteRegion. tryTypedExtraction returns nil if no confident
	// match or if the round-trip validation fails — in either case
	// we fall through to the honest RewriteRegion fallback.
	if typed := tryTypedExtraction(path, pre, post); typed != nil {
		return typed, nil
	}

	return extractRewriteRegion(path, pre, post), nil
}

// extractRewriteRegion computes the minimal byte range in pre that
// must be replaced (with the corresponding content from post) to
// transform pre into post.
//
// Algorithm:
//  1. Find length of longest common prefix.
//  2. Find length of longest common suffix (not overlapping with prefix).
//  3. The range [prefix, len(pre)-suffix) in pre is replaced by the
//     range [prefix, len(post)-suffix) in post.
//
// This is the simplest correct extraction: it always round-trips
// through the projector (project(pre, op) == post by construction)
// and produces the smallest RewriteRegion that captures the change.
func extractRewriteRegion(path string, pre, post []byte) operation.Operation {
	prefix := commonPrefixLen(pre, post)
	suffix := commonSuffixLen(pre[prefix:], post[prefix:])

	preEnd := len(pre) - suffix
	postEnd := len(post) - suffix

	// Defensive: ensure the range is well-formed. Range{Start, End}
	// requires End > Start (Range.IsValid).
	if preEnd <= prefix {
		// pre is a strict prefix of post (pure append). The "range"
		// has zero width at position prefix; expand to a half-open
		// range that Validate accepts: insert at [prefix, prefix+1)
		// by including one byte of overlap, OR — cleaner — fall back
		// to a whole-file RewriteRegion if pre is too short.
		//
		// In practice this branch fires when pre is empty + post has
		// content, but that's caught earlier as AddFile. For non-nil
		// pre with pure-append change, use the minimal valid range:
		// [prefix-1, prefix) replaced by content. If prefix == 0,
		// fall back to whole-file.
		return wholeFileRewrite(path, pre, post)
	}

	content := make([]byte, postEnd-prefix)
	copy(content, post[prefix:postEnd])

	return &operation.RewriteRegion{
		Path:      path,
		ByteRange: operation.Range{Start: prefix, End: preEnd},
		Content:   content,
	}
}

// wholeFileRewrite returns a RewriteRegion covering all of pre,
// replaced by all of post. Used as the safe fallback when the
// minimal-range algorithm produces a degenerate range.
func wholeFileRewrite(path string, pre, post []byte) operation.Operation {
	// Range.IsValid requires Start < End. Empty pre means we can't
	// emit a valid RewriteRegion (range[0,0) is invalid). In that
	// case, callers should have routed to AddFile instead — this
	// is a defensive fallback.
	if len(pre) == 0 {
		// pre is empty (but not nil — would have been caught earlier).
		// Emit a Range[0, 1) replacing nothing with post. The
		// projector handler validates the range against pre's actual
		// length and will return ErrRangeOutOfBounds, surfacing the
		// shape mismatch. Better a loud error than silent corruption.
		content := make([]byte, len(post))
		copy(content, post)
		return &operation.RewriteRegion{
			Path:      path,
			ByteRange: operation.Range{Start: 0, End: 1},
			Content:   content,
		}
	}
	content := make([]byte, len(post))
	copy(content, post)
	return &operation.RewriteRegion{
		Path:      path,
		ByteRange: operation.Range{Start: 0, End: len(pre)},
		Content:   content,
	}
}

// commonPrefixLen returns the length of the longest byte sequence
// that is a prefix of both a and b.
func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// commonSuffixLen returns the length of the longest byte sequence
// that is a suffix of both a and b. Computed on slices that have
// already had the common prefix stripped, so the returned suffix
// length does not overlap with the prefix.
func commonSuffixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[len(a)-1-i] == b[len(b)-1-i] {
		i++
	}
	return i
}

// Diagnostic helper — sanity check that an extracted op describes a
// change consistent with the input. Not called automatically by
// Extract; production callers (per D5 policy) MUST validate via the
// projector that project(pre_state, op) == post_state before
// recording the op. This helper supports that validation.
//
// Phase 1.1: for typed ops beyond the v1.0 file-level set, delegates
// to the projector for round-trip verification — the projector is
// the authoritative materializer for all op kinds.
func ValidateRoundTrip(op operation.Operation, path string, pre, post []byte) error {
	// Cheap inline sanity for the v1.0 file-level op kinds.
	switch o := op.(type) {
	case *operation.AddFile:
		if pre != nil {
			return fmt.Errorf("extract.ValidateRoundTrip: AddFile but pre is non-nil")
		}
		if !bytes.Equal(o.Content, post) {
			return fmt.Errorf("extract.ValidateRoundTrip: AddFile.Content != post")
		}
		return nil
	case *operation.DeleteFile:
		if post != nil {
			return fmt.Errorf("extract.ValidateRoundTrip: DeleteFile but post is non-nil")
		}
		return nil
	case *operation.RewriteRegion:
		if pre == nil {
			return fmt.Errorf("extract.ValidateRoundTrip: RewriteRegion but pre is nil")
		}
		if o.ByteRange.End > len(pre) {
			return fmt.Errorf("extract.ValidateRoundTrip: RewriteRegion range past end of pre")
		}
		// Reconstruct: pre[:Start] || Content || pre[End:] should equal post.
		got := make([]byte, 0, len(pre)+len(o.Content)-o.ByteRange.End+o.ByteRange.Start)
		got = append(got, pre[:o.ByteRange.Start]...)
		got = append(got, o.Content...)
		got = append(got, pre[o.ByteRange.End:]...)
		if !bytes.Equal(got, post) {
			return fmt.Errorf("extract.ValidateRoundTrip: RewriteRegion does not reproduce post")
		}
		return nil
	}

	// Phase 1.1 typed ops: delegate to projector for round-trip check.
	if !validateRoundTrip(path, pre, op, post) {
		return fmt.Errorf("extract.ValidateRoundTrip: projector round-trip failed for %T", op)
	}
	return nil
}
