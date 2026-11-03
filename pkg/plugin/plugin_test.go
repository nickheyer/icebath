package plugin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Directory holding the reference plugin repositories, the test skipped when it is absent
func references(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "references", "projects")
	if !isDir(dir) {
		t.Skip("reference projects are not checked out")
	}
	return dir
}

func TestParseFrontReadsOnlyLeadingMarker(t *testing.T) {
	f := ParseFront("---\nname: demo\ndescription: >-\n  two\n  lines\nallowed-tools: Bash(git *) Read\nlist:\n  - a\n  - b\nflag: Yes\n---\n# body\n")
	if !f.Present || f.Err != nil {
		t.Fatalf("front not read: %+v", f)
	}
	if f.String("name") != "demo" || f.String("description") != "two lines" {
		t.Fatalf("scalars %q %q", f.String("name"), f.String("description"))
	}
	if got := f.Rules("allowed-tools"); len(got) != 2 || got[0] != "Bash(git *)" || got[1] != "Read" {
		t.Fatalf("rules %v", got)
	}
	if got := f.List("list"); len(got) != 2 {
		t.Fatalf("list %v", got)
	}
	if !f.Bool("flag", false) {
		t.Fatal("yes should read true")
	}
	if strings.TrimSpace(f.Body) != "# body" {
		t.Fatalf("body %q", f.Body)
	}
	late := ParseFront("intro\n---\nname: x\n---\n")
	if late.Present {
		t.Fatal("a marker past the first line is content")
	}
	bad := ParseFront("---\nname: [unclosed\n---\nbody")
	if bad.Err == nil || !bad.Present {
		t.Fatalf("bad yaml should keep the body with an error: %+v", bad)
	}
}

func TestSubstituteArgs(t *testing.T) {
	cases := []struct {
		body, args string
		names      []string
		want       string
		appended   bool
	}{
		{"fix $ARGUMENTS now", "the bug", nil, "fix the bug now", false},
		{"first $0 second $1", `"hello world" second`, nil, "first hello world second second", false},
		{"only $ARGUMENTS[1]", "a", nil, "only $ARGUMENTS[1]\n\nARGUMENTS: a", true},
		{"issue $issue on $branch", "12 main", []string{"issue", "branch"}, "issue 12 on main", false},
		{"costs \\$1.00 and $0", "x", nil, "costs $1.00 and x", false},
		{"no placeholder", "extra", nil, "no placeholder\n\nARGUMENTS: extra", true},
		{"named $issue stays", "", []string{"issue"}, "named  stays", false},
	}
	for _, c := range cases {
		got, appended := substituteArgs(c.body, c.args, c.names)
		if got != c.want || appended != c.appended {
			t.Errorf("substituteArgs(%q, %q) = %q %v, want %q %v", c.body, c.args, got, appended, c.want, c.appended)
		}
	}
}

