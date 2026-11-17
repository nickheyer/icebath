package tools

import (
	"context"
	"sync"
	"testing"
)

func TestGatewayTokenCleaning(t *testing.T) {
	isolateSessionContext(t, false)
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	env := &Env{Home: home, Values: &sync.Map{}}
	for in, want := range map[any]string{"  tok \n": "tok", "   ": "", 7: "", nil: ""} {
		if got := cleanGatewayValue(in); got != want {
			t.Fatalf("clean %#v = %q", in, got)
		}
	}
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "  tok  ")
	if got := readUserTokenOverride(context.Background(), env); got != "tok" {
		t.Fatalf("override %q", got)
	}
}
