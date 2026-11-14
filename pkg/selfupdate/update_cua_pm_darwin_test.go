package selfupdate

import (
	"context"
	"io"
	"slices"
	"testing"
)

func TestMacosRefreshRegistersPostEnsureSignedApp(t *testing.T) {
	calls := cuaRefreshFixture(t, true, true)
	if err := refreshCuaDriverAfterUpdate(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls.lookups, []bool{true}) || len(calls.ensures) != 0 || !slices.Equal(calls.hostSetups, []bool{false}) {
		t.Fatalf("%+v", calls)
	}
}
