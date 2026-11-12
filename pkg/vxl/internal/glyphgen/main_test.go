package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedTableIsTheCommittedGlyphsGo(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "glyphs.go"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatal("generated glyph table differs from pkg/vxl/glyphs.go")
	}
}
