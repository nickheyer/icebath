package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/policy/rules"
	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestReadConfigRejectsBadInput(t *testing.T) {
	if c, err := ReadConfig(strings.NewReader(`{"profile":"x","nope":1}`)); err != nil || c.Profile != "x" {
		t.Fatalf("unknown keys must be ignored: %v %+v", err, c)
	}
	if _, err := ReadConfig(strings.NewReader(`{"mode":"weird"}`)); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := ReadConfig(strings.NewReader(`{"hooks":{"Nope":[]}}`)); err == nil {
		t.Fatal("unknown hook event accepted")
	}
	if _, err := ReadConfigFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
	c, err := ReadConfig(strings.NewReader(`{"profile":"anthropic","model":"m","mode":"plan","max_tokens":10,"permissions":{"allow":["Read"]},"mcp":[{"name":"a","command":"x"}]}`))
	if err != nil || c.Profile != "anthropic" || c.Mode != "plan" || c.MaxTokens != 10 || len(c.Permissions.Allow) != 1 || len(c.MCP) != 1 {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestMergeLayersConfigurations(t *testing.T) {
	temp := 0.3
	motion := 0.5
	cascade := true
	base := Config{Profile: "a", Model: "m1", MaxTokens: 1, Permissions: rules.Set{Allow: []string{"Read"}}, Hooks: hooks.Config{hooks.Stop: []hooks.Group{{Hooks: []hooks.Command{{Command: "a"}}}}}, MCP: []ServerConfig{{Name: "x", Command: "old"}, {Name: "y", Command: "y"}}, Shell: ShellConfig{Env: []string{"A=1"}}, Sources: []string{"base"}}
	over := Config{Model: "m2", MaxRounds: 3, Temperature: &temp, Permissions: rules.Set{Deny: []string{"Bash"}}, Hooks: hooks.Config{hooks.Stop: []hooks.Group{{Hooks: []hooks.Command{{Command: "b"}}}}, hooks.PreToolUse: []hooks.Group{{Hooks: []hooks.Command{{Command: "c"}}}}}, MCP: []ServerConfig{{Name: "x", Command: "new"}, {Name: "z", URL: "http://z"}}, Subagents: Subagents{Parallel: true, MaxDepth: 2, Timeout: 9, Model: &Target{Profile: "p"}}, Shell: ShellConfig{Timeout: 5, MaxTimeout: 6, Env: []string{"B=2"}}, Web: WebConfig{AllowPrivate: true, Disabled: true}, UI: UIConfig{Theme: "light", Motion: &motion, Ratio: 0.5, Cascade: &cascade}, Record: &RecordConfig{Path: "c.json"}, Root: "/r", SessionsDir: "/s", Compaction: Compaction{Threshold: 100, Keep: 2, Model: &Target{Profile: "q"}, Instructions: "i"}, Judge: &Judge{Target: Target{Profile: "j"}}, Fallbacks: []Target{{Profile: "f"}}, System: "sys", SystemFile: "f.md", Mode: "plan", Extra: map[string]json.RawMessage{"k": json.RawMessage(`1`)}, ProfileFiles: []string{"p.json"}, CatalogFiles: []string{"c.json"}, Sources: []string{"over"}}
	out := Merge(base, over)
	if out.Profile != "a" || out.Model != "m2" || out.MaxTokens != 1 || out.MaxRounds != 3 || *out.Temperature != 0.3 || out.Mode != "plan" || out.System != "sys" || out.SystemFile != "f.md" {
		t.Fatalf("%+v", out)
	}
	if len(out.Permissions.Allow) != 1 || len(out.Permissions.Deny) != 1 {
		t.Fatalf("%+v", out.Permissions)
	}
	if len(out.Hooks[hooks.Stop]) != 2 || len(out.Hooks[hooks.PreToolUse]) != 1 {
		t.Fatalf("%+v", out.Hooks)
	}
	if len(out.MCP) != 3 || out.MCP[0].Command != "new" || out.MCP[1].Name != "y" || out.MCP[2].Name != "z" {
		t.Fatalf("%+v", out.MCP)
	}
	if !out.Subagents.Parallel || out.Subagents.MaxDepth != 2 || out.Subagents.Timeout != 9 || out.Subagents.Model.Profile != "p" {
		t.Fatalf("%+v", out.Subagents)
	}
	if out.Shell.Timeout != 5 || out.Shell.MaxTimeout != 6 || len(out.Shell.Env) != 2 || !out.Web.AllowPrivate || !out.Web.Disabled {
		t.Fatalf("%+v %+v", out.Shell, out.Web)
	}
	if out.UI.Theme != "light" || *out.UI.Motion != 0.5 || out.UI.Ratio != 0.5 || out.UI.Cascade == nil || *out.UI.Cascade != true || out.Record.Path != "c.json" || out.Root != "/r" || out.SessionsDir != "/s" {
		t.Fatalf("%+v", out)
	}
	if out.Compaction.Threshold != 100 || out.Compaction.Keep != 2 || out.Compaction.Model.Profile != "q" || out.Compaction.Instructions != "i" || out.Judge.Profile != "j" || len(out.Fallbacks) != 1 {
		t.Fatalf("%+v", out)
	}
	if string(out.Extra["k"]) != "1" || len(out.ProfileFiles) != 1 || len(out.CatalogFiles) != 1 || len(out.Sources) != 2 {
		t.Fatalf("%+v", out)
	}
	if again := Merge(out, Config{Extra: map[string]json.RawMessage{"j": json.RawMessage(`2`)}}); len(again.Extra) != 2 {
		t.Fatal("extra must merge")
	}
}

func TestDiscoverLayersFilesAndEnvironment(t *testing.T) {
	user := t.TempDir()
	project := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, user)
	t.Setenv(ProfileEnv, "")
	t.Setenv(ModelEnv, "")
	cfg, err := Discover(project)
	if err != nil || cfg.Profile != "" {
		t.Fatalf("%v %+v", err, cfg)
	}
	os.WriteFile(filepath.Join(user, ConfigFile), []byte(`{"profile":"openai","model":"gpt-5","permissions":{"allow":["Read"]}}`), 0o644)
	os.WriteFile(filepath.Join(user, RulesFile), []byte(`{"permissions":{"deny":["Bash(rm *)"]}}`), 0o644)
	os.MkdirAll(filepath.Join(project, ProjectDir), 0o755)
	os.WriteFile(filepath.Join(project, ProjectDir, ConfigFile), []byte(`{"model":"gpt-5-mini","mode":"acceptEdits"}`), 0o644)
	os.WriteFile(filepath.Join(project, ProjectDir, RulesFile), []byte(`{"allow":["Glob"]}`), 0o644)
	os.WriteFile(filepath.Join(project, ProjectDir, HooksFile), []byte(`{"hooks":{"Stop":[{"hooks":[{"command":"echo"}]}]}}`), 0o644)
	cfg, err = Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "openai" || cfg.Model != "gpt-5-mini" || cfg.Mode != "acceptEdits" {
		t.Fatalf("%+v", cfg)
	}
	if len(cfg.Permissions.Allow) != 2 || len(cfg.Permissions.Deny) != 1 || len(cfg.Hooks[hooks.Stop]) != 1 {
		t.Fatalf("%+v %+v", cfg.Permissions, cfg.Hooks)
	}
	if len(cfg.Sources) != 5 {
		t.Fatalf("%v", cfg.Sources)
	}
	t.Setenv(ProfileEnv, "google")
	t.Setenv(ModelEnv, "gemini-2.5-pro")
	cfg, _ = Discover(project)
	if cfg.Profile != "google" || cfg.Model != "gemini-2.5-pro" {
		t.Fatalf("%+v", cfg)
	}
	os.WriteFile(filepath.Join(project, ProjectDir, RulesFile), []byte(`{bad`), 0o644)
	if _, err := Discover(project); err == nil {
		t.Fatal("bad rules accepted")
	}
	os.WriteFile(filepath.Join(project, ProjectDir, RulesFile), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(project, ProjectDir, HooksFile), []byte(`{bad`), 0o644)
	if _, err := Discover(project); err == nil {
		t.Fatal("bad hooks accepted")
	}
	os.WriteFile(filepath.Join(project, ProjectDir, HooksFile), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(project, ProjectDir, ConfigFile), []byte(`{bad`), 0o644)
	if _, err := Discover(project); err == nil {
		t.Fatal("bad config accepted")
	}
	sessions, err := cfg.SessionsPath()
	if err != nil || sessions != filepath.Join(user, SessionsDir) {
		t.Fatalf("%v %q", err, sessions)
	}
	cfg.SessionsDir = "rel"
	if p, _ := cfg.SessionsPath(); !filepath.IsAbs(p) {
		t.Fatal("relative sessions dir must become absolute")
	}
	sys := filepath.Join(project, "sys.md")
	os.WriteFile(sys, []byte(" be brief \n"), 0o644)
	cfg.SystemFile = sys
	if got, err := cfg.SystemPrompt(); err != nil || got != "be brief" {
		t.Fatalf("%v %q", err, got)
	}
	cfg.SystemFile = filepath.Join(project, "nope.md")
	if _, err := cfg.SystemPrompt(); err == nil {
		t.Fatal("missing system file accepted")
	}
	cfg.SystemFile, cfg.System = "", "inline"
	if got, _ := cfg.SystemPrompt(); got != "inline" {
		t.Fatal(got)
	}
}

func TestKnobsAndKeyValues(t *testing.T) {
	var c Config
	for _, ok := range []string{"max_tokens=100", "max_rounds=3", "temperature=0.5", "compact_threshold=0", "mode=plan", "theme=light", "motion=0.2", "cascade=true", "extra.thinking={\"type\":\"adaptive\"}", "extra.note=plain"} {
		k, v, err := ParseKnob(ok)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetKnob(k, v); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	if c.MaxTokens != 100 || c.MaxRounds != 3 || *c.Temperature != 0.5 || c.Mode != "plan" || c.UI.Theme != "light" || *c.UI.Motion != 0.2 || c.UI.Cascade == nil || !*c.UI.Cascade || string(c.Extra["thinking"]) != `{"type":"adaptive"}` || string(c.Extra["note"]) != `"plain"` {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{"max_tokens=x", "max_tokens=0", "max_rounds=-1", "temperature=9", "compact_threshold=-1", "mode=weird", "motion=2", "cascade=maybe", "unknown=1"} {
		k, v, _ := ParseKnob(bad)
		if err := c.SetKnob(k, v); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if _, _, err := ParseKnob("novalue"); err == nil {
		t.Fatal("knob without value accepted")
	}
}

func TestNewlineIsNoKnob(t *testing.T) {
	var c Config
	if err := c.SetKnob("newline", "shift+enter"); err == nil {
		t.Fatal("a backslash before enter is the one newline, there is no knob for it")
	}
}
