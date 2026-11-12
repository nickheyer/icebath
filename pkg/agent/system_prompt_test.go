package agent

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Replaces the hermes_time seams with a fixed clock and the strftime directives the prompt uses
func fixedPromptClock(t *testing.T, now time.Time) {
	t.Helper()
	prevNow, prevTZ, prevFmt := hermesNow, hermesTimezone, safeStrftime
	t.Cleanup(func() { hermesNow, hermesTimezone, safeStrftime = prevNow, prevTZ, prevFmt })
	hermesNow = func(context.Context) time.Time { return now }
	hermesTimezone = func(context.Context) (*time.Location, bool) { return now.Location(), true }
	safeStrftime = func(v time.Time, format string) string {
		return strings.NewReplacer("%A", v.Format("Monday"), "%B", v.Format("January"), "%d", v.Format("02"),
			"%Y", v.Format("2006"), "%Z", v.Format("MST"), "%z", v.Format("-0700")).Replace(format)
	}
}

// Replaces the file and host probes with empty results, the patched builders of the Python tests
func emptyPromptProbes(t *testing.T) {
	t.Helper()
	prevSoul, prevHints, prevFiles := loadSoulMDFn, buildEnvironmentHintsFn, buildContextFilesPromptFn
	t.Cleanup(func() {
		loadSoulMDFn, buildEnvironmentHintsFn, buildContextFilesPromptFn = prevSoul, prevHints, prevFiles
	})
	loadSoulMDFn = func(context.Context, *tools.Env, *int, string) (string, bool) { return "", true }
	buildEnvironmentHintsFn = func(context.Context, *tools.Env) (string, error) { return "", nil }
	buildContextFilesPromptFn = func(context.Context, *tools.Env, ContextFilesOptions) (string, error) { return "", nil }
}

func promptTestAgent(t *testing.T, platform string) *AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	empty := ""
	a := &AIAgent{platform: platform, env: &tools.Env{Values: &sync.Map{}}, validToolNames: map[string]bool{}}
	a.toolUseEnforcement = false
	a.botModeProtocol = true
	a.kanbanWorkerGuidance = &empty
	a.platformHintOverrides = map[string]any{}
	return a
}

func stablePrompt(t *testing.T, a *AIAgent) string {
	t.Helper()
	emptyPromptProbes(t)
	fixedPromptClock(t, time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC))
	parts, err := BuildSystemPromptParts(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	return parts.Stable
}

const (
	hintDefault = "You are on WhatsApp. Do not use markdown."
	hintExtra   = "When tabular output would help, invoke the table_formatting skill."
)

func TestResolvePlatformHint(t *testing.T) {
	appendAgent := &AIAgent{}
	appendAgent.platformHintOverrides = map[string]any{"whatsapp": map[string]any{"append": hintExtra}}
	t.Run("test_missing_attr_returns_default", func(t *testing.T) {
		if got := resolvePlatformHint(&AIAgent{}, "whatsapp", hintDefault); got != hintDefault {
			t.Fatalf("missing overrides %q", got)
		}
	})
	t.Run("test_append_dict", func(t *testing.T) {
		got := resolvePlatformHint(appendAgent, "whatsapp", hintDefault)
		if got != hintDefault+"\n\n"+hintExtra || !strings.Contains(got, hintDefault) || !strings.Contains(got, hintExtra) {
			t.Fatalf("append %q", got)
		}
	})
	t.Run("test_replace_dict", func(t *testing.T) {
		replaceAgent := &AIAgent{}
		replaceAgent.platformHintOverrides = map[string]any{"whatsapp": map[string]any{"replace": hintExtra}}
		if got := resolvePlatformHint(replaceAgent, "whatsapp", hintDefault); got != hintExtra || strings.Contains(got, hintDefault) {
			t.Fatalf("replace %q", got)
		}
	})
	t.Run("test_other_platform_unaffected", func(t *testing.T) {
		tg := "You are on Telegram. Markdown works."
		if got := resolvePlatformHint(appendAgent, "telegram", tg); got != tg {
			t.Fatalf("other platform %q", got)
		}
	})
	shorthand := &AIAgent{}
	shorthand.platformHintOverrides = map[string]any{"whatsapp": "  " + hintExtra + " "}
	if got := resolvePlatformHint(shorthand, "whatsapp", hintDefault); got != hintDefault+"\n\n"+hintExtra {
		t.Fatalf("bare string %q", got)
	}
	malformed := &AIAgent{}
	malformed.platformHintOverrides = map[string]any{"whatsapp": []any{"x"}, "slack": map[string]any{"append": 5, "replace": nil}}
	if resolvePlatformHint(malformed, "whatsapp", hintDefault) != hintDefault || resolvePlatformHint(malformed, "slack", hintDefault) != hintDefault {
		t.Fatal("malformed override changed the default")
	}
}

