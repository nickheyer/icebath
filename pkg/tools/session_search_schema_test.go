package tools

import (
	"strings"
	"testing"
)

func TestSessionSearchSchemaNamesTheProduct(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	desc, _ := sessionSearchSchema["description"].(string)
	if !strings.HasPrefix(desc, "Search or read past Icebath conversations (FTS5), or scroll inside one.") {
		t.Fatalf("description %q", desc)
	}
}
