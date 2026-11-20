package agent

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Restores every relay cwd lookup after the test
func keepRelaySeams(t *testing.T) {
	recorded, scoped, sessionEnv, terminalEnv, resolve := relayRecordedSessionCwd, relayScopedSessionCwd, relaySessionEnv, relayTerminalEnv, relayResolveAgentCwd
	t.Cleanup(func() {
		relayRecordedSessionCwd, relayScopedSessionCwd, relaySessionEnv, relayTerminalEnv, relayResolveAgentCwd = recorded, scoped, sessionEnv, terminalEnv, resolve
	})
}

func relayAgent() *AIAgent { return &AIAgent{env: &tools.Env{Values: &sync.Map{}}} }

func relayPair(t *testing.T, a *AIAgent, platform string) [2]string {
	t.Helper()
	session, turn := resolveRelayScopeCwds(a, "task-1", "session-1", platform)
	return [2]string{*session, *turn}
}

func TestSessionContextAndTaskRecordRemainDistinct(t *testing.T) {
	keepRelaySeams(t)
	a := relayAgent()
	tools.SetLogicalSessionCwd(a.env, "/workspace/session")
	tools.RecordSessionCwd(a.env, "task-1", "/workspace/task")
	t.Cleanup(func() { tools.ClearSessionCwd(a.env, "task-1") })
	relaySessionEnv = func(*tools.Env, string, string) (string, error) { return "", nil }
	if got := relayPair(t, a, "cli"); got != [2]string{"/workspace/session", "/workspace/task"} {
		t.Fatalf("cli %v", got)
	}
	if got := relayPair(t, a, "subagent"); got != [2]string{"/workspace/task", "/workspace/task"} {
		t.Fatalf("subagent %v", got)
	}
}

func TestRemoteScopedCwdIsPreservedWithoutHostResolution(t *testing.T) {
	for _, logical := range []string{"~/remote-project", `C:\work\project`, `\\server\share\project`} {
		t.Run(logical, func(t *testing.T) {
			keepRelaySeams(t)
			a := relayAgent()
			tools.SetLogicalSessionCwd(a.env, logical)
			relayRecordedSessionCwd = func(*tools.Env, string) (any, error) { return nil, nil }
			relayResolveAgentCwd = func(*tools.Env) (string, error) { panic("must not resolve on this host") }
			if got := relayPair(t, a, "gateway"); got != [2]string{logical, logical} {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestGatewaySessionKeyUsesItsRecordedCwd(t *testing.T) {
	keepRelaySeams(t)
	a := relayAgent()
	tools.RecordSessionCwd(a.env, "gateway-key", "/remote/gateway-workspace")
	t.Cleanup(func() { tools.ClearSessionCwd(a.env, "gateway-key") })
	relaySessionEnv = func(_ *tools.Env, name, def string) (string, error) {
		if name == "ICEBATH_SESSION_KEY" {
			return "gateway-key", nil
		}
		return def, nil
	}
	relayResolveAgentCwd = func(*tools.Env) (string, error) { panic("must not resolve on this host") }
	if got := relayPair(t, a, "gateway"); got != [2]string{"/remote/gateway-workspace", "/remote/gateway-workspace"} {
		t.Fatalf("got %v", got)
	}
}

func TestOnlyLocalCliUsesHostCwd(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		platform, backend, resolved, expected string
		usesHost                              bool
	}{
		{"cli", "local", "/workspace", "/workspace", true},
		{"", "local", ".", wd, true},
		{"cli", "ssh", "/host", "", false},
		{"gateway", "local", "/host", "", false},
	}
	for _, c := range cases {
		t.Run(c.platform+"/"+c.backend, func(t *testing.T) {
			keepRelaySeams(t)
			lookups := 0
			relayRecordedSessionCwd = func(*tools.Env, string) (any, error) { return nil, nil }
			relaySessionEnv = func(_ *tools.Env, _, def string) (string, error) { return def, nil }
			relayTerminalEnv = func(_ *tools.Env, name, def string) (string, error) {
				if name == "TERMINAL_ENV" {
					return c.backend, nil
				}
				return def, nil
			}
			relayResolveAgentCwd = func(*tools.Env) (string, error) { lookups++; return c.resolved, nil }
			if got := relayPair(t, relayAgent(), c.platform); got != [2]string{c.expected, c.expected} {
				t.Fatalf("got %v", got)
			}
			if (lookups > 0) != c.usesHost {
				t.Fatalf("host lookups %d", lookups)
			}
		})
	}
}

func TestCwdLookupFailuresDoNotBreakTheTurn(t *testing.T) {
	keepRelaySeams(t)
	unavailable := errors.New("unavailable")
	relayRecordedSessionCwd = func(*tools.Env, string) (any, error) { return nil, unavailable }
	relayScopedSessionCwd = func(*tools.Env) (any, error) { return nil, unavailable }
	relaySessionEnv = func(*tools.Env, string, string) (string, error) { return "", unavailable }
	relayTerminalEnv = func(*tools.Env, string, string) (string, error) { return "", unavailable }
	if got := relayPair(t, &AIAgent{}, "gateway"); got != [2]string{"", ""} {
		t.Fatalf("got %v", got)
	}
}
