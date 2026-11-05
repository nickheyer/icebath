package skills

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

var testCoreCommands = map[string]bool{"help": true, "model": true, "skills": true, "skill": true, "reload-skills": true, "new": true, "quit": true, "exit": true, "q": true}

func init() {
	RegisterCoreCommandResolver(func(name string) bool { return testCoreCommands[name] })
}

type fakePluginHost struct {
	skills   map[string]string
	meta     []map[string]any
	owners   map[string][]string
	disabled map[string]bool
}

func (f *fakePluginHost) Discover(context.Context) {}
func (f *fakePluginHost) FindPluginSkill(q string) (string, bool) {
	p, ok := f.skills[q]
	return p, ok
}
func (f *fakePluginHost) ListPluginSkills(string) []string                      { return nil }
func (f *fakePluginHost) ListPluginSkillMetadata() []map[string]any             { return f.meta }
func (f *fakePluginHost) RemovePluginSkill(string)                              {}
func (f *fakePluginHost) LegacyPortableSkillName(namespace, bare string) string { return "" }
func (f *fakePluginHost) ActiveMemoryProvider() (string, error)                 { return "", nil }
func (f *fakePluginHost) PruneInactiveMemoryProviderSkills(string) error        { return nil }
func (f *fakePluginHost) LoadMemoryProvider(string) error                       { return nil }
func (f *fakePluginHost) SkillOwners(q string) []string                         { return f.owners[q] }
func (f *fakePluginHost) DisabledPlugins() map[string]bool                      { return f.disabled }

// Installs a plugin host for the test and restores the previous factory afterwards
func withPluginHost(t *testing.T, host PluginSkillHost) {
	t.Helper()
	pluginHostMu.Lock()
	prev := pluginHostFn
	pluginHostFn = func(context.Context, string) PluginSkillHost { return host }
	pluginHostMu.Unlock()
	t.Cleanup(func() {
		pluginHostMu.Lock()
		pluginHostFn = prev
		pluginHostMu.Unlock()
	})
}

func resetSkillCommandCaches(t *testing.T) {
	t.Helper()
	skillCmds.mu.Lock()
	skillCmds.byKey = map[skillCommandScope]SkillCommands{}
	skillCmds.sigByKey = map[skillCommandScope]string{}
	skillCmds.lastInteractive = map[skillCommandScope]SkillCommands{}
	skillCmds.recency = nil
	skillCmds.plugin = SkillCommands{}
	skillCmds.pluginTag = nil
	skillCmds.mu.Unlock()
	ClearSkillsCache()
}

// Scope over a fresh home whose skills dir is overridden to a temp dir, with an empty plugin host
func cmdScope(t *testing.T) (*Scope, string, *lockedBuffer) {
	t.Helper()
	resetSkillCommandCaches(t)
	withPluginHost(t, &fakePluginHost{})
	root := t.TempDir()
	skills := filepath.Join(root, "skills")
	os.MkdirAll(skills, 0o755)
	s := NewScope(filepath.Join(root, "home"), config.Defaults())
	s.SkillsDirOverride = skills
	s.Cwd = root
	s.Platform = ""
	logs := &lockedBuffer{}
	s.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return s, skills, logs
}

func makeCmdSkill(t *testing.T, skillsDir, name, extra, body, category string) string {
	t.Helper()
	if body == "" {
		body = "Do the thing."
	}
	d := filepath.Join(skillsDir, category, name)
	os.MkdirAll(d, 0o755)
	content := "---\nname: " + name + "\ndescription: Description for " + name + ".\n" + extra + "---\n\n# " + name + "\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func mustInvoke(t *testing.T, s *Scope, key, instruction string) string {
	t.Helper()
	msg, ok, err := s.BuildSkillInvocationMessage(context.Background(), key, instruction, "", "")
	if err != nil || !ok {
		t.Fatalf("invoke %s: %v %v", key, ok, err)
	}
	return msg
}