func TestDesktopPlatformYieldsDesktopHintNoTUIFraming(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DESKTOP_TERMINAL", "")
	stable := stablePrompt(t, promptTestAgent(t, "desktop"))
	if !strings.Contains(stable, PlatformHints["desktop"]) {
		t.Fatal("desktop hint missing")
	}
	for _, banned := range []string{"terminal UI", "Runtime surface:", "embedded terminal pane"} {
		if strings.Contains(stable, banned) {
			t.Fatalf("stable prompt carries %q", banned)
		}
	}
}

func TestEmbeddedTUIYieldsTUIHintWithClarifier(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP", "1")
	t.Setenv("ICEBATH_DESKTOP_TERMINAL", "1")
	stable := stablePrompt(t, promptTestAgent(t, "tui"))
	if !strings.Contains(stable, PlatformHints["tui"]) || !strings.Contains(stable, "embedded terminal pane") {
		t.Fatal("tui hint or clarifier missing")
	}
	if !strings.Contains(stable, "Shift-drag") && !strings.Contains(stable, "Option-drag") {
		t.Fatal("selection gesture missing")
	}
}

func TestTUIEmbeddedPaneClarifier(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP_TERMINAL", "1")
	once := tuiEmbeddedPaneClarifier(PlatformHints["tui"])
	if !strings.HasSuffix(once, tuiEmbeddedPaneClarifierText) || tuiEmbeddedPaneClarifier(once) != once {
		t.Fatal("clarifier not appended exactly once")
	}
	if tuiEmbeddedPaneClarifier("") != "" {
		t.Fatal("empty hint gained a clarifier")
	}
	for _, val := range []string{"0", "false", ""} {
		t.Setenv("ICEBATH_DESKTOP_TERMINAL", val)
		if got := tuiEmbeddedPaneClarifier(PlatformHints["tui"]); got != PlatformHints["tui"] {
			t.Fatalf("ICEBATH_DESKTOP_TERMINAL=%q triggered the clarifier", val)
		}
	}
}

// Registers gateway platform entries carrying the given hints, restoring the previous registrations after the case
func fakePlatformRegistry(t *testing.T, hints map[string]string) {
	t.Helper()
	for name, hint := range hints {
		prevEntry, prevLoader := gateway.Registry.SnapshotRegistration(name, "")
		entry := gateway.NewPlatformEntry(name, name, nil, nil)
		entry.PlatformHint = hint
		if err := gateway.Registry.Register(context.Background(), entry, ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { gateway.Registry.RestoreRegistration(name, entry, nil, prevEntry, prevLoader, "") })
	}
}

func TestCronAgentGetsDeliveryChannelHintWithItsOverride(t *testing.T) {
	t.Setenv("ICEBATH_DESKTOP_TERMINAL", "")
	slackHint := "You are on Slack. MEDIA: tags become attachments."
	fakePlatformRegistry(t, map[string]string{"slack": slackHint})
	extra := "Always attach the report as a .pdf."
	a := promptTestAgent(t, "cron")
	tools.BindSessionContext(a.env, map[string]string{"ICEBATH_CRON_AUTO_DELIVER_PLATFORM": "slack"})
	a.platformHintOverrides = map[string]any{"slack": map[string]any{"append": extra}}
	hint := PlatformHint(context.Background(), a)
	if !strings.HasPrefix(hint, PlatformHints["cron"]) || !strings.Contains(hint, slackHint) || !strings.Contains(hint, extra) {
		t.Fatalf("cron delivery hint %q", hint)
	}
	if !strings.Contains(hint, "Delivery destination (slack): ") {
		t.Fatalf("delivery label missing in %q", hint)
	}
	a.platformHintOverrides = map[string]any{"cron": map[string]any{"replace": "X"}}
	if got := PlatformHint(context.Background(), a); !strings.HasPrefix(got, "X\n\n") {
		t.Fatalf("cron replace %q", got)
	}
}

