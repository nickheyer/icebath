package agent

import (
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func attendedTestAgent(t *testing.T, platform string, vars map[string]string) *AIAgent {
	t.Helper()
	for _, name := range []string{"ICEBATH_SESSION_SOURCE", "ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_SESSION_SOURCE_EXPLICIT"} {
		t.Setenv(name, "")
	}
	env := &tools.Env{Values: &sync.Map{}}
	if vars != nil {
		tools.BindSessionContext(env, vars)
	}
	return &AIAgent{platform: platform, env: env}
}

func TestSessionSourceSets(t *testing.T) {
	for _, src := range []string{"cli", "oneshot"} {
		if !CLIFamilySources[src] {
			t.Fatalf("cli family lacks %s", src)
		}
	}
	if len(CLIFamilySources) != 2 || len(DaybreakSources) != 3 || len(AttendedSources) != 4 {
		t.Fatalf("set sizes %d %d %d", len(CLIFamilySources), len(DaybreakSources), len(AttendedSources))
	}
	for _, src := range []string{"tui", "desktop", "cli"} {
		if !DaybreakSources[src] || !AttendedSources[src] {
			t.Fatalf("%s missing", src)
		}
	}
	if DaybreakSources["acp"] || !AttendedSources["acp"] || DaybreakSources["telegram"] || AttendedSources["oneshot"] {
		t.Fatal("acp, telegram or oneshot misplaced")
	}
}

func TestCreateSurface(t *testing.T) {
	cases := map[string]string{"desktop": "gui", "tui": "tui", "cli": "cli", "": "", "telegram": "telegram"}
	for in, want := range cases {
		if got := CreateSurface(in); got != want {
			t.Fatalf("CreateSurface(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsAttended(t *testing.T) {
	if IsAttended(nil) || IsAttended(attendedTestAgent(t, "", nil)) || IsAttended(attendedTestAgent(t, "subagent", nil)) {
		t.Fatal("library caller or subagent reported attended")
	}
	for _, platform := range []string{"cli", "tui", "desktop", "acp"} {
		if !IsAttended(attendedTestAgent(t, platform, nil)) {
			t.Fatalf("%s not attended", platform)
		}
	}
	for _, platform := range []string{"telegram", "cron", "batch"} {
		if IsAttended(attendedTestAgent(t, platform, nil)) {
			t.Fatalf("%s attended", platform)
		}
	}
	if IsAttended(attendedTestAgent(t, "cli", map[string]string{"ICEBATH_SINGLE_QUERY_SESSION": "1"})) {
		t.Fatal("oneshot cli run attended")
	}
	if IsAttended(attendedTestAgent(t, "tui", map[string]string{"ICEBATH_SESSION_SOURCE": "cron"})) {
		t.Fatal("cron sourced session attended")
	}
	if !IsAttended(attendedTestAgent(t, "telegram", map[string]string{"ICEBATH_SESSION_SOURCE": "tui"})) {
		t.Fatal("tui sourced session not attended")
	}
}

func TestIsAttendedFiniteGatewayTurn(t *testing.T) {
	prev := tools.FiniteTurnRequired
	defer func() { tools.FiniteTurnRequired = prev }()
	tools.FiniteTurnRequired = func(*tools.Env) (bool, bool) { return true, true }
	if IsAttended(attendedTestAgent(t, "cli", nil)) {
		t.Fatal("finite gateway turn attended")
	}
	tools.FiniteTurnRequired = func(*tools.Env) (bool, bool) { return false, true }
	if !IsAttended(attendedTestAgent(t, "cli", nil)) {
		t.Fatal("non finite turn not attended")
	}
	tools.FiniteTurnRequired = func(*tools.Env) (bool, bool) { return false, false }
	if !IsAttended(attendedTestAgent(t, "tui", nil)) {
		t.Fatal("unknown finite flag not attended")
	}
}
