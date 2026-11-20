package tools

import (
	"slices"
	"testing"
)

func TestRestoreLastResolvedToolNames(t *testing.T) {
	saved := LastResolvedToolNames()
	defer RestoreLastResolvedToolNames(saved)
	RestoreLastResolvedToolNames([]string{"terminal", "read_file"})
	if got := LastResolvedToolNames(); !slices.Equal(got, []string{"terminal", "read_file"}) {
		t.Fatalf("names %v", got)
	}
}
