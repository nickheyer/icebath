package profiles

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestProfileNamesHookListsTheDefaultProfile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if observability.ProfileNamesHook == nil {
		t.Fatal("ProfileNamesHook is not set")
	}
	names := observability.ProfileNamesHook(context.Background())
	if len(names) == 0 || names[0] != "default" {
		t.Fatalf("profile names = %q, want default first", names)
	}
}
