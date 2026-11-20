package state

import "testing"

func TestShapePreviewMatchesTheUnexportedShaper(t *testing.T) {
	for _, raw := range []any{nil, "hello world", []byte("bytes"), 42, false} {
		if got, want := ShapePreview(raw), shapePreview(raw); got != want {
			t.Fatalf("ShapePreview(%v) = %q, want %q", raw, got, want)
		}
	}
	if ShapePreview(nil) != "" || ShapePreview("plain") != "plain" {
		t.Fatalf("nil and plain previews: %q %q", ShapePreview(nil), ShapePreview("plain"))
	}
}