func TestScanLoadsSkillInvocationFromSymlinkedSkillDir(t *testing.T) {
	s, skills, _ := cmdScope(t)
	real := makeCmdSkill(t, filepath.Join(filepath.Dir(skills), "external"), "impeccable", "", "Apply impeccable design craft.", "")
	if err := os.Symlink(real, filepath.Join(skills, "impeccable")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ScanSkillCommands()["/impeccable"]; !ok {
		t.Fatal("symlinked skill not scanned")
	}
	if msg := mustInvoke(t, s, "/impeccable", ""); !strings.Contains(msg, "Apply impeccable design craft.") {
		t.Fatal(msg)
	}
}

func TestGetSkillCommandsRescansWhenPlatformScopeChanges(t *testing.T) {
	s, skills, _ := cmdScope(t)
	s.Config["skills"] = map[string]any{"platform_disabled": map[string]any{"telegram": []any{"telegram-only"}, "discord": []any{"discord-only"}}}
	for _, n := range []string{"shared", "telegram-only", "discord-only"} {
		makeCmdSkill(t, skills, n, "", "", "")
	}
	view := func(platform string) SkillCommands {
		sc := *s
		sc.Platform = platform
		return copyCommands(sc.GetSkillCommands())
	}
	has := func(c SkillCommands, k string) bool { _, ok := c[k]; return ok }
	tg := view("telegram")
	if !has(tg, "/shared") || !has(tg, "/discord-only") || has(tg, "/telegram-only") {
		t.Fatalf("telegram %v", tg)
	}
	dc := view("discord")
	if !has(dc, "/shared") || !has(dc, "/telegram-only") || has(dc, "/discord-only") {
		t.Fatalf("discord %v", dc)
	}
	again := view("telegram")
	if has(again, "/telegram-only") || !has(again, "/discord-only") {
		t.Fatalf("telegram again %v", again)
	}
	bare := view("")
	if !has(bare, "/telegram-only") || !has(bare, "/shared") {
		t.Fatalf("bare %v", bare)
	}
}

func TestGetSkillCommandsRescansWhenProfileHomeChanges(t *testing.T) {
	s, _, _ := cmdScope(t)
	root := t.TempDir()
	extA, extB := filepath.Join(root, "external_a"), filepath.Join(root, "external_b")
	makeCmdSkill(t, extA, "a-only", "", "", "")
	makeCmdSkill(t, extB, "b-only", "", "", "")
	empty := filepath.Join(root, "no-local-skills")
	os.MkdirAll(empty, 0o755)
	profile := func(h, ext string) SkillCommands {
		sc := *s
		sc.Home = h
		sc.SkillsDirOverride = empty
		sc.Config = config.Defaults()
		sc.Config["skills"] = map[string]any{"external_dirs": []any{ext}}
		return copyCommands(sc.GetSkillCommands())
	}
	a := profile(filepath.Join(root, "profile_a"), extA)
	b := profile(filepath.Join(root, "profile_b"), extB)
	if _, ok := a["/a-only"]; !ok {
		t.Fatalf("%v", a)
	}
	if _, ok := a["/b-only"]; ok {
		t.Fatalf("%v", a)
	}
	if _, ok := b["/b-only"]; !ok {
		t.Fatalf("%v", b)
	}
	if _, ok := b["/a-only"]; ok {
		t.Fatalf("%v", b)
	}
}

func TestGetSkillCommandsScansProfileSkillsDir(t *testing.T) {
	s, skills, _ := cmdScope(t)
	profileB := filepath.Join(t.TempDir(), "profiles", "b")
	makeCmdSkill(t, filepath.Join(profileB, "skills"), "b-only", "", "Body of b-only.", "")
	sc := *s
	sc.Home = profileB
	sc.SkillsDirOverride = ""
	commands := sc.GetSkillCommands()
	if _, ok := commands["/b-only"]; !ok {
		t.Fatalf("%v", commands)
	}
	for _, info := range commands {
		if strings.HasPrefix(info["skill_dir"].(string), skills) {
			t.Fatal("launch skills dir leaked into the profile scan")
		}
	}
	if msg := mustInvoke(t, &sc, "/b-only", "go"); !strings.Contains(msg, "Body of b-only.") {
		t.Fatal(msg)
	}
}

func TestSlugCollisionKeepsFirstSkill(t *testing.T) {
	s, skills, _ := cmdScope(t)
	first, second := filepath.Join(skills, "a-first"), filepath.Join(skills, "z-second")
	os.MkdirAll(first, 0o755)
	os.MkdirAll(second, 0o755)
	os.WriteFile(filepath.Join(first, "SKILL.md"), []byte("---\nname: git_helper\ndescription: First skill.\n---\n\nBody.\n"), 0o644)
	os.WriteFile(filepath.Join(second, "SKILL.md"), []byte("---\nname: git-helper\ndescription: Second skill.\n---\n\nBody.\n"), 0o644)
	got := s.ScanSkillCommands()["/git-helper"]
	if got == nil || got["name"] != "git_helper" || got["skill_dir"] != first {
		t.Fatalf("%v", got)
	}
}

// Swaps the frontmatter parser the scan uses for the test duration
func withCommandFrontmatter(t *testing.T, fn func(string) (map[string]any, string)) {
	t.Helper()
	prev := commandFrontmatter
	commandFrontmatter = fn
	t.Cleanup(func() { commandFrontmatter = prev })
}

func TestConcurrentScansDoNotReportSkillsAsClaimingThemselves(t *testing.T) {
	s, skills, logs := cmdScope(t)
	for i := 0; i < 5; i++ {
		makeCmdSkill(t, skills, "skill-"+itoa(i), "", "", "")
	}
	parked, otherDone := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	withCommandFrontmatter(t, func(content string) (map[string]any, string) {
		if once.CompareAndSwap(false, true) {
			close(parked)
			select {
			case <-otherDone:
			case <-time.After(10 * time.Second):
			}
		}
		return ParseFrontmatter(content)
	})
	done := make(chan struct{})
	go func() { s.ScanSkillCommands(); close(done) }()
	<-parked
	fg := s.ScanSkillCommands()
	close(otherDone)
	<-done
	if strings.Contains(logs.String(), "already claimed") {
		t.Fatalf("overlapping scans reported self-collisions: %s", logs.String())
	}
	if len(fg) != 5 || fg["/skill-0"]["name"] != "skill-0" {
		t.Fatalf("%v", fg)
	}
}

func TestPublicationAndLookupShareOneLock(t *testing.T) {
	s, skills, _ := cmdScope(t)
	makeCmdSkill(t, skills, "shared", "", "", "")
	s.ScanSkillCommands()
	done := make(chan struct{})
	skillCmds.mu.Lock()
	go func() { s.GetSkillCommands(); close(done) }()
	select {
	case <-done:
		skillCmds.mu.Unlock()
		t.Fatal("lookup read the published pair without the publish lock")
	case <-time.After(500 * time.Millisecond):
	}
	skillCmds.mu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not finish after release")
	}
}

