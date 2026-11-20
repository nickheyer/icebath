package commands

import (
	"slices"
	"testing"
)

func TestCompressFlagsCompactResolvesToCompress(t *testing.T) {
	cmd := Resolve("compact")
	if cmd == nil || cmd.Name != "compress" || !slices.Contains(cmd.Aliases, "compact") {
		t.Fatalf("compact resolved to %+v", cmd)
	}
}
