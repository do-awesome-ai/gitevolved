//go:build golden_print

// golden_print_test.go — one-off helper to dump canonical bytes for
// the TS port's parity tests. Skipped by default via build tag; run
// explicitly: go test -tags golden_print -run GoldenPrint -v.

package operation

import (
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func TestGoldenPrint(t *testing.T) {
	cases := []struct {
		name string
		op   Operation
	}{
		{"AddFile_hello", &AddFile{Path: "hello.go", Content: []byte("package hello\n")}},
		{"AddFile_empty_content", &AddFile{Path: "a.txt", Content: []byte{}}},
		{"AddFile_binary", &AddFile{Path: "x", Content: []byte{0x00, 0x01, 0xff, 0xfe}}},
		{"DeleteFile", &DeleteFile{Path: "old.go"}},
		{"RewriteRegion", &RewriteRegion{
			Path:      "a.go",
			ByteRange: Range{Start: 14, End: 21},
			Content:   []byte("fetch"),
		}},
		{"AddFile_with_quotes", &AddFile{Path: "q", Content: []byte("a\"b\\c\n")}},
		{"AddFile_with_html", &AddFile{Path: "h", Content: []byte("<div>&amp;</div>")}},
	}

	for _, c := range cases {
		env := &Envelope{
			SourceSession: "sess-golden",
			SourceTurn:    7,
			EmittedAt:     time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC),
		}
		if err := env.Seal(c.op); err != nil {
			t.Fatalf("Seal %s: %v", c.name, err)
		}
		fmt.Printf("=== %s ===\n", c.name)
		fmt.Printf("  body_hex: %s\n", hex.EncodeToString(env.Body))
		fmt.Printf("  body_str: %s\n", string(env.Body))
		fmt.Printf("  op_id:    %s\n", env.OpID)
	}
}