func TestScanNeverPublishesPartiallyBuiltMap(t *testing.T) {
	s, skills, _ := cmdScope(t)
	for i := 0; i < 5; i++ {
		makeCmdSkill(t, skills, "skill-"+itoa(i), "", "", "")
	}
	s.ScanSkillCommands()
	var mu sync.Mutex
	var sizes []int
	withCommandFrontmatter(t, func(content string) (map[string]any, string) {
		skillCmds.mu.Lock()
		n := len(skillCmds.byKey[s.commandScopeKey()])
		skillCmds.mu.Unlock()
		mu.Lock()
		sizes = append(sizes, n)
		mu.Unlock()
		return ParseFrontmatter(content)
	})
	s.ScanSkillCommands()
	if len(sizes) != 5 {
		t.Fatalf("%v", sizes)
	}
	for _, n := range sizes {
		if n != 5 {
			t.Fatalf("partially built map observed %v", sizes)
		}
	}
}

func TestNonASCIINameRegistersCommand(t *testing.T) {
	s, skills, _ := cmdScope(t)
	d := filepath.Join(skills, "novel-clipper")
	os.MkdirAll(d, 0o755)
	name := "\xE5\xB0\x8F\xE8\xAF\xB4+\xE6\x8B\x86\xE6\x9D\xA1"
	slug := "\xE5\xB0\x8F\xE8\xAF\xB4\xE6\x8B\x86\xE6\x9D\xA1"
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Split novels into clips.\n---\n\nBody.\n"), 0o644)
	got := s.ScanSkillCommands()["/"+slug]
	if got == nil || got["name"] != name {
		t.Fatalf("%v", got)
	}
	if key, ok, _ := s.ResolveSkillCommandKey(context.Background(), slug, false); !ok || key != "/"+slug {
		t.Fatalf("%q", key)
	}
}

