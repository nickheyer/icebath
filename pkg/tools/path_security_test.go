package tools

import "testing"

func TestHasUnsafePathCharsExport(t *testing.T) {
	for _, p := range []string{"/tmp/a.mp3", "/tmp/a\nb.mp3", "/tmp/a\u2028b.mp3"} {
		if got, want := HasUnsafePathChars(p), hasUnsafePathChars(p); got != want {
			t.Fatalf("HasUnsafePathChars(%q) = %v, want %v", p, got, want)
		}
	}
}
