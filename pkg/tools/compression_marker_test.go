package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Limits passed to elide during a test, reset by the test that reads them
var elideCalls []int

func init() { elideObserver = func(limit int) { elideCalls = append(elideCalls, limit) } }

func TestCompressionMarkerMintedIsCaughtByDispatchGuard(t *testing.T) {
	for _, out := range []string{elide(strings.Repeat("y", 4000), 199), elideMiddle(strings.Repeat("y", 4000), 100, 100)} {
		if !compressionMarkerRe.MatchString(out) || !compressionMarkerArtifactRe.MatchString(out) {
			t.Fatalf("marker not recognised in %q", out)
		}
	}
	legacy := "⟪HERMES-CONTEXT-COMPRESSION: 1,000 of 2,000 chars omitted here by Hermes's context compressor.⟫"
	if !compressionMarkerRe.MatchString(legacy) || !compressionMarkerArtifactRe.MatchString(legacy) {
		t.Fatal("legacy marker must still match")
	}
}

func TestElideNeverExceedsLimit(t *testing.T) {
	text := strings.Repeat("é ", 3000)
	for _, limit := range []int{10, 199, 500, 5999, 6000, 7000} {
		out := elide(text, limit)
		n := utf8.RuneCountInString(out)
		if limit >= 6000 && out != text {
			t.Fatalf("limit %d must return the text unchanged", limit)
		}
		if limit < 6000 && n > limit && limit >= elisionMarkerMaxLen {
			t.Fatalf("limit %d produced %d chars", limit, n)
		}
		if limit < 6000 && !strings.HasSuffix(out, "⟫") {
			t.Fatalf("limit %d lost the marker: %q", limit, out)
		}
	}
	if got := elide("abc", 3); got != "abc" {
		t.Fatalf("fits %q", got)
	}
	if got := elide("abcd", 1); got != elisionMarker(4, 4) {
		t.Fatalf("marker alone %q", got)
	}
	if pyCommaInt(1234567) != "1,234,567" || pyCommaInt(999) != "999" || pyCommaInt(1000) != "1,000" {
		t.Fatal("comma formatting")
	}
}

func TestElideMiddle(t *testing.T) {
	text := strings.Repeat("a", 50) + strings.Repeat("b", 50)
	out := elideMiddle(text, 10, 10)
	if !strings.HasPrefix(out, strings.Repeat("a", 10)) || !strings.HasSuffix(out, strings.Repeat("b", 10)) || !strings.Contains(out, "80 of 100 chars omitted") {
		t.Fatalf("%q", out)
	}
	if elideMiddle("short", 10, 10) != "short" {
		t.Fatal("fits")
	}
}
