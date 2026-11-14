package tools

import (
	"errors"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Replaces the relay lookups for one test
func withRelayLookups(t *testing.T, record func(*Env, string) (string, bool), sessionEnv func(*Env, string) string,
	terminal func(*Env, string, string) (string, error), resolve func(*Env) (string, error)) {
	prevRecord, prevScoped, prevSession, prevTerminal, prevResolve := relaySessionCwdRecord, relayScopedSessionCwd, relaySessionEnv, relayTerminalEnv, relayResolveAgentCwd
	if record != nil {
		relaySessionCwdRecord = record
	}
	if sessionEnv != nil {
		relaySessionEnv = sessionEnv
	}
	if terminal != nil {
		relayTerminalEnv = terminal
	}
	if resolve != nil {
		relayResolveAgentCwd = resolve
	}
	t.Cleanup(func() {
		relaySessionCwdRecord, relayScopedSessionCwd, relaySessionEnv, relayTerminalEnv, relayResolveAgentCwd = prevRecord, prevScoped, prevSession, prevTerminal, prevResolve
	})
}

func noRecord(*Env, string) (string, bool) { return "", false }
func emptySessionEnv(*Env, string) string  { return "" }
func forbiddenResolve(t *testing.T) func(*Env) (string, error) {
	return func(*Env) (string, error) { t.Fatal("must not resolve on this host"); return "", nil }
}

func assertRelay(t *testing.T, env *Env, platform, wantSession, wantTurn string) {
	t.Helper()
	if s, tc := ResolveRelayScopeCwds(env, nil, "task-1", "session-1", platform); s != wantSession || tc != wantTurn {
		t.Fatalf("%s -> (%q, %q), want (%q, %q)", platform, s, tc, wantSession, wantTurn)
	}
}

// The scoped session cwd and the task record stay distinct except for subagent and cron turns
func TestSessionContextAndTaskRecordRemainDistinct(t *testing.T) {
	env := newTestToolEnv(t)
	SetLogicalSessionCwd(env, "/workspace/session")
	RecordSessionCwd(env, "task-1", "/workspace/task")
	t.Cleanup(func() { ClearSessionCwd(env, "task-1") })
	withRelayLookups(t, nil, emptySessionEnv, nil, nil)
	assertRelay(t, env, "cli", "/workspace/session", "/workspace/task")
	assertRelay(t, env, "subagent", "/workspace/task", "/workspace/task")
}

// Remote scoped cwds are preserved as declared without host resolution
func TestRemoteScopedCwdIsPreservedWithoutHostResolution(t *testing.T) {
	for _, logical := range []string{"~/remote-project", `C:\work\project`, `\\server\share\project`} {
		env := newTestToolEnv(t)
		SetLogicalSessionCwd(env, logical)
		withRelayLookups(t, noRecord, nil, nil, forbiddenResolve(t))
		assertRelay(t, env, "gateway", logical, logical)
	}
}

// A gateway session key resolves through its recorded cwd
func TestGatewaySessionKeyUsesItsRecordedCwd(t *testing.T) {
	env := newTestToolEnv(t)
	SetLogicalSessionCwd(env, "")
	RecordSessionCwd(env, "gateway-key", "/remote/gateway-workspace")
	t.Cleanup(func() { ClearSessionCwd(env, "gateway-key") })
	withRelayLookups(t, nil, func(_ *Env, name string) string {
		if name == "ICEBATH_SESSION_KEY" {
			return "gateway-key"
		}
		return ""
	}, nil, forbiddenResolve(t))
	assertRelay(t, env, "gateway", "/remote/gateway-workspace", "/remote/gateway-workspace")
}

// Only a local cli turn falls back to the host cwd
func TestOnlyLocalCliUsesHostCwd(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwdResolved, err := pypath.Resolve(cwd)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := pypath.Resolve("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		platform, backend, resolved, expected string
		usesHost                              bool
	}{
		{"cli", "local", "/workspace", workspace, true},
		{"", "local", ".", cwdResolved, true},
		{"cli", "ssh", "/host", "", false},
		{"gateway", "local", "/host", "", false},
	} {
		env := newTestToolEnv(t)
		lookups := 0
		withRelayLookups(t, noRecord, emptySessionEnv, func(_ *Env, name, def string) (string, error) {
			if name == "TERMINAL_ENV" {
				return c.backend, nil
			}
			return def, nil
		}, func(*Env) (string, error) { lookups++; return c.resolved, nil })
		assertRelay(t, env, c.platform, c.expected, c.expected)
		if (lookups > 0) != c.usesHost {
			t.Fatalf("%+v host lookups %d", c, lookups)
		}
	}
}

// Failing cwd lookups leave both cwds empty instead of breaking the turn
func TestCwdLookupFailuresDoNotBreakTheTurn(t *testing.T) {
	env := newTestToolEnv(t)
	withRelayLookups(t, func(*Env, string) (string, bool) { panic("unavailable") },
		func(*Env, string) string { panic("unavailable") },
		func(*Env, string, string) (string, error) { return "", errors.New("unavailable") }, nil)
	relayScopedSessionCwd = func(*Env) string { panic("unavailable") }
	assertRelay(t, env, "gateway", "", "")
}