func TestResolveSkillCommandKeyForms(t *testing.T) {
	s, skills, _ := cmdScope(t)
	makeCmdSkill(t, skills, "claude-code", "", "", "")
	s.ScanSkillCommands()
	ctx := context.Background()
	for _, typed := range []string{"claude-code", "claude_code"} {
		if key, ok, _ := s.ResolveSkillCommandKey(ctx, typed, false); !ok || key != "/claude-code" {
			t.Fatalf("%s -> %q", typed, key)
		}
	}
	if _, ok, _ := s.ResolveSkillCommandKey(ctx, "does-not-exist", false); ok {
		t.Fatal("unknown command resolved")
	}
}

func TestBuildPreloadedSkillsPromptMultipleAndDisabled(t *testing.T) {
	s, skills, _ := cmdScope(t)
	makeCmdSkill(t, skills, "first-skill", "", "", "")
	makeCmdSkill(t, skills, "second-skill", "", "", "")
	prompt, loaded, missing := s.BuildPreloadedSkillsPrompt(context.Background(), []string{"first-skill", "second-skill"}, "", nil)
	if len(missing) != 0 || strings.Join(loaded, ",") != "first-skill,second-skill" || !strings.Contains(strings.ToLower(prompt), "preloaded") || !strings.Contains(prompt, "second-skill") {
		t.Fatalf("%v %v", loaded, missing)
	}
	makeCmdSkill(t, skills, "enabled-skill", "", "Enabled content.", "")
	makeCmdSkill(t, skills, "disabled-skill", "", "SECRET DISABLED CONTENT.", "")
	s.Config["skills"] = map[string]any{"disabled": []any{"disabled-skill"}}
	prompt, loaded, missing = s.BuildPreloadedSkillsPrompt(context.Background(), []string{"enabled-skill", "disabled-skill"}, "", nil)
	if strings.Join(loaded, ",") != "enabled-skill" || strings.Join(missing, ",") != "disabled-skill" || strings.Contains(prompt, "SECRET DISABLED CONTENT.") {
		t.Fatalf("%v %v", loaded, missing)
	}
}

func TestBuildSkillInvocationMessageSecureSetup(t *testing.T) {
	s, skills, _ := cmdScope(t)
	t.Setenv("TENOR_API_KEY", "")
	os.Unsetenv("TENOR_API_KEY")
	var calls []string
	SetSecretCaptureCallback(func(name, prompt string, metadata map[string]any) (map[string]any, error) {
		calls = append(calls, name)
		os.Setenv(name, "stored-in-test")
		return map[string]any{"success": true, "stored_as": name, "validated": false, "skipped": false}, nil
	})
	t.Cleanup(func() { SetSecretCaptureCallback(nil) })
	makeCmdSkill(t, skills, "test-skill", "required_environment_variables:\n  - name: TENOR_API_KEY\n    prompt: Tenor API key\n", "", "")
	s.ScanSkillCommands()
	if msg := mustInvoke(t, s, "/test-skill", "do stuff"); !strings.Contains(msg, "test-skill") || len(calls) != 1 || calls[0] != "TENOR_API_KEY" {
		t.Fatalf("%v", calls)
	}
}

