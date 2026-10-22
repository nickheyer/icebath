package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/rules"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
	"github.com/nickheyer/icebath/pkg/tool/shell"
)

// App over a project directory and a user directory with an empty policy, a registry and a server manager
func editorApp(t *testing.T) (*App, string, string) {
	t.Helper()
	user := t.TempDir()
	project := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, user)
	pol, err := rules.New()
	if err != nil {
		t.Fatal(err)
	}
	reg := &tool.Registry{}
	reg.Add(tool.Func{Name: "Read", Parameters: []byte(`{"type":"object"}`)})
	a := &App{Dir: project, Rules: pol, Tools: reg, MCP: NewMCP(func(any) {}, reg)}
	t.Cleanup(a.MCP.Close)
	return a, user, project
}

func TestScopesNameTheirFiles(t *testing.T) {
	a, user, project := editorApp(t)
	if a.ScopeOf(filepath.Join(user, RulesFile)) != "user" || a.ScopeOf(filepath.Join(project, ProjectDir, HooksFile)) != "project" || a.ScopeOf("/etc/extra.json") != "/etc/extra.json" {
		t.Fatal("scopes")
	}
	a.Config.Sources = []string{"/x/extra.json", filepath.Join(user, HooksFile)}
	got, err := a.files(RulesFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(user, ConfigFile), filepath.Join(project, ProjectDir, ConfigFile), filepath.Join(user, RulesFile), filepath.Join(project, ProjectDir, RulesFile), "/x/extra.json"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rule files in read order without hook files\n%v", got)
	}
	if _, err := a.scopeDir("elsewhere"); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

func TestRulesAreEditedInTheFileHoldingThem(t *testing.T) {
	a, user, project := editorApp(t)
	cfgPath := filepath.Join(user, ConfigFile)
	os.WriteFile(cfgPath, []byte(`{"permissions":{"deny":["Bash(rm *)"]},"model":"m"}`), 0o644)
	if err := a.ReloadRules(); err != nil {
		t.Fatal(err)
	}
	if got := a.Rules.Rules(policy.Deny); len(got) != 1 || got[0] != "Bash(rm *)" {
		t.Fatalf("rules of the config file are in force %v", got)
	}
	if err := a.AddRule(ScopeProject, policy.Allow, "Read(src/**)"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddRule(ScopeProject, policy.Allow, "Read(src/**)"); err != nil {
		t.Fatal(err)
	}
	projectRules := filepath.Join(project, ProjectDir, RulesFile)
	set, err := rules.LoadFile(projectRules)
	if err != nil || len(set.Allow) != 1 || set.Allow[0] != "Read(src/**)" {
		t.Fatalf("a new rule lands once in the project rules file %+v %v", set, err)
	}
	entries, err := a.RuleEntries()
	if err != nil || len(entries) != 2 || entries[0].Path != cfgPath || entries[1].Path != projectRules {
		t.Fatalf("entries carry their file %+v %v", entries, err)
	}
	if err := a.ReplaceRule(entries[0], policy.Ask, "Bash(rm -rf *)"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), `"ask": [`) || !strings.Contains(string(data), `"Bash(rm -rf *)"`) || strings.Contains(string(data), `"deny"`) || strings.Contains(string(data), `null`) || !strings.Contains(string(data), `"model": "m"`) {
		t.Fatalf("an edit stays in the config file and keeps its other keys\n%s", data)
	}
	if got := a.Rules.Rules(policy.Ask); len(got) != 1 {
		t.Fatalf("the edit is in force %v", got)
	}
	entries, _ = a.RuleEntries()
	for _, e := range entries {
		if err := a.RemoveRule(e); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(cfgPath); strings.Contains(string(data), "permissions") {
		t.Fatalf("removing the last rule drops the block\n%s", data)
	}
	if len(a.Config.Permissions.Allow)+len(a.Config.Permissions.Ask)+len(a.Config.Permissions.Deny) != 0 || len(a.Rules.Rules(policy.Allow)) != 0 {
		t.Fatal("removed rules leave force")
	}
	if err := a.AddRule(ScopeUser, "maybe", "Read"); err == nil {
		t.Fatal("unknown decision accepted")
	}
	if err := a.AddRule(ScopeUser, policy.Allow, "Read("); err == nil {
		t.Fatal("malformed rule accepted")
	}
	if err := a.ReplaceRule(RuleEntry{Path: cfgPath}, policy.Allow, ""); err == nil {
		t.Fatal("empty rule accepted")
	}
	if err := a.RemoveRule(RuleEntry{Decision: policy.Allow, Rule: "Glob", Path: cfgPath}); err == nil || !strings.Contains(err.Error(), "no longer holds") {
		t.Fatalf("a rule already gone is reported %v", err)
	}
	if err := a.ReplaceRule(RuleEntry{Decision: policy.Allow, Rule: "Glob", Path: cfgPath}, policy.Deny, "Glob"); err == nil {
		t.Fatal("replacing a rule already gone is reported")
	}
	os.WriteFile(projectRules, []byte(`{`), 0o644)
	if _, err := a.RuleEntries(); err == nil || !strings.Contains(err.Error(), projectRules) {
		t.Fatalf("a broken file is named %v", err)
	}
}

func TestHooksAreEditedInTheFileHoldingThem(t *testing.T) {
	a, user, project := editorApp(t)
	cfgPath := filepath.Join(project, ProjectDir, ConfigFile)
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"hooks":{"Stop":[{"hooks":[{"command":"say done"}]}]}}`), 0o644)
	if err := a.ReloadHooks(); err != nil || len(a.Config.Hooks[hooks.Stop]) != 1 {
		t.Fatalf("hooks of a config file are in force %v %v", a.Config.Hooks, err)
	}
	add := HookEntry{Event: hooks.PreToolUse, Matcher: "Bash", Command: "check.sh", Timeout: 5}
	if err := a.AddHook(ScopeUser, add); err != nil {
		t.Fatal(err)
	}
	if err := a.AddHook(ScopeUser, HookEntry{Event: hooks.PreToolUse, Matcher: "Bash", Command: "audit.sh"}); err != nil {
		t.Fatal(err)
	}
	userHooks := filepath.Join(user, HooksFile)
	cfg, err := hooks.LoadFile(userHooks)
	if err != nil || len(cfg[hooks.PreToolUse]) != 1 || len(cfg[hooks.PreToolUse][0].Hooks) != 2 {
		t.Fatalf("hooks sharing a matcher share a group %+v %v", cfg, err)
	}
	entries, err := a.HookEntries()
	if err != nil || len(entries) != 3 || entries[0].Path != cfgPath || entries[1] != (HookEntry{Event: hooks.PreToolUse, Matcher: "Bash", Command: "check.sh", Timeout: 5, Path: userHooks}) {
		t.Fatalf("entries in read order with their file %+v %v", entries, err)
	}
	moved := HookEntry{Event: hooks.PostToolUse, Matcher: "Edit|Write", Command: "fmt.sh"}
	if err := a.ReplaceHook(entries[1], moved); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := hooks.LoadFile(userHooks); len(cfg[hooks.PostToolUse]) != 1 || len(cfg[hooks.PreToolUse][0].Hooks) != 1 {
		t.Fatalf("a replaced hook moves to its new event %+v", cfg)
	}
	if len(a.Config.Hooks[hooks.PostToolUse]) != 1 || len(a.Config.Hooks[hooks.Stop]) != 1 {
		t.Fatalf("hooks in force follow the files %+v", a.Config.Hooks)
	}
	if err := a.RemoveHook(entries[0]); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(cfgPath); strings.Contains(string(data), "hooks") {
		t.Fatalf("removing the last hook of a config file drops the block\n%s", data)
	}
	if err := a.RemoveHook(entries[0]); err == nil {
		t.Fatal("a hook already gone is reported")
	}
	for _, bad := range []HookEntry{{Event: "Never", Command: "x"}, {Event: hooks.Stop}, {Event: hooks.PreToolUse, Command: "x", Matcher: "("}, {Event: hooks.Stop, Command: "x", Timeout: -1}} {
		if err := a.AddHook(ScopeProject, bad); err == nil {
			t.Fatalf("bad hook accepted %+v", bad)
		}
	}
	if CheckMatcher("a|b") != nil || CheckMatcher("[") == nil {
		t.Fatal("matcher check")
	}
}

func TestServersAreEditedInTheFileConfiguringThem(t *testing.T) {
	a, user, project := editorApp(t)
	ctx := context.Background()
	userCfg := filepath.Join(user, ConfigFile)
	os.WriteFile(userCfg, []byte(`{"mcp":[{"name":"files","command":"serve-files","disabled":true}]}`), 0o644)
	entries, err := a.ServerEntries()
	if err != nil || len(entries) != 1 || entries[0].Path != userCfg {
		t.Fatalf("servers carry their file %+v %v", entries, err)
	}
	web := ServerConfig{Name: "web", URL: "http://127.0.0.1:1/mcp", Disabled: true}
	if err := a.SaveServer(ScopeProject, "", web); err != nil {
		t.Fatal(err)
	}
	projectCfg := filepath.Join(project, ProjectDir, ConfigFile)
	if list, _ := readServers(projectCfg); len(list) != 1 || list[0].Name != "web" {
		t.Fatalf("a new server lands in the project config %+v", list)
	}
	if len(a.MCP.Status()) != 0 {
		t.Fatal("saving leaves the live servers to the restart")
	}
	if err := a.RestartServer(ctx, "", web); err != nil {
		t.Fatal(err)
	}
	if st := a.MCP.Status(); len(st) != 1 || st[0].Name != "web" || st[0].State != mcp.StateDisconnected {
		t.Fatalf("a server turned off is managed without connecting %+v", st)
	}
	if err := a.SaveServer(ScopeProject, "", ServerConfig{Name: "web", URL: "http://x"}); err == nil {
		t.Fatal("a second server of the same name accepted")
	}
	if err := a.SaveServer(ScopeProject, "", ServerConfig{Name: "both", Command: "x", URL: "http://x"}); err == nil {
		t.Fatal("a server with two transports accepted")
	}
	if err := a.SaveServer(ScopeProject, "", ServerConfig{Command: "x"}); err == nil {
		t.Fatal("an unnamed server accepted")
	}
	if err := a.SaveServer(ScopeUser, "files", ServerConfig{Name: "disk", Command: "serve-disk", Args: []string{"--root", "/"}, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if list, _ := readServers(userCfg); len(list) != 1 || list[0].Name != "disk" || list[0].Args[1] != "/" {
		t.Fatalf("an edit renames in place in the file configuring it %+v", list)
	}
	if err := a.SaveServer(ScopeUser, "ghost", ServerConfig{Name: "ghost2", Command: "x"}); err == nil {
		t.Fatal("editing a server no file configures accepted")
	}
	cfg, err := a.SetServerEnabled("web", false)
	if err != nil || !cfg.Disabled {
		t.Fatal(err)
	}
	if list, _ := readServers(projectCfg); !list[0].Disabled {
		t.Fatal("turning a server off is written")
	}
	if _, err := a.SetServerEnabled("ghost", true); err == nil {
		t.Fatal("switching a missing server accepted")
	}
	if len(a.Config.MCP) != 2 {
		t.Fatalf("the configuration keeps the server list %+v", a.Config.MCP)
	}
	if err := a.RemoveServer("web"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(projectCfg); strings.Contains(string(data), "mcp") {
		t.Fatalf("removing the last server drops the list\n%s", data)
	}
	if err := a.CloseServer("web"); err != nil || len(a.MCP.Status()) != 0 {
		t.Fatal("closing a removed server takes it out of the manager")
	}
	if err := a.CloseServer("never-there"); err != nil {
		t.Fatalf("closing a server that is not open is quiet %v", err)
	}
	if err := a.RemoveServer("never-there"); err != nil {
		t.Fatalf("removing a name nothing configures is quiet %v", err)
	}
}

func TestToolsTurnOffAndSaveTheirNames(t *testing.T) {
	a, user, _ := editorApp(t)
	if err := a.SetToolEnabled("Read", false); err != nil {
		t.Fatal(err)
	}
	if a.Tools.Enabled("Read") || len(a.Tools.Definitions()) != 0 || len(a.Config.Tools.Disabled) != 1 {
		t.Fatal("an off tool leaves the requests")
	}
	got, err := ReadConfigFile(filepath.Join(user, ConfigFile))
	if err != nil || len(got.Tools.Disabled) != 1 || got.Tools.Disabled[0] != "Read" {
		t.Fatalf("the off names are saved %+v %v", got.Tools, err)
	}
	if err := a.SetToolEnabled("Read", true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(user, ConfigFile)); strings.Contains(string(data), "tools") {
		t.Fatalf("turning the last tool on drops the field\n%s", data)
	}
	merged := Merge(Config{Tools: ToolsConfig{Disabled: []string{"b", "a"}}}, Config{Tools: ToolsConfig{Disabled: []string{"a", "c"}}})
	if strings.Join(merged.Tools.Disabled, ",") != "a,b,c" {
		t.Fatalf("off names join across layers %v", merged.Tools.Disabled)
	}
}

func TestProfilesAreWrittenAsInstancesOfProviders(t *testing.T) {
	a, user, _ := editorApp(t)
	anthropic, _ := profiles.Lookup("anthropic")
	a.Primary = Endpoint{Profile: anthropic}
	work := profiles.Profile{Name: "work", Template: "anthropic", Key: profiles.Env("WORK_KEY"), Models: []string{"claude-x"}}
	if err := a.SaveProfile("", work); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveProfile("", profiles.Profile{Name: "home", Protocol: protocol.OllamaChat, BaseURL: "http://home:11434", Models: []string{"llama"}}); err != nil {
		t.Fatal(err)
	}
	list := a.Instances()
	if len(list) != 2 || list[0].Profile.BaseURL != anthropic.BaseURL || list[0].Written.BaseURL != "" || list[0].Profile.Key.Value != "WORK_KEY" || list[0].Path != filepath.Join(user, "profiles.json") {
		t.Fatalf("a profile takes what it leaves empty from its provider %+v", list)
	}
	if p, ok := a.FindProfile("work"); !ok || p.Auth != anthropic.Auth {
		t.Fatal("written profiles are found by name")
	}
	if _, ok := a.FindProfile("openai"); !ok || a.Shadowed("openai") {
		t.Fatal("providers are found as they are")
	}
	if err := a.SaveProfile("", profiles.Profile{Name: "work", Template: "anthropic"}); err == nil {
		t.Fatal("a second profile of the same name accepted")
	}
	if err := a.SaveProfile("", profiles.Profile{Name: "openai", Template: "openai"}); err == nil {
		t.Fatal("a new profile may not take the name of a provider")
	}
	if err := a.SaveProfile("", profiles.Profile{Name: "bad", Template: "nowhere"}); err == nil {
		t.Fatal("an unknown provider accepted")
	}
	if err := a.SaveProfile("", profiles.Profile{Name: "half", Protocol: protocol.OllamaChat}); err == nil {
		t.Fatal("a custom profile without an endpoint accepted")
	}
	if err := a.MoveProfile("home", -1); err != nil || a.Instances()[0].Profile.Name != "home" {
		t.Fatal("profiles move through their file")
	}
	if err := a.SetProfileEnabled("work", false); err != nil || !a.Instances()[1].Profile.Disabled {
		t.Fatal("profiles turn off")
	}
	if _, err := a.open(context.Background(), Target{Profile: "work"}); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("a profile turned off refuses to open %v", err)
	}
	renamed := a.Instances()[1].Written
	renamed.Name, renamed.Disabled = "job", false
	if err := a.SaveProfile("work", renamed); err != nil {
		t.Fatal(err)
	}
	if names := []string{a.Instances()[0].Profile.Name, a.Instances()[1].Profile.Name}; names[1] != "job" {
		t.Fatalf("an edit keeps the place of the profile %v", names)
	}
	a.Primary = Endpoint{Profile: a.Instances()[1].Profile}
	if a.RemoveProfile("job") == nil || a.SetProfileEnabled("job", false) == nil {
		t.Fatal("the profile in use stays and stays on")
	}
	off := a.Instances()[1].Written
	off.Disabled = true
	if a.SaveProfile("job", off) == nil {
		t.Fatal("the profile in use may not be saved turned off")
	}
	if err := a.RemoveProfile("home"); err != nil || len(a.Instances()) != 1 {
		t.Fatal("profiles delete")
	}
	for _, err := range []error{a.RemoveProfile("ghost"), a.MoveProfile("ghost", 1), a.SetProfileEnabled("ghost", true)} {
		if err == nil {
			t.Fatal("missing profiles are reported")
		}
	}
	data, _ := os.ReadFile(filepath.Join(user, "profiles.json"))
	if st, _ := os.Stat(filepath.Join(user, "profiles.json")); st.Mode().Perm() != 0o600 || !strings.Contains(string(data), `"template": "anthropic"`) || strings.Contains(string(data), "base_url") {
		t.Fatalf("the file holds what was written, readable by its owner alone\n%s", data)
	}
	extra := filepath.Join(t.TempDir(), "team.json")
	os.WriteFile(extra, []byte(`{"default":"team","profiles":[{"name":"team","template":"openai"},{"name":"job","template":"mistral"}]}`), 0o600)
	a.Config.ProfileFiles = []string{extra}
	if err := a.ReloadProfiles(); err != nil {
		t.Fatal(err)
	}
	if list := a.Instances(); len(list) != 2 || list[1].Profile.Name != "job" || list[1].Profile.Vendor != "anthropic" || list[1].Path != filepath.Join(user, "profiles.json") {
		t.Fatalf("a later file replaces an earlier namesake in place %+v", list)
	}
	if err := a.SaveProfile("team", profiles.Profile{Name: "team", Template: "openai", Models: []string{"gpt-x"}}); err != nil {
		t.Fatal(err)
	}
	if doc, _ := profiles.ReadFile(extra); doc.Profiles[0].Models[0] != "gpt-x" || doc.Default != "team" {
		t.Fatalf("an edit lands in the file holding the profile %+v", doc)
	}
	if _, def, err := loadInstances(a.Config); err != nil || def != "team" {
		t.Fatalf("the default a file names is read %q %v", def, err)
	}
	a.Config.ProfileFiles = []string{filepath.Join(t.TempDir(), "missing.json")}
	if err := a.ReloadProfiles(); err == nil {
		t.Fatal("a configured profile file that is missing is reported")
	}
}

func TestJournalRecordsWhatTheHarnessReports(t *testing.T) {
	j := NewJournal()
	j.Clock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	var passed []any
	send := j.Tap(func(m any) { passed = append(passed, m) })
	call := protocol.ToolCall{Name: "Bash", Args: []byte(`{"command":"ls"}`)}
	for _, msg := range []any{
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventRunStart, Parts: []protocol.Part{protocol.Text{Text: "check disk"}}}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventToolStart, Call: call}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventToolEnd, Call: call, Duration: time.Second}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventToolEnd, Call: call, Err: errors.New("denied")}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventToolEnd, Call: call, Result: protocol.ToolResult{IsError: true, Content: []protocol.Part{protocol.Text{Text: "exit 1"}}}}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventSteer, Parts: []protocol.Part{protocol.Text{Text: "faster"}}}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventCompactStart}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventCompactEnd}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventSubagentStart, Call: protocol.ToolCall{Name: "scout"}}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventSubagentEnd, Call: protocol.ToolCall{Name: "scout"}}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventInterrupt}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventHook, Text: "Stop ran"}},
		EventMsg{Run: "r1", Event: agent.Event{Type: agent.EventModel}},
		RunDoneMsg{Run: "r1", Duration: 1500 * time.Millisecond},
		RunDoneMsg{Run: "r2", Err: errors.New("boom")},
		ApprovalMsg{Pending: &Pending{ID: 3, Request: policy.Request{Call: call}}},
		ApprovalDoneMsg{ID: 3, Verdict: policy.Always},
		ApprovalDoneMsg{ID: 4, Err: errors.New("timed out")},
		AttemptMsg{Attempt: provider.Attempt{Provider: "anthropic", Method: "POST", URL: "https://x/v1", Status: 200, Duration: time.Second}},
		AttemptMsg{Attempt: provider.Attempt{Provider: "anthropic", Method: "POST", URL: "https://x/v1", Status: 529, Err: errors.New("overloaded"), Wait: time.Second}},
		AttemptMsg{Attempt: provider.Attempt{Provider: "anthropic", Method: "POST", URL: "https://x/v1", Status: 500, Err: errors.New("down")}},
		FailoverMsg{From: 0, Err: errors.New("down")},
		MCPMsg{Server: "s", State: mcp.StateConnected, Tools: []string{"a"}},
		MCPMsg{Server: "s", State: mcp.StateDisconnected, Err: errors.New("gone")},
		MCPMsg{Server: "s", State: mcp.StateClosed},
		MCPLogMsg{Line: LogLine{Server: "s", Level: "warning", Text: "slow"}},
		MCPLogMsg{Line: LogLine{Server: "s", Level: "debug", Text: "tick"}},
		MCPLogMsg{Line: LogLine{Server: "s", Level: "critical", Text: "dead"}},
		HookOutputMsg{Text: "hook says hi"},
		JobMsg{Event: shell.JobEvent{Kind: shell.JobStarted, Job: shell.JobInfo{ID: "j1", Line: "sleep 1"}}},
		JobMsg{Event: shell.JobEvent{Kind: shell.JobOutput, Job: shell.JobInfo{ID: "j1"}}},
		JobMsg{Event: shell.JobEvent{Kind: shell.JobFinished, Job: shell.JobInfo{ID: "j1", ExitCode: 0}}},
		JobMsg{Event: shell.JobEvent{Kind: shell.JobFinished, Job: shell.JobInfo{ID: "j2", TimedOut: true}}},
		JobMsg{Event: shell.JobEvent{Kind: shell.JobFinished, Job: shell.JobInfo{ID: "j3", Error: "no shell"}}},
		SavedMsg{Session: "s1"},
		SavedMsg{Session: "s1", Err: errors.New("disk full")},
		FactMsg{Change: facts.Change{Op: facts.OpAdd, Fact: facts.Fact{ID: "f1", Title: "open port"}}},
		MissionMsg{Mission: mission.Mission{ID: "m1", Status: "active"}, Op: mission.OpAdd},
		ProgressMsg{Run: "r1"},
	} {
		send(msg)
	}
	if len(passed) != 39 {
		t.Fatalf("every message passes on, %d", len(passed))
	}
	var text []string
	for _, l := range j.Lines() {
		text = append(text, l.Level+" "+l.Source+" "+l.Text)
	}
	all := strings.Join(text, "\n")
	for _, want := range []string{
		"info run r1 root started check disk",
		"debug run r1 Bash {\"command\":\"ls\"}",
		"info run r1 Bash done in 1s",
		"warn run r1 Bash refused, denied",
		"warn run r1 Bash failed, exit 1",
		"info run r1 steered faster",
		"info run r1 spawned scout",
		"warn run r1 interrupted",
		"info run r1 finished in 1.5s",
		"error run r2 ended with boom",
		"warn approval root asks to run Bash",
		"info approval request 3 answered always",
		"warn approval request 4 dropped, timed out",
		"debug endpoint anthropic POST https://x/v1 200 in 1s",
		"warn endpoint anthropic POST https://x/v1 529 in 0s, retrying in 1s, overloaded",
		"error endpoint anthropic POST https://x/v1 500 in 0s, down",
		"warn endpoint model 1 failed over, down",
		"info mcp s connected with 1 tools",
		"error mcp s disconnected, gone",
		"info mcp s closed",
		"warn mcp s slow",
		"debug mcp s tick",
		"error mcp s dead",
		"info hook hook says hi",
		"info job j1 started sleep 1",
		"info job j1 exited 0",
		"warn job j2 timed out",
		"error job j3 no shell",
		"error session s1 save failed, disk full",
		"info facts add f1 open port",
		"info mission m1 add active",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("journal misses %q\n%s", want, all)
		}
	}
	if len(text) != 35 {
		t.Fatalf("streaming output and quiet events stay out, %d lines\n%s", len(text), all)
	}
	j.Max = 2
	j.Add(LevelInfo, "app", "  ")
	j.Add(LevelInfo, "app", "kept")
	if lines := j.Lines(); len(lines) != 2 || lines[1].Text != "kept" || !lines[1].Time.Equal(j.Clock()) {
		t.Fatalf("the journal keeps the newest lines and skips blank ones %+v", lines)
	}
	j.Clear()
	if len(j.Lines()) != 0 {
		t.Fatal("clear drops every line")
	}
	if clip("a\n b   c", 10) != "a b c" || clip("abcdef", 4) != "abc…" {
		t.Fatal("clip folds and cuts")
	}
}
