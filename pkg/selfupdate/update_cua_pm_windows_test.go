package selfupdate

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestWindowsRefreshDefersPinAndUacToExplicitSetup(t *testing.T) {
	calls := cuaRefreshFixture(t, true, true)
	var out bytes.Buffer
	if err := refreshCuaDriverAfterUpdate(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if len(calls.ensures) != 0 || len(calls.hostSetups) != 0 {
		t.Fatalf("%+v", calls)
	}
	text := out.String()
	if !strings.Contains(strings.ToLower(text), "deferred") || !strings.Contains(text, "UAC") || !strings.Contains(text, "icebath computer-use install --upgrade") {
		t.Fatalf("output %q", text)
	}
}