func TestGatewayLoadsSkillButReturnsSetupGuidance(t *testing.T) {
	s, skills, _ := cmdScope(t)
	t.Setenv("TENOR_API_KEY", "")
	os.Unsetenv("TENOR_API_KEY")
	SetSecretCaptureCallback(func(string, string, map[string]any) (map[string]any, error) {
		t.Error("gateway flow should not try secure in-band secret capture")
		return nil, nil
	})
	t.Cleanup(func() { SetSecretCaptureCallback(nil) })
	t.Setenv("ICEBATH_SESSION_PLATFORM", "telegram")
	s.Platform = "telegram"
	makeCmdSkill(t, skills, "test-skill", "required_environment_variables:\n  - name: TENOR_API_KEY\n    prompt: Tenor API key\n", "", "")
	s.ScanSkillCommands()
	if msg := mustInvoke(t, s, "/test-skill", "do stuff"); !strings.Contains(strings.ToLower(msg), "local cli") {
		t.Fatal(msg)
	}
}

func TestInvocationHeaderTemplateVarsAndInlineShell(t *testing.T) {
	s, skills, _ := cmdScope(t)
	abs := makeCmdSkill(t, skills, "abs-dir-skill", "", "", "")
	tpl := makeCmdSkill(t, skills, "templated", "", "Run: node ${HERMES_SKILL_DIR}/scripts/foo.js", "")
	s.ScanSkillCommands()
	if msg := mustInvoke(t, s, "/abs-dir-skill", "go"); !strings.Contains(msg, abs) {
		t.Fatal(msg)
	}
	msg := mustInvoke(t, s, "/templated", "")
	if !strings.Contains(msg, "node "+tpl+"/scripts/foo.js") || strings.Contains(strings.SplitN(msg, "[Skill directory:", 2)[0], "${HERMES_SKILL_DIR}") {
		t.Fatal(msg)
	}
	s.Config["skills"] = map[string]any{"template_vars": false}
	makeCmdSkill(t, skills, "no-sub", "", "Run: node ${HERMES_SKILL_DIR}/scripts/foo.js", "")
	s.ScanSkillCommands()
	if msg := mustInvoke(t, s, "/no-sub", ""); !strings.Contains(msg, "${HERMES_SKILL_DIR}/scripts/foo.js") {
		t.Fatal(msg)
	}
	if runtime.GOOS != "linux" {
		return
	}
	s.Config["skills"] = map[string]any{"template_vars": true, "inline_shell": true, "inline_shell_timeout": 5}
	cwd := makeCmdSkill(t, skills, "dyn-cwd", "", "Here: !`pwd`", "")
	s.Config["skills"] = map[string]any{"template_vars": true, "inline_shell": true, "inline_shell_timeout": 5}
	s.ScanSkillCommands()
	if msg := mustInvoke(t, s, "/dyn-cwd", ""); !strings.Contains(msg, "Here: "+cwd) {
		t.Fatal(msg)
	}
	s.Config["skills"] = map[string]any{"template_vars": true, "inline_shell": true, "inline_shell_timeout": 1}
	makeCmdSkill(t, skills, "dyn-slow", "", "Slow: !`sleep 5 && printf DYN_MARKER`", "")
	s.ScanSkillCommands()
	msg = mustInvoke(t, s, "/dyn-slow", "")
	if !strings.Contains(msg, "inline-shell timeout") || strings.Contains(strings.ReplaceAll(msg, "sleep 5 && printf DYN_MARKER", ""), "DYN_MARKER") {
		t.Fatal(msg)
	}
}