func TestRenderRunsInlineCommandsOnce(t *testing.T) {
	s := ParseSkill("---\nname: demo\n---\nStatus: !`git status`\n\n```!\necho multi\n```\nsee ${CLAUDE_SKILL_DIR}/x\n", "/tmp/demo/SKILL.md")
	calls := 0
	r := Render{Vars: map[string]string{"CLAUDE_SKILL_DIR": "/tmp/demo"}, Shell: func(ctx context.Context, cmd, sh string) (string, error) {
		calls++
		return "out of " + cmd, nil
	}}
	out, err := s.Render(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !strings.Contains(out.Text, "Status: out of git status") || !strings.Contains(out.Text, "out of echo multi") || !strings.Contains(out.Text, "/tmp/demo/x") {
		t.Fatalf("rendered %q with %d calls", out.Text, calls)
	}
	off, err := s.Render(context.Background(), Render{})
	if err != nil || !strings.Contains(off.Text, "disabled by policy") {
		t.Fatalf("disabled shell %q %v", off.Text, err)
	}
	failing := Render{Shell: func(context.Context, string, string) (string, error) { return "", os.ErrNotExist }}
	if _, err := s.Render(context.Background(), failing); err == nil {
		t.Fatal("a failing command aborts the render")
	}
}

func TestParseCommandNamespace(t *testing.T) {
	c := ParseCommand("---\ndescription: migrate\n---\nrun $ARGUMENTS", "/p/commands/db/migrate.md", "/p/commands")
	if c.Short != "db:migrate" || !c.Legacy || c.Description != "migrate" {
		t.Fatalf("command %+v", c)
	}
}

func TestParseAgentRules(t *testing.T) {
	a, ok := ParseAgent("---\nname: reviewer\ndescription: reviews\ntools: Read, Grep\nmodel: sonnet\npermissionMode: manual\nmaxTurns: 5\nmemory: project\nhooks:\n  Stop:\n    - hooks:\n        - type: command\n          command: echo done\n---\nYou review.", "/p/agents/r.md", false)
	if !ok || a.Short != "reviewer" || len(a.Tools) != 2 || a.PermissionMode != "default" || a.MaxTurns != 5 || a.Memory != "project" {
		t.Fatalf("agent %+v ok=%v", a, ok)
	}
	if len(a.Hooks[agent.SubagentStop]) != 1 || len(a.Hooks[agent.Stop]) != 0 {
		t.Fatalf("stop hooks should move to SubagentStop: %+v", a.Hooks)
	}
	if _, ok := ParseAgent("---\nname: nodesc\n---\nbody", "/p/agents/n.md", false); ok {
		t.Fatal("an agent without a description is skipped outside plugins")
	}
	if p, ok := ParseAgent("not frontmatter", "/p/agents/plain.md", true); !ok || p.Short != "plain" {
		t.Fatalf("a plugin agent without frontmatter loads under its file name: %+v %v", p, ok)
	}
}

func TestParseMarketSource(t *testing.T) {
	cases := map[string]string{
		"anthropics/skills":                MarketGitHub,
		"owner/repo@v1":                    MarketGitHub,
		"https://github.com/owner/repo":    MarketGit,
		"git@github.com:owner/repo.git":    MarketGit,
		"https://example.com/catalog.json": MarketURL,
		"./":                               MarketDirectory,
	}
	for text, kind := range cases {
		src, err := ParseMarketSource(text)
		if err != nil || src.Kind != kind {
			t.Errorf("%q: %+v %v, want %s", text, src, err, kind)
		}
	}
	if _, err := ParseMarketSource("not a source!"); err == nil {
		t.Fatal("garbage should fail")
	}
	if !ReservedMarket("claude-code-plugins", MarketSource{Kind: MarketGitHub, Repo: "someone/x"}) {
		t.Fatal("official names are reserved for others")
	}
	if ReservedMarket("claude-code-plugins", MarketSource{Kind: MarketGitHub, Repo: "anthropics/claude-code"}) {
		t.Fatal("anthropic may use its own names")
	}
}

func TestLoadEGM(t *testing.T) {
	root := filepath.Join(references(t), "elephant-goldfish-model")
	p, err := Load(root, LoadOptions{ID: "egm@egm", Entry: "egm", Marketplace: "egm"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Format != FormatClaude || p.Name != "egm" {
		t.Fatalf("manifest %+v", p.Manifest)
	}
	if len(p.Skills) != 7 {
		t.Fatalf("egm has 7 skills, got %d", len(p.Skills))
	}
	if len(p.Agents) != 9 {
		t.Fatalf("egm lists 9 agents, got %d", len(p.Agents))
	}
	var review *Skill
	for _, s := range p.Skills {
		if s.Name == "egm:egm-review" {
			review = s
		}
	}
	if review == nil || !review.DisableModelInvocation || review.Short != "egm-review" {
		t.Fatalf("egm-review %+v", review)
	}
	found := false
	for _, a := range p.Agents {
		if a.Name == "egm:egm-design" && a.OmitInstructions && a.Color == "yellow" && len(a.Tools) == 3 {
			found = true
		}
	}
	if !found {
		t.Fatal("egm-design adapter not read with its tools, color and omitClaudeMd")
	}
	for _, prob := range p.Problems {
		if strings.Contains(prob, "Default agents/ folder is ignored") {
			found = true
		}
	}
	m, err := ReadMarketplace(filepath.Join(root, ".agents", "plugins", "marketplace.json"))
	if err != nil || len(m.Plugins) != 1 || m.Plugins[0].Source.Kind != SourceRelative {
		t.Fatalf("codex marketplace %+v %v", m, err)
	}
	portable, err := ParseManifest(mustRead(t, filepath.Join(root, "plugin.json")), filepath.Join(root, "plugin.json"))
	if err != nil || portable.Format != FormatPortable {
		t.Fatalf("portable manifest %+v %v", portable, err)
	}
	codex, err := ParseManifest(mustRead(t, filepath.Join(root, CodexManifest)), filepath.Join(root, CodexManifest))
	if err != nil || codex.Format != FormatCodex || len(codex.Skills) != 1 || codex.Interface.DisplayName == "" {
		t.Fatalf("codex manifest %+v %v", codex, err)
	}
}

func TestLoadImpeccable(t *testing.T) {
	root := filepath.Join(references(t), "impeccable", "plugin")
	p, err := Load(root, LoadOptions{ID: "impeccable@impeccable", Entry: "impeccable", Marketplace: "impeccable", ProjectDir: "/proj"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "impeccable:impeccable" || !p.Skills[0].UserInvocable {
		t.Fatalf("skills %+v", p.Skills)
	}
	if len(p.Agents) != 4 {
		t.Fatalf("4 agents, got %d", len(p.Agents))
	}
	if len(p.Hooks[agent.SessionStart]) != 1 || len(p.Hooks[agent.PostToolUse]) != 1 || len(p.Hooks[agent.Stop]) != 1 {
		t.Fatalf("hooks %+v", p.Hooks)
	}
	h := p.Hooks[agent.PostToolUse][0].Hooks[0]
	if h.Source != agent.SourcePlugin || h.Owner != "impeccable" || h.Vars["CLAUDE_PLUGIN_ROOT"] != root || h.Env["IMPECCABLE_HOOK_HARNESS"] == "" || h.StatusMessage == "" || h.Timeout != 5 {
		t.Fatalf("hook %+v", h)
	}
	for _, a := range p.Agents {
		if a.Name == "impeccable:impeccable-finish-reviewer" && (a.MaxTurns != 30 || a.Effort != "high" || len(a.Tools) != 4) {
			t.Fatalf("finish reviewer %+v", a)
		}
	}
	for _, s := range p.Skills {
		vars := TextVars("sess", "/proj", "", s)
		if vars["CLAUDE_SKILL_DIR"] != s.Dir || vars["CLAUDE_PLUGIN_ROOT"] != root {
			t.Fatalf("vars %v", vars)
		}
	}
}

func TestLoadHallmarkAndHermes(t *testing.T) {
	refs := references(t)
	hallmark, err := Load(filepath.Join(refs, "hallmark"), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hallmark.Skills) != 1 || hallmark.Skills[0].Name != "hallmark:hallmark" || hallmark.Skills[0].ArgumentHint == "" || hallmark.Skills[0].Version != "2.0.0" {
		t.Fatalf("hallmark %+v", hallmark.Skills)
	}
	m, err := ReadMarketplace(filepath.Join(refs, "hallmark", ".claude-plugin", "marketplace.json"))
	if err != nil || len(m.Plugins) != 1 || m.Plugins[0].Strict() || len(m.Plugins[0].Skills) != 1 {
		t.Fatalf("hallmark marketplace %+v %v", m, err)
	}
	skills, err := readSkillTree(filepath.Join(refs, "hermes-agent", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) < 20 {
		t.Fatalf("hermes categories hold skills, got %d", len(skills))
	}
	for _, s := range skills {
		if s.Name == "" && s.Short == "" {
			t.Fatalf("unnamed skill %s", s.File)
		}
		if len(s.Platforms) > 0 && !s.Supported("linux") && !s.Supported("darwin") && !s.Supported("windows") {
			t.Fatalf("platform gate of %s reads no platform: %v", s.Short, s.Platforms)
		}
	}
}

func TestDiscoverResolvesNames(t *testing.T) {
	refs := references(t)
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	config := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(project, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude/skills/deploy/SKILL.md", "---\ndescription: deploy it\n---\nDeploy $ARGUMENTS")
	write(".claude/commands/deploy.md", "---\ndescription: old deploy\n---\nold")
	write(".claude/commands/frontend/component.md", "make a component")
	write(".claude/agents/reviewer.md", "---\nname: reviewer\ndescription: reviews code\n---\nReview it.")
	write("sub/.claude/skills/deploy/SKILL.md", "---\ndescription: nested deploy\n---\nnested")
	write(".claude/output-styles/terse.md", "---\ndescription: short answers\n---\nBe terse.")
	lib, err := Discover(Sources{ConfigDir: config, Root: project, Dir: filepath.Join(project, "sub"), PluginDirs: []string{filepath.Join(refs, "impeccable", "plugin")}, Overrides: map[string]string{"frontend:component": "off"}})
	if err != nil {
		t.Fatal(err)
	}
	deploy, ok := lib.Skill("deploy")
	if !ok || deploy.Legacy || deploy.Description != "deploy it" {
		t.Fatalf("the skill beats the command: %+v", deploy)
	}
	if _, ok := lib.Skill("sub:deploy"); !ok {
		t.Fatalf("nested clash gets the directory prefix, have %v", names(lib.Skills))
	}
	if _, ok := lib.Skill("frontend:component"); ok {
		t.Fatal("an off override hides the command")
	}
	if s, ok := lib.Skill("impeccable"); !ok || s.Plugin == nil || s.Name != "impeccable:impeccable" {
		t.Fatalf("plugin skills answer to their short name when free: %+v", s)
	}
	if a, ok := lib.Agent("reviewer"); !ok || a.Origin != OriginProject {
		t.Fatalf("project agent %+v", a)
	}
	if _, ok := lib.Agent("impeccable:impeccable-documenter"); !ok {
		t.Fatal("plugin agents are namespaced")
	}
	if _, ok := lib.Style("terse"); !ok {
		t.Fatal("project style")
	}
	if len(lib.Hooks[agent.PostToolUse]) == 0 {
		t.Fatal("plugin hooks are gathered")
	}
	listing := lib.Listing(200000, 0, nil)
	if !strings.Contains(listing, "- deploy: deploy it") || !strings.Contains(listing, "impeccable:impeccable") {
		t.Fatalf("listing %q", listing)
	}
	tight := lib.Listing(1000, 0.001, map[string]int{"deploy": 5})
	if len(tight) > 1000*0.001*4+200 || !strings.Contains(tight, "deploy") {
		t.Fatalf("budgeted listing %q", tight)
	}
}

func TestInstructionsImportsAndPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(project, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(text), 0o644)
	}
	write("CLAUDE.md", "root rules\nsee @docs/style.md here")
	write("docs/style.md", "style rules")
	write(".claude/rules/go.md", "---\npaths:\n  - \"**/*.go\"\n---\ngo rules")
	write("sub/AGENTS.md", "sub rules")
	list := LoadInstructions(t.TempDir(), project, filepath.Join(project, "sub"))
	always := Always(list)
	var texts []string
	for _, in := range always {
		texts = append(texts, in.Text)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "root rules") || !strings.Contains(joined, "style rules") || !strings.Contains(joined, "sub rules") {
		t.Fatalf("instructions %q", joined)
	}
	if got := Matching(list, project, filepath.Join(project, "pkg", "a.go")); len(got) != 1 || !strings.Contains(got[0].Text, "go rules") {
		t.Fatalf("path rules %+v", got)
	}
	if got := Matching(list, project, filepath.Join(project, "pkg", "a.md")); len(got) != 0 {
		t.Fatalf("md should not match: %+v", got)
	}
}

func TestStoreInstallsLocalMarketplace(t *testing.T) {
	refs := references(t)
	store := NewStore(t.TempDir())
	src, err := ParseMarketSource(filepath.Join(refs, "elephant-goldfish-model"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.AddMarketplace(context.Background(), src, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "egm" {
		t.Fatalf("marketplace %s", m.Name)
	}
	id, in, err := store.Install(context.Background(), "egm", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if id != "egm@egm" || !in.InPlace || in.Version != "0.1.0" {
		t.Fatalf("install %s %+v", id, in)
	}
	plugins, errs, err := store.Load(map[string]bool{}, nil, "")
	if err != nil || len(errs) != 0 || len(plugins) != 1 || !plugins[0].Enabled || len(plugins[0].Skills) != 7 {
		t.Fatalf("load %v %v %+v", err, errs, plugins)
	}
	if err := store.Uninstall(id, false); err != nil {
		t.Fatal(err)
	}
	if installed, _ := store.Installed(); len(installed) != 0 {
		t.Fatal("uninstall should empty the store")
	}
	if err := store.RemoveMarketplace("egm"); err != nil {
		t.Fatal(err)
	}
}

func TestTomlPlugins(t *testing.T) {
	got := tomlPlugins("[plugins.\"egm@egm\"]\nenabled = false\n\n[plugins.\"x@y\"]\nenabled = true\n[plugins.\"x@y\".mcp_servers.docs]\nenabled = false\n")
	if got["egm@egm"] || !got["x@y"] || len(got) != 2 {
		t.Fatalf("toml %v", got)
	}
}

func names(skills []*Skill) []string {
	var out []string
	for _, s := range skills {
		out = append(out, s.Name)
	}
	return out
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
