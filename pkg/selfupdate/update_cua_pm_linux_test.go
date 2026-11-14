package selfupdate

import (
	"context"
	"io"
	"slices"
	"testing"
)

func TestLinuxRefreshReconcilesInstalledPin(t *testing.T) {
	calls := cuaRefreshFixture(t, true, true)
	if err := refreshCuaDriverAfterUpdate(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls.lookups, []bool{true}) || !slices.Equal(calls.ensures, []string{"cua-driver"}) || len(calls.hostSetups) != 0 {
		t.Fatalf("%+v", calls)
	}
}
