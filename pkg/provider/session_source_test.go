package provider

import (
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func sessionSourceTestEnv(t *testing.T, vars map[string]string) *tools.Env {
	t.Helper()
	for _, name := range []string{"ICEBATH_SESSION_SOURCE", "ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_SESSION_SOURCE_EXPLICIT"} {
		t.Setenv(name, "")
	}
	env := &tools.Env{Values: &sync.Map{}}
	if vars != nil {
		tools.BindSessionContext(env, vars)
	}
	return env
}

func TestSessionSourceContextOverridesPlatform(t *testing.T) {
	env := sessionSourceTestEnv(t, map[string]string{"ICEBATH_SESSION_SOURCE": "tool"})
	if got := SessionSourceFor(env, "tui"); got != "tool" {
		t.Fatalf("got %q", got)
	}
}

func TestSessionSourceFallsBackToPlatform(t *testing.T) {
	env := sessionSourceTestEnv(t, nil)
	if got := SessionSourceFor(env, "tui"); got != "tui" {
		t.Fatalf("got %q", got)
	}
	if got := SessionSourceFor(nil, ""); got != "cli" {
		t.Fatalf("library caller got %q", got)
	}
}

func TestOneshotRunGetsDistinctSource(t *testing.T) {
	for _, inherited := range []string{"", "tui", "desktop"} {
		t.Run(inherited, func(t *testing.T) {
			env := sessionSourceTestEnv(t, nil)
			t.Setenv("ICEBATH_SESSION_SOURCE", inherited)
			t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
			if got := SessionSourceFor(env, "cli"); got != "oneshot" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestOneshotMarkerDoesNotRelabelSubagents(t *testing.T) {
	env := sessionSourceTestEnv(t, nil)
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	if got := SessionSourceFor(env, "subagent"); got != "subagent" {
		t.Fatalf("got %q", got)
	}
}

func TestOneshotChildKeepsInheritedAutomationSource(t *testing.T) {
	for _, inherited := range []string{"kanban", "tool", "a2a"} {
		t.Run(inherited, func(t *testing.T) {
			env := sessionSourceTestEnv(t, nil)
			t.Setenv("ICEBATH_SESSION_SOURCE", inherited)
			t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
			if got := SessionSourceFor(env, "cli"); got != inherited {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestOneshotKeepsExplicitSourceFlag(t *testing.T) {
	for _, explicit := range []string{"tui", "desktop"} {
		t.Run(explicit, func(t *testing.T) {
			env := sessionSourceTestEnv(t, nil)
			t.Setenv("ICEBATH_SESSION_SOURCE", explicit)
			t.Setenv("ICEBATH_SESSION_SOURCE_EXPLICIT", "1")
			t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
			if got := SessionSourceFor(env, "cli"); got != explicit {
				t.Fatalf("got %q", got)
			}
		})
	}
}