func TestDeliveryTargetOnlyAppliesToCronAgents(t *testing.T) {
	tgHint := "You are on Telegram."
	fakePlatformRegistry(t, map[string]string{"slack": "You are on Slack.", "telegram": tgHint})
	tg := promptTestAgent(t, "telegram")
	tools.BindSessionContext(tg.env, map[string]string{"ICEBATH_CRON_AUTO_DELIVER_PLATFORM": "slack"})
	if got := PlatformHint(context.Background(), tg); got != tgHint {
		t.Fatalf("telegram hint %q", got)
	}
	cron := promptTestAgent(t, "cron")
	tools.BindSessionContext(cron.env, map[string]string{"ICEBATH_CRON_AUTO_DELIVER_PLATFORM": ""})
	if got := PlatformHint(context.Background(), cron); got != PlatformHints["cron"] {
		t.Fatalf("cron without target %q", got)
	}
}

func TestSkillsPromptScopedToOverrideNotAmbientHome(t *testing.T) {
	tmp := t.TempDir()
	defaultHome := filepath.Join(tmp, "default")
	leaky := filepath.Join(defaultHome, "skills", "general", "leaky-skill")
	if err := os.MkdirAll(leaky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaky, "SKILL.md"), []byte("---\nname: leaky-skill\ndescription: should never appear in a bot prompt\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	botSkills := filepath.Join(tmp, "profiles", "emptybot", "skills")
	t.Setenv("ICEBATH_HOME", defaultHome)
	skills.ClearSkillsSystemPromptCache(defaultHome, false)
	var prompt string
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		prompt, err = skills.NewScope(defaultHome, nil).BuildSkillsSystemPrompt(context.Background(), skills.SkillsPromptOptions{SkillsDirOverride: botSkills})
	}()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)<available_skills>(.*?)</available_skills>`).FindStringSubmatch(prompt)
	if m != nil && strings.TrimSpace(m[1]) != "" {
		t.Fatalf("bot prompt carries ambient skills %q", m[1])
	}
	if strings.Contains(prompt, "leaky-skill") {
		t.Fatal("ambient skill leaked")
	}
}

func TestAgentHomeResolvesFromSessionDBPath(t *testing.T) {
	botHome := filepath.Join(t.TempDir(), "profiles", "mybot")
	if err := os.MkdirAll(botHome, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := state.OpenHome(botHome, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &AIAgent{sessionDB: db}
	if got := agentOwnHome(context.Background(), a); got != botHome {
		t.Fatalf("home %q", got)
	}
	if got := agentSkillsDir(context.Background(), a); got != filepath.Join(botHome, "skills") {
		t.Fatalf("skills dir %q", got)
	}
}

func TestAgentHomeNoneWithoutSessionDB(t *testing.T) {
	a := &AIAgent{}
	if agentOwnHome(context.Background(), a) != "" || agentSkillsDir(context.Background(), a) != "" {
		t.Fatal("home resolved without a session db")
	}
}

func profileHomes(t *testing.T) (string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	botHome := filepath.Join(tmp, "profiles", "mybot")
	otherHome := filepath.Join(tmp, "profiles", "other")
	for _, d := range []string{botHome, otherHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tmp, botHome, otherHome
}

func TestProfileNameCorrectOnBoundProfileSession(t *testing.T) {
	_, botHome, _ := profileHomes(t)
	t.Setenv("ICEBATH_HOME", botHome)
	if got := profileNameForHome(botHome); got != "mybot" {
		t.Fatalf("bound profile session %q", got)
	}
}

func TestProfileNameDefaultWhenHomeIsRoot(t *testing.T) {
	tmp, _, _ := profileHomes(t)
	t.Setenv("ICEBATH_HOME", tmp)
	if got := profileNameForHome(tmp); got != "default" {
		t.Fatalf("root home %q", got)
	}
}

func TestProfileNameCorrectWhenAmbientIsAnotherProfile(t *testing.T) {
	_, botHome, otherHome := profileHomes(t)
	t.Setenv("ICEBATH_HOME", otherHome)
	if got := profileNameForHome(botHome); got != "mybot" {
		t.Fatalf("ambient other profile %q", got)
	}
}

func TestSoulMDScopedToHomeOverrideNotAmbient(t *testing.T) {
	tmp := t.TempDir()
	defaultHome := filepath.Join(tmp, "default")
	botHome := filepath.Join(tmp, "profiles", "mybot")
	emptyBot := filepath.Join(tmp, "profiles", "emptybot")
	for _, d := range []string{defaultHome, botHome, emptyBot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(defaultHome, "SOUL.md"), []byte("DEFAULT SOUL — must never leak into a profile prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botHome, "SOUL.md"), []byte("BOT SOUL"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", defaultHome)
	var soul string
	done := make(chan struct{})
	go func() {
		defer close(done)
		soul, _ = LoadSoulMD(context.Background(), nil, nil, botHome)
	}()
	<-done
	if soul != "BOT SOUL" {
		t.Fatalf("override soul %q", soul)
	}
	if got, ok := LoadSoulMD(context.Background(), nil, nil, emptyBot); ok || got != "" {
		t.Fatalf("missing override soul %q %v", got, ok)
	}
	ambientHome := filepath.Join(tmp, "home")
	if err := os.MkdirAll(ambientHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ambientHome, "SOUL.md"), []byte("AMBIENT SOUL"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", ambientHome)
	if got, _ := LoadSoulMD(context.Background(), nil, nil, ""); got != "AMBIENT SOUL" {
		t.Fatalf("ambient soul %q", got)
	}
}

func TestLoadSoulMDDropsLegacyProtocolSection(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	legacy := "# Me\n\nBe terse.\n\n## Messaging other agents\n\nold plugin text\n- `bot-a`\n"
	if err := os.WriteFile(filepath.Join(h, "SOUL.md"), []byte(legacy+"\n## Symbols\n\nuse ◆\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	soul, _ := LoadSoulMD(context.Background(), nil, nil, h)
	if strings.Contains(soul, "Messaging other agents") || strings.Contains(soul, "old plugin text") {
		t.Fatalf("legacy section survived %q", soul)
	}
	if !strings.Contains(soul, "Be terse.") || !strings.Contains(soul, "use ◆") {
		t.Fatalf("neighbouring sections lost %q", soul)
	}
}

func TestRestorePluginPromptSectionsRoundTrip(t *testing.T) {
	frame := "<!-- icebath-plugin-sections:start -->\n## Plugin Context: notes.v1\n<!-- icebath-plugin-section-chars:5 -->\n\nhé ok\n<!-- icebath-plugin-sections:end -->"
	a := &AIAgent{}
	RestorePluginPromptSections(a, "identity\n\n"+frame+"\n\nConversation started: Monday")
	if len(a.pluginSectionsSnapshot) != 1 || a.pluginSectionsSnapshot[0].ID != "notes.v1" || a.pluginSectionsSnapshot[0].Content != "hé ok" {
		t.Fatalf("restored %+v", a.pluginSectionsSnapshot)
	}
	RestorePluginPromptSections(a, "identity\n\n"+frame+"\n\nTrailing user text")
	if !a.pluginSectionsFrozen || len(a.pluginSectionsSnapshot) != 0 {
		t.Fatal("frame without the conversation start line was accepted")
	}
	tampered := strings.Replace(frame, "chars:5", "chars:4", 1)
	RestorePluginPromptSections(a, tampered+"\n\nConversation started: Monday")
	if len(a.pluginSectionsSnapshot) != 0 {
		t.Fatal("non canonical frame was accepted")
	}
}

func TestModelGate(t *testing.T) {
	defaults := []string{"gpt", "qwen"}
	cases := []struct {
		setting any
		model   string
		want    bool
	}{
		{true, "claude", true}, {false, "gpt-5", false}, {"ALWAYS", "claude", true}, {"never", "gpt-5", false},
		{[]any{"Claude", 3}, "anthropic/claude-x", true}, {[]any{"llama"}, "gpt-5", false},
		{"auto", "Qwen3", true}, {nil, "claude", false},
	}
	for _, c := range cases {
		if got := modelGate(c.setting, c.model, defaults); got != c.want {
			t.Fatalf("modelGate(%v, %q) = %v", c.setting, c.model, got)
		}
	}
}

func TestTimestampLineAndTimelessBotPrompt(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	fixedPromptClock(t, time.Date(2026, 3, 4, 10, 0, 0, 0, loc))
	a := &AIAgent{model: "m", provider: "p", platform: "cli", sessionID: "20260301_120000_abc", passSessionID: true}
	line := timestampLine(context.Background(), a)
	want := "Conversation started: Sunday, March 01, 2026 (EST, UTC-05:00)\nToday's date (as of the last context rebuild): Wednesday, March 04, 2026"
	if !strings.HasPrefix(line, want) {
		t.Fatalf("line %q", line)
	}
	if !strings.HasSuffix(line, "\nSession ID: 20260301_120000_abc\nModel: m\nProvider: p\nPlatform: cli") {
		t.Fatalf("trailer %q", line)
	}
	a.botChatTimelessPrompt = true
	a.passSessionID = false
	if got := timestampLine(context.Background(), a); got != "Timezone: EST, UTC-05:00\nModel: m\nProvider: p\nPlatform: cli" {
		t.Fatalf("timeless %q", got)
	}
}

func TestPersistedWorkspaceBlock(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	block := tools.WorkspaceBlockHeader + "\n- Root: " + root + "\n- Branch: main"
	prompt := "stable\n\n" + block + "\n\nvolatile"
	if got, ok := persistedWorkspaceBlock(prompt, sub); !ok || got != block {
		t.Fatalf("ancestor root block %q %v", got, ok)
	}
	if got, ok := persistedWorkspaceBlock("no snapshot", sub); !ok || got != "" {
		t.Fatal("prompt without a snapshot")
	}
	if _, ok := persistedWorkspaceBlock(prompt, t.TempDir()); ok {
		t.Fatal("snapshot of another root adopted")
	}
}

func TestFormatToolsForSystemMessage(t *testing.T) {
	a := &AIAgent{}
	if got, _ := FormatToolsForSystemMessage(a); got != "[]" {
		t.Fatalf("empty %q", got)
	}
	a.tools = []any{map[string]any{"function": map[string]any{"name": "web", "description": "Söke", "parameters": map[string]any{"type": "object"}}}, map[string]any{"function": map[string]any{"name": "x"}}}
	got, err := FormatToolsForSystemMessage(a)
	want := `[{"name": "web", "description": "Söke", "parameters": {"type": "object"}, "required": null}, {"name": "x", "description": "", "parameters": {}, "required": null}]`
	if err != nil || got != want {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestInvalidateAndReconstructStaticPrefix(t *testing.T) {
	a := promptTestAgent(t, "desktop")
	stable := stablePrompt(t, a)
	a.usePromptCaching = true
	a.cachedSystemPrompt = stable + "\n\nvolatile"
	ReconstructStaticPrefix(context.Background(), a, nil, "restore")
	if a.cachedSystemPromptStatic != stable || a.staticRebuildFailedFor != "" {
		t.Fatalf("static %q failed %q", a.cachedSystemPromptStatic, a.staticRebuildFailedFor)
	}
	a.cachedSystemPrompt, a.cachedSystemPromptStatic = "different prompt", ""
	ReconstructStaticPrefix(context.Background(), a, nil, "restore")
	if a.cachedSystemPromptStatic != "" || a.staticRebuildFailedFor != "different prompt" {
		t.Fatal("mismatched rebuild was kept or not memoized")
	}
	a.pluginSectionsFrozen = true
	if err := InvalidateSystemPrompt(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if a.cachedSystemPrompt != "" || a.pluginSectionsFrozen {
		t.Fatal("invalidate left cached state")
	}
}