func TestStackedSkillCommands(t *testing.T) {
	s, skills, _ := cmdScope(t)
	ctx := context.Background()
	for _, n := range []string{"skill-a", "skill-b", "skill-c"} {
		makeCmdSkill(t, skills, n, "", "Body "+strings.ToUpper(n[len(n)-1:])+".", "")
	}
	for i := 0; i < 7; i++ {
		makeCmdSkill(t, skills, "stk-"+itoa(i), "", "", "")
	}
	s.ScanSkillCommands()
	keys, instr, _ := s.SplitStackedSkillCommands(ctx, "/skill-b /not-a-skill /skill-c hello", false)
	if strings.Join(keys, ",") != "/skill-b" || instr != "/not-a-skill /skill-c hello" {
		t.Fatalf("%v %q", keys, instr)
	}
	var rest []string
	for i := 1; i < 7; i++ {
		rest = append(rest, "/stk-"+itoa(i))
	}
	keys, instr, _ = s.SplitStackedSkillCommands(ctx, strings.Join(rest, " ")+" run", false)
	if len(keys) != 4 || !strings.HasPrefix(instr, "/stk-5") {
		t.Fatalf("%v %q", keys, instr)
	}
	msg, loaded, missing, ok, err := s.BuildStackedSkillInvocationMessage(ctx, []string{"/skill-a", "/gone"}, "go", "")
	if err != nil || !ok || strings.Join(loaded, ",") != "skill-a" || strings.Join(missing, ",") != "gone" ||
		!strings.Contains(msg, "gone") || !strings.Contains(msg, "Body A.") {
		t.Fatalf("%v %v %v", loaded, missing, err)
	}
	if msg, ok, err := s.Command(ctx, "/skill-a /skill-b go now", ""); !ok || err != nil || !strings.Contains(msg, "Body A.") || !strings.Contains(msg, "Body B.") {
		t.Fatalf("%v %v", ok, err)
	}
}

func TestReloadSkillsDetectsRemovedSkillAndKeepsPromptSnapshot(t *testing.T) {
	s, _, _ := cmdScope(t)
	s.SkillsDirOverride = ""
	skills := filepath.Join(s.Home, "skills")
	d := filepath.Join(skills, "demo")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: demo\ndescription: soon to be gone\n---\nbody\n"), 0o644)
	snapshot := filepath.Join(s.Home, ".skills_prompt_snapshot.json")
	os.WriteFile(snapshot, []byte("{}"), 0o644)
	ctx := context.Background()
	first, err := s.ReloadSkills(ctx)
	if err != nil || first["total"] != 1 {
		t.Fatalf("%v %v", first, err)
	}
	if added := first["added"].([]map[string]any); len(added) != 1 || added[0]["name"] != "demo" || added[0]["description"] != "soon to be gone" {
		t.Fatalf("%v", added)
	}
	os.RemoveAll(d)
	second, _ := s.ReloadSkills(ctx)
	removed := second["removed"].([]map[string]any)
	if len(removed) != 1 || removed[0]["name"] != "demo" || removed[0]["description"] != "soon to be gone" ||
		len(second["added"].([]map[string]any)) != 0 || second["total"] != 0 {
		t.Fatalf("%v", second)
	}
	if !lexistsFollow(snapshot) {
		t.Fatal("reload invalidated the prompt cache snapshot")
	}
}

func TestBuildAutoLoadPromptLoadsConfiguredSkillsAndReportsMissing(t *testing.T) {
	s, _, _ := cmdScope(t)
	s.SkillsDirOverride = ""
	d := filepath.Join(s.Home, "skills", "pinned-skill")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: pinned-skill\ndescription: Test.\n---\n\nPINNED CONTENT\n"), 0o644)
	s.Config["skills"] = map[string]any{"auto_load": []any{"pinned-skill", " pinned-skill ", "no-such-skill", 7}}
	prompt, loaded, missing := s.BuildAutoLoadPrompt(context.Background(), "s1", nil)
	if strings.Join(loaded, ",") != "pinned-skill" || strings.Join(missing, ",") != "no-such-skill" ||
		!strings.Contains(prompt, "PINNED CONTENT") || strings.Count(prompt, "auto-loaded via config (skills.auto_load)") != 1 {
		t.Fatalf("%v %v", loaded, missing)
	}
}

