package pyshutil

import "testing"

func TestGetTerminalSizeEnvFirst(t *testing.T) {
	t.Setenv("COLUMNS", "133")
	t.Setenv("LINES", "41")
	if c, l := GetTerminalSize(120, 24); c != 133 || l != 41 {
		t.Fatalf("got %d %d", c, l)
	}
}

func TestGetTerminalSizeFallback(t *testing.T) {
	t.Setenv("COLUMNS", "nope")
	t.Setenv("LINES", "-3")
	c, l := GetTerminalSize(120, 24)
	if c <= 0 || l <= 0 {
		t.Fatalf("got %d %d", c, l)
	}
}

func TestEnvDimension(t *testing.T) {
	t.Setenv("COLUMNS", " 1_00 ")
	if n := envDimension("COLUMNS"); n != 100 {
		t.Fatalf("got %d", n)
	}
	if n := envDimension("ICEBATH_UNSET_DIMENSION"); n != 0 {
		t.Fatalf("got %d", n)
	}
}
