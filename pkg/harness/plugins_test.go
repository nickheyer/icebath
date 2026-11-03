package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Project with a skill, a command, an agent and a style under .claude, built into an app
func pluginApp(t *testing.T) (*App, string) {
	t.Helper()
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(project, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("CLAUDE.md", "project rules here")
	write(".claude/skills/deploy/SKILL.md", "---\ndescription: Deploys the service\nallowed-tools: Bash(git status)\ndisallowed-tools: WebFetch\n---\nDeploy $ARGUMENTS from ${CLAUDE_SKILL_DIR}")
	write(".claude/skills/secret/SKILL.md", "---\ndescription: person only\ndisable-model-invocation: true\n---\nsecret steps")
	write(".claude/agents/reviewer.md", "---\nname: reviewer\ndescription: Reviews code\ntools: Read, Grep\nmaxTurns: 3\nmemory: project\n---\nYou review.")
	write(".claude/output-styles/terse.md", "---\ndescription: short\n---\nAnswer in one line.")
	write(".claude/rules/go.md", "---\npaths: [\"**/*.go\"]\n---\ngo only rules")
	cfg, err := Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	cfg.Plugins.Foreign = &off
	a, err := Build(context.Background(), cfg, func(any) {}, BuildOptions{Dir: project})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	return a, project
}

func TestLibraryKindsAndSkills(t *testing.T) {
	a, project := pluginApp(t)
	k, ok := a.Kind("reviewer")
	if !ok || k.MaxRounds != 3 || k.Memory != "project" || len(k.Tools) != 2 || k.System != "You review." {
		t.Fatalf("reviewer kind %+v ok=%v", k, ok)
	}
	for _, name := range []string{KindGeneral, KindExplore, KindPlan} {
		if _, ok := a.Kind(name); !ok {
			t.Fatalf("built in kind %s", name)
		}
	}
	lib := a.Library()
	if lib == nil {
		t.Fatal("no library")
	}
	if _, ok := lib.Skill("deploy"); !ok {
		t.Fatal("project skill")
	}
	system, err := a.PromptFor(context.Background(), a.Primary, Kind{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system, "- deploy: Deploys the service") || strings.Contains(system, "secret") {
		t.Fatalf("listing in prompt: %q", system)
	}
	if !strings.Contains(system, "project rules here") || !strings.Contains(system, filepath.Join(project, "CLAUDE.md")) {
		t.Fatal("project instructions in prompt")
	}
	if err := a.SetOutputStyle("terse"); err != nil {
		t.Fatal(err)
	}
	styled, err := a.PromptFor(context.Background(), a.Primary, Kind{})
	if err != nil || !strings.Contains(styled, "Answer in one line.") || strings.Contains(styled, "Delivering work") {
		t.Fatalf("style replaces conduct: %v %q", err, styled)
	}
	if err := a.SetOutputStyle("missing"); err == nil {
		t.Fatal("unknown style")
	}
	memo, err := a.memoryBlock(k)
	if err != nil || !strings.Contains(memo, "agent-memory") {
		t.Fatalf("memory block %v %q", err, memo)
	}
	plain, err := a.PromptFor(context.Background(), a.Primary, Kind{NoInstructions: true})
	if err != nil || strings.Contains(plain, "project rules here") {
		t.Fatal("omitting instructions leaves the project file out")
	}
}

func TestSkillToolAndExpansion(t *testing.T) {
	a, project := pluginApp(t)
	ctx := WithRun(context.Background(), "1")
	tool := skillTool{app: a}
	res, err := tool.Run(ctx, json.RawMessage(`{"skill":"deploy","args":"prod"}`))
	if err != nil || res.IsError {
		t.Fatalf("skill tool %+v %v", res, err)
	}
	text := protocol.Response{Content: res.Content}.Text()
	if !strings.Contains(text, "Deploy prod from "+filepath.Join(project, ".claude", "skills", "deploy")) {
		t.Fatalf("rendered %q", text)
	}
	ac := a.runActivation("1")
	if !ac.grants(protocol.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command":"git status"}`)}) || ac.grants(protocol.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command":"rm -rf x"}`)}) {
		t.Fatal("allowed-tools grant only the named command")
	}
	if !ac.withholds("WebFetch") {
		t.Fatal("disallowed tools are withheld")
	}
	again, _ := tool.Run(ctx, json.RawMessage(`{"skill":"deploy","args":"prod"}`))
	if !strings.Contains(protocol.Response{Content: again.Content}.Text(), "already loaded") {
		t.Fatal("repeated identical content is noted, not repeated")
	}
	secret, _ := tool.Run(ctx, json.RawMessage(`{"skill":"secret"}`))
	if !secret.IsError {
		t.Fatal("the model may not load a person only skill")
	}
	exp, err := a.ExpandSkills(context.Background(), a.Current(), "/secret now", false)
	if err != nil || len(exp.Parts) == 0 || !strings.Contains(protocol.Response{Content: exp.Parts}.Text(), "secret steps") {
		t.Fatalf("person expansion %+v %v", exp, err)
	}
	if _, _, ok := a.SkillOf("/nothing"); ok {
		t.Fatal("unknown skill")
	}
}

func TestPathRulesLoadOnTouch(t *testing.T) {
	a, project := pluginApp(t)
	sess := a.Session
	h := watchHook{app: a, sess: sess}
	call := protocol.ToolCall{ID: "1", Name: "Read", Args: json.RawMessage(`{"file_path":"` + filepath.Join(project, "main.go") + `"}`)}
	res, err := h.AfterTool(context.Background(), call, protocol.ToolResult{CallID: "1", Name: "Read", Content: []protocol.Part{protocol.Text{Text: "package main"}}})
	if err != nil || !strings.Contains(protocol.Response{Content: res.Content}.Text(), "go only rules") {
		t.Fatalf("path rule loads on touch: %v %+v", err, res)
	}
	res, _ = h.AfterTool(context.Background(), call, protocol.ToolResult{CallID: "1", Name: "Read", Content: []protocol.Part{protocol.Text{Text: "package main"}}})
	if strings.Contains(protocol.Response{Content: res.Content}.Text(), "go only rules") {
		t.Fatal("a rule loads once per session")
	}
}

func TestChildModeFollowsParent(t *testing.T) {
	if got := childMode("plan", Kind{Mode: "bypassPermissions"}); got != "plan" {
		t.Fatalf("plan parent keeps plan, got %s", got)
	}
	if got := childMode("default", Kind{Mode: "acceptEdits"}); got != "acceptEdits" {
		t.Fatalf("default parent takes the kind mode, got %s", got)
	}
	if got := childMode("default", Kind{Mode: "bypassPermissions"}); got != "default" {
		t.Fatalf("a kind cannot bypass under a default parent, got %s", got)
	}
	if got := childMode("acceptEdits", Kind{Mode: "plan"}); got != "acceptEdits" {
		t.Fatalf("accept edits parent wins, got %s", got)
	}
}

func TestPluginOperationsOnLocalMarketplace(t *testing.T) {
	refs := filepath.Join("..", "..", "references", "projects")
	if _, err := os.Stat(refs); err != nil {
		t.Skip("reference projects are not checked out")
	}
	refs, _ = filepath.Abs(refs)
	a, _ := pluginApp(t)
	ctx := context.Background()
	m, err := a.AddMarketplace(ctx, filepath.Join(refs, "hallmark"), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "hallmark" {
		t.Fatalf("marketplace %s", m.Name)
	}
	id, err := a.InstallPlugin(ctx, "hallmark", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Library().Skill("hallmark:hallmark"); !ok {
		t.Fatal("installed skill is loaded")
	}
	if _, ok := a.Library().Skill("hallmark"); !ok {
		t.Fatal("short alias")
	}
	if err := a.EnablePlugin(ctx, id, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Library().Skill("hallmark:hallmark"); ok {
		t.Fatal("disabled plugin skills leave")
	}
	if err := a.EnablePlugin(ctx, id, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.UninstallPlugin(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Library().Skill("hallmark:hallmark"); ok {
		t.Fatal("uninstalled plugin skills leave")
	}
	if err := a.RemoveMarketplace("hallmark"); err != nil {
		t.Fatal(err)
	}
}