func TestExplicitPreloadDedupesAgainstAutoLoadedNames(t *testing.T) {
	s, skills, _ := cmdScope(t)
	makeCmdSkill(t, skills, "pinned-skill", "", "PINNED CONTENT", "")
	makeCmdSkill(t, skills, "extra-skill", "", "EXTRA CONTENT", "")
	prompt, loaded, missing := s.BuildPreloadedSkillsPrompt(context.Background(), []string{"pinned-skill", "extra-skill"}, "s1", map[string]bool{"pinned-skill": true})
	if strings.Join(loaded, ",") != "pinned-skill,extra-skill" || len(missing) != 0 || strings.Contains(prompt, "PINNED CONTENT") || !strings.Contains(prompt, "EXTRA CONTENT") {
		t.Fatalf("%v %v", loaded, missing)
	}
}

func commandKeys(c SkillCommands) string {
	var keys []string
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func TestSkillAddedOnDiskAfterFirstScanIsServedWithoutReload(t *testing.T) {
	s, skills, _ := cmdScope(t)
	makeCmdSkill(t, skills, "first", "", "", "")
	if got := commandKeys(s.GetSkillCommands()); got != "/first" {
		t.Fatal(got)
	}
	makeCmdSkill(t, skills, "installed-elsewhere", "", "", "")
	if got := commandKeys(s.GetSkillCommands()); got != "/first,/installed-elsewhere" {
		t.Fatal(got)
	}
	os.RemoveAll(filepath.Join(skills, "first"))
	if got := commandKeys(s.GetSkillCommands()); got != "/installed-elsewhere" {
		t.Fatal(got)
	}
}

func TestUnchangedSkillsDirsReuseThePublishedMap(t *testing.T) {
	s, skills, _ := cmdScope(t)
	makeCmdSkill(t, skills, "steady", "", "", "")
	first := s.GetSkillCommands()
	if reflect.ValueOf(s.GetSkillCommands()).Pointer() != reflect.ValueOf(first).Pointer() {
		t.Fatal("unchanged dirs rescanned")
	}
}

func TestSkillInstalledUnderExistingCategorySubdirIsServed(t *testing.T) {
	s, skills, _ := cmdScope(t)
	training := filepath.Join(skills, "mlops", "training")
	makeCmdSkill(t, training, "existing", "", "", "")
	if got := commandKeys(s.GetSkillCommands()); got != "/existing" {
		t.Fatal(got)
	}
	makeCmdSkill(t, training, "new-nested", "", "", "")
	if got := commandKeys(s.GetSkillCommands()); got != "/existing,/new-nested" {
		t.Fatal(got)
	}
	before, _ := s.skillDirsSignature()
	os.MkdirAll(filepath.Join(training, "existing", "references", "deep"), 0o755)
	after, _ := s.skillDirsSignature()
	if strings.Contains(after, filepath.Join("existing", "references")) || before == after {
		t.Fatal("support dirs walked or child name change missed")
	}
}

func TestOnlyAnUnreadableSkillRootKeepsThePublishedMap(t *testing.T) {
	s, skills, _ := cmdScope(t)
	makeCmdSkill(t, skills, "kept", "", "", "")
	if got := commandKeys(s.GetSkillCommands()); got != "/kept" {
		t.Fatal(got)
	}
	real := skillDirsSignatureSteps.scan
	t.Cleanup(func() { skillDirsSignatureSteps.scan = real })
	skillDirsSignatureSteps.scan = func([]string, map[string]bool) (string, error) {
		return "", &fs.PathError{Op: "stat", Path: skills, Err: syscall.EACCES}
	}
	if _, has := s.skillDirsSignature(); has {
		t.Fatal("permission error produced a signature")
	}
	makeCmdSkill(t, skills, "added", "", "", "")
	if got := commandKeys(s.GetSkillCommands()); got != "/kept" {
		t.Fatal(got)
	}
	skillDirsSignatureSteps.scan = func([]string, map[string]bool) (string, error) { return "", errors.New("bug") }
	defer func() {
		if recover() == nil {
			t.Fatal("non OS error was swallowed")
		}
	}()
	s.skillDirsSignature()
}
