package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolCallsReadTheClientEnvTheirTurnBound(t *testing.T) {
	prevName, prevSubs := state.SecretEnvName, state.SecretEnvSubstrings
	state.SecretEnvName = func(name string) bool { return strings.Contains(strings.ToUpper(name), "TOKEN") }
	state.SecretEnvSubstrings = []string{"TOKEN"}
	t.Cleanup(func() { state.SecretEnvName, state.SecretEnvSubstrings = prevName, prevSubs })
	a := &AIAgent{env: &tools.Env{Home: t.TempDir(), Values: &sync.Map{}}}
	if got := tools.SessionToolEnv(a.env); len(got) != 0 {
		t.Fatalf("unbound env leaked %v", got)
	}
	ctx := state.ClientEnvScope(context.Background(), map[string]string{"PATH": "/client/bin", "GOPATH": "/client/go"}, true, nil)
	bindTurnClientEnv(ctx, a)
	if got := tools.SessionToolEnv(a.env); got["PATH"] != "/client/bin" || got["GOPATH"] != "/client/go" {
		t.Fatalf("tool env %v", got)
	}
	if got := tools.SessionToolchainEnviron(a.env, map[string]string{"PATH": "/daemon/bin", "HOME": "/daemon"}); got["PATH"] != "/client/bin" {
		t.Fatalf("toolchain environ %v", got)
	}
	remote := state.ClientEnvScope(context.Background(), map[string]string{"PATH": "/remote/bin"}, false, nil)
	bindTurnClientEnv(remote, a)
	if got := tools.SessionToolEnv(a.env); len(got) != 0 {
		t.Fatalf("remote client toolchain travelled %v", got)
	}
	bindTurnClientEnv(context.Background(), a)
	if got := tools.SessionToolchainEnviron(a.env, map[string]string{"PATH": "/daemon/bin"}); got["PATH"] != "/daemon/bin" {
		t.Fatalf("unbound turn kept the previous client env %v", got)
	}
}
