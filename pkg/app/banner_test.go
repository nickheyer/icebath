package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Restores every banner cache, seam and update check signal after the test and starts it from a clean state
func resetBannerState(t *testing.T) {
	t.Helper()
	bannerMemoMu.Lock()
	saved := struct {
		skills, git, release bannerMemo
		result               *int64
		signal               *updateCheckSignal
		prefetch, deferred   bool
	}{availableSkillsCache, gitBannerStateCache, latestReleaseCache, updateResult, updateCheck, bannerPrefetchStarted, deferredNoticeStarted}
	availableSkillsCache, gitBannerStateCache, latestReleaseCache = bannerMemo{}, bannerMemo{}, bannerMemo{}
	updateResult, updateCheck, bannerPrefetchStarted, deferredNoticeStarted = nil, newUpdateCheckSignal(), false, false
	bannerMemoMu.Unlock()
	columns, findSkills, skin, identity, status := bannerTerminalColumns, bannerFindAllSkills, bannerActiveSkin, bannerCodeIdentity, bannerMCPStatus
	profile, codex, configured, aggregator := bannerActiveProfileName, bannerCodexRuntime, bannerMCPConfigured, bannerMoAAggregator
	recommended, managed, label := bannerRecommendedUpdate, bannerManagedUpdate, bannerVersionLabel
	t.Cleanup(func() {
		bannerActiveProfileName, bannerCodexRuntime, bannerMCPConfigured, bannerMoAAggregator = profile, codex, configured, aggregator
		bannerRecommendedUpdate, bannerManagedUpdate, bannerVersionLabel = recommended, managed, label
		bannerMemoMu.Lock()
		availableSkillsCache, gitBannerStateCache, latestReleaseCache = saved.skills, saved.git, saved.release
		updateResult, updateCheck, bannerPrefetchStarted, deferredNoticeStarted = saved.result, saved.signal, saved.prefetch, saved.deferred
		bannerMemoMu.Unlock()
		bannerTerminalColumns, bannerFindAllSkills, bannerActiveSkin, bannerCodeIdentity, bannerMCPStatus = columns, findSkills, skin, identity, status
	})
}

// Pins the memoized skills to the given categories, the role a patched get_available_skills plays
func pinBannerSkills(skills *pyjson.Object) {
	bannerMemoMu.Lock()
	availableSkillsCache = bannerMemo{set: true, value: skills}
	bannerMemoMu.Unlock()
}

// Pins the memoized git state and release tag to none
func pinNoGitState() {
	bannerMemoMu.Lock()
	gitBannerStateCache = bannerMemo{set: true, value: (*BannerGitState)(nil)}
	latestReleaseCache = bannerMemo{set: true, value: (*[2]string)(nil)}
	bannerMemoMu.Unlock()
}

var noUnavailableToolsets = map[string]any{"unavailable_toolsets": []any{}, "lazy_tools": []any{}, "disabled_tools": []any{}}

func TestBannerSnapshotAcceptsBomWithoutWeakeningFreshness(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	resetBannerState(t)
	pinNoGitState()
	pinBannerSkills(orderedObject("notes", []any{"café"}))
	ctx := context.Background()
	saveBannerSnapshot(ctx, []map[string]any{{"function": map[string]any{"name": "read_file"}}}, []string{"file"}, map[string]any{}, map[string]any{"read_file": "file"})
	path := bannerSnapshotPath(ctx)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		t.Fatal("snapshot written with a BOM")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode %v", info.Mode().Perm())
	}
	var expected any
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("\xef\xbb\xbf"), raw...), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := loadBannerSnapshot(ctx, []string{"file"})
	if loaded == nil {
		t.Fatal("BOM prefixed snapshot rejected")
	}
	text, err := pyjson.Dumps(loaded)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("loaded %v want %v", got, expected)
	}
	if loadBannerSnapshot(ctx, []string{"web"}) != nil {
		t.Fatal("snapshot of other toolsets accepted")
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("display: {skin: mono}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if loadBannerSnapshot(ctx, []string{"file"}) != nil {
		t.Fatal("snapshot survived a config change")
	}
}

func TestCprintFallsBackToPlainPrintWhenPromptToolkitHasNoConsole(t *testing.T) {
	out, _ := captureStreams(t)
	cprint("fallback text")
	if out.String() != "fallback text\n" {
		t.Fatalf("printed %q", out.String())
	}
}

func TestEmptyModelShowsNoModelConfigured(t *testing.T) {
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), ".icebath"))
	if err := os.MkdirAll(os.Getenv("ICEBATH_HOME"), 0o755); err != nil {
		t.Fatal(err)
	}
	resetBannerState(t)
	pinNoGitState()
	pinBannerSkills(pyjson.NewObject())
	currentUpdateCheck().set()
	bannerMCPStatus = func(context.Context) []mcp.ServerStatus { return nil }
	var buf bytes.Buffer
	console := &pyrich.Console{Out: &buf, Width: 160}
	if err := buildWelcomeBanner(context.Background(), console, welcomeBanner{Model: "", Cwd: "/tmp/project", Tools: []any{}, Provider: "auto",
		Availability: noUnavailableToolsets}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no model configured") {
		t.Fatalf("banner %q", buf.String())
	}
}

func TestBuildWelcomeBannerDoesNotCenterPadHeroArt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	pinNoGitState()
	pinBannerSkills(pyjson.NewObject())
	currentUpdateCheck().set()
	bannerMCPStatus = func(context.Context) []mcp.ServerStatus { return nil }
	bannerActiveSkin = func(context.Context) *config.SkinConfig {
		return &config.SkinConfig{BannerHero: "[green]⠀X[/]", BannerLogo: ""}
	}
	bannerTerminalColumns = func() int { return 80 }
	var buf bytes.Buffer
	if err := buildWelcomeBanner(context.Background(), &pyrich.Console{Out: &buf, Width: 80}, welcomeBanner{Model: "m", Cwd: "/tmp", Tools: []any{},
		ToolsetForTool: func(string) string { return "" }, Availability: noUnavailableToolsets}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "⠀X") {
			if !strings.HasPrefix(line, "│  ⠀X") {
				t.Fatalf("hero line %q", line)
			}
			return
		}
	}
	t.Fatalf("no hero line in %q", buf.String())
}

func TestBakedBannerUsesLiveIdentity(t *testing.T) {
	resetBannerState(t)
	bannerCodeIdentity = func(bool) (*pyjson.Object, error) { return orderedObject("short_sha", "a1b2c3d4"), nil }
	state := bakedBannerState()
	if state == nil || *state != (BannerGitState{Upstream: "a1b2c3d4", Local: "a1b2c3d4", Ahead: 0}) {
		t.Fatalf("baked state %+v", state)
	}
}

func TestGetAvailableSkillsDelegatesToFindAllSkills(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	bannerFindAllSkills = func(context.Context) ([]map[string]any, error) {
		return []map[string]any{{"name": "skill-a", "description": "A skill", "category": "tools"},
			{"name": "skill-b", "description": "B skill", "category": "tools"}, {"name": "skill-c", "description": "C skill", "category": "creative"}}, nil
	}
	result := getAvailableSkills(context.Background())
	toolNames, _ := result.Get("tools")
	creative, _ := result.Get("creative")
	if !reflect.DeepEqual(toolNames, []any{"skill-a", "skill-b"}) || !reflect.DeepEqual(creative, []any{"skill-c"}) {
		t.Fatalf("grouped %v %v", toolNames, creative)
	}
	if !reflect.DeepEqual(result.Keys, []string{"tools", "creative"}) {
		t.Fatalf("category order %v", result.Keys)
	}
}

func TestGetAvailableSkillsNullCategoryBecomesGeneral(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	bannerFindAllSkills = func(context.Context) ([]map[string]any, error) {
		return []map[string]any{{"name": "orphan-skill", "description": "No cat", "category": nil}}, nil
	}
	general, _ := getAvailableSkills(context.Background()).Get("general")
	if !reflect.DeepEqual(general, []any{"orphan-skill"}) {
		t.Fatalf("general %v", general)
	}
}

// Banner text for the given skills on a terminal of termWidth columns
func buildBannerWithSkills(t *testing.T, skills *pyjson.Object, termWidth int) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	pinNoGitState()
	pinBannerSkills(skills)
	currentUpdateCheck().set()
	bannerMCPStatus = func(context.Context) []mcp.ServerStatus { return nil }
	bannerTerminalColumns = func() int { return termWidth }
	var buf bytes.Buffer
	if err := buildWelcomeBanner(context.Background(), &pyrich.Console{Out: &buf, Width: termWidth}, welcomeBanner{Model: "anthropic/test-model",
		Cwd: "/tmp/project", Tools: []any{}, Availability: noUnavailableToolsets}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestWideTerminalShowsMoreThan8Skills(t *testing.T) {
	names := make([]any, 15)
	for i := range names {
		names[i] = "skill-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	text := buildBannerWithSkills(t, orderedObject("research", names), 200)
	if !strings.Contains(text, "skill-08") {
		t.Fatalf("wide terminal banner lacks skill-08: %s", text)
	}
}

func TestSmallCategoryShowsAllSkills(t *testing.T) {
	text := buildBannerWithSkills(t, orderedObject("security", []any{"auth", "vault"}), 80)
	if !strings.Contains(text, "auth") || !strings.Contains(text, "vault") || strings.Contains(text, "+2 more") {
		t.Fatalf("small category banner %s", text)
	}
}

// Writes a desktop app install stamp into a fresh tree and makes it the tree of the running binary
func desktopStamp(t *testing.T, payload, updateMechanism string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "app-"+payload+"-"+updateMechanism)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp, _ := json.Marshal(map[string]any{"source": "local", "distribution": "desktop-app", "payload": payload, "updateMechanism": updateMechanism,
		"commit": strings.Repeat("a", 40), "displayVersion": "0.17.6", "baseVersion": "0.17.6", "tag": nil})
	if err := os.WriteFile(filepath.Join(root, "install-stamp.json"), stamp, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pm.PatchRepoRoot(root))
	t.Setenv("ICEBATH_INSTALL_ROOT", "")
	os.Unsetenv("ICEBATH_INSTALL_ROOT")
	updaterecord.ResetVersionInfoCache()
	t.Cleanup(updaterecord.ResetVersionInfoCache)
	resetBannerState(t)
}

func TestBootstrapShellBannerSaysInstaller(t *testing.T) {
	desktopStamp(t, "bootstrap", "self")
	label := formatBannerVersionLabel()
	if !strings.Contains(label, "0.17.6") || !strings.Contains(label, "installer") {
		t.Fatalf("label %q", label)
	}
}

func TestBundledReleaseBannerDoesNotSayInstaller(t *testing.T) {
	desktopStamp(t, "bundled", "electron-updater")
	if label := formatBannerVersionLabel(); strings.Contains(label, "installer") {
		t.Fatalf("label %q", label)
	}
}

func TestBannerUpdateCheckNonBlockingBannerDoesNotBlockOnPendingUpdateCheck(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	start := time.Now()
	behind := getUpdateResult(50 * time.Millisecond)
	if behind == nil && !currentUpdateCheck().isSet() {
		deferUpdateNotice(context.Background(), 30*time.Second)
	}
	if elapsed := time.Since(start); elapsed >= 300*time.Millisecond {
		t.Fatalf("banner update check blocked %v", elapsed)
	}
	currentUpdateCheck().set()
}

func TestBannerUpdateCheckNonBlockingDeferredNoticePrintsThroughPromptToolkitRenderer(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	out := &lockedBuffer{}
	t.Cleanup(SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}, &bytes.Buffer{}))
	deferUpdateNotice(context.Background(), 5*time.Second)
	three := int64(3)
	bannerMemoMu.Lock()
	updateResult = &three
	bannerMemoMu.Unlock()
	currentUpdateCheck().set()
	deadline := time.Now().Add(5 * time.Second)
	printed := ""
	for time.Now().Before(deadline) {
		printed = out.String()
		if strings.HasSuffix(printed, "\n") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if printed == "" {
		t.Fatal("deferred update notice never reached the console")
	}
	if !strings.Contains(printed, "\x1b[1;33m") {
		t.Fatalf("notice not rendered as ANSI: %q", printed)
	}
	visible := stripANSIForTest(printed)
	if !strings.Contains(visible, "3 commits behind") || strings.Contains(visible, "\x1b") || strings.Contains(visible, "[bold") {
		t.Fatalf("visible notice %q", visible)
	}
}

// Buffer safe to write from the notice goroutine while the test polls it
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Text with every SGR sequence removed
func stripANSIForTest(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestBannerHelpersMatchTheReferenceFormatting(t *testing.T) {
	for tokens, want := range map[int]string{128000: "128K", 1048576: "1M", 200000: "200K", 1500000: "1.5M", 999: "999", 32768: "32.8K", 2500: "2.5K"} {
		if got := formatContextLength(tokens); got != want {
			t.Errorf("formatContextLength(%d) = %q want %q", tokens, got, want)
		}
	}
	if displayToolsetName("file_tools") != "file" || displayToolsetName("") != "unknown" || displayToolsetName("web") != "web" {
		t.Error("displayToolsetName")
	}
	long := strings.Repeat("m", 29)
	if shortLabel(long) != strings.Repeat("m", 25)+"..." || shortLabel(strings.Repeat("m", 28)) != strings.Repeat("m", 28) {
		t.Error("shortLabel")
	}
	if got := packSkillNames([]string{"alpha", "beta", "gamma", "delta"}, 14); got != "alpha, +3 more" {
		t.Errorf("packSkillNames %q", got)
	}
	shown, elided := truncateToolNames([]string{"browser_click", "browser_navigate", "browser_snapshot", "browser_vision"})
	if !elided || !reflect.DeepEqual(shown, []string{"browser_click", "browser_navigate"}) {
		t.Errorf("truncateToolNames %v %v", shown, elided)
	}
	if got := mcpFailedLine("gh", "stdio", "HTTP 401  Unauthorized\n[token]"); got != "[red]gh[/] [dim](stdio)[/] [red]— could not connect:[/] HTTP 401 Unauthorized \\[token] [dim]— run `icebath mcp login gh`[/]" {
		t.Errorf("mcpFailedLine %q", got)
	}
	if got := mcpFailedLine("gh", "stdio", ""); !strings.Contains(got, "no details recorded") || !strings.Contains(got, "icebath mcp test gh") {
		t.Errorf("mcpFailedLine empty %q", got)
	}
	if got := mcpServerLine(mcp.ServerStatus{Name: "s", Transport: "http", Tools: 4, Connected: true}, "#B8860B", "#FFF8DC"); got != "[dim #B8860B]s[/] [#FFF8DC](http)[/] [dim #B8860B]—[/] [#FFF8DC]4 tool(s)[/]" {
		t.Errorf("mcpServerLine connected %q", got)
	}
	if got := mcpServerLine(mcp.ServerStatus{Name: "s", Transport: "stdio", Status: "connecting"}, "#B8860B", "#FFF8DC"); got != "[dim #B8860B]s[/] [dim](stdio)[/] [yellow]— connecting[/]" {
		t.Errorf("mcpServerLine connecting %q", got)
	}
}

func TestBannerToolLinesGroupSortColorAndCapToolsets(t *testing.T) {
	defs := []any{}
	lookup := map[string]string{}
	for i := 0; i < 10; i++ {
		name := "tool" + string(rune('a'+i))
		defs = append(defs, map[string]any{"function": map[string]any{"name": name}})
		lookup[name] = "set" + string(rune('a'+i)) + "_tools"
	}
	unavailable := []any{map[string]any{"name": "seta", "tools": []any{"toola", "lazyone"}}, map[string]any{"name": "zzz", "tools": []any{"deadone"}}}
	lines, err := bannerToolLines(defs, unavailable, func(name string) string { return lookup[name] },
		map[string]bool{"lazyone": true}, map[string]bool{"deadone": true}, "#A", "#D", "#T")
	if err != nil {
		t.Fatal(err)
	}
	if lines[0] != "[bold #A]Available Tools[/]" || lines[1] != "[dim #D]seta:[/] [yellow]lazyone[/], [#T]toola[/]" {
		t.Fatalf("lines %q", lines[:2])
	}
	if len(lines) != 10 || lines[9] != "[dim #D](and 3 more toolsets...)[/]" {
		t.Fatalf("tail %q of %d lines", lines[len(lines)-1], len(lines))
	}
	if _, err := bannerToolLines([]any{map[string]any{"name": "flat"}}, nil, func(string) string { return "" }, nil, nil, "#A", "#D", "#T"); err == nil {
		t.Fatal("tool definition without a function entry accepted")
	}
}

var bannerLinkIDRE = regexp.MustCompile("\x1b\\]8;id=[^;]*;")

func TestWelcomeBannerMatchesTheReferenceRendering(t *testing.T) {
	raw, err := os.ReadFile("testdata/welcome_banner_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Toolsets  map[string]string `json:"toolsets"`
		Scenarios map[string]struct {
			Kw struct {
				Model           string   `json:"model"`
				Cwd             string   `json:"cwd"`
				SessionID       *string  `json:"session_id"`
				ContextLength   int      `json:"context_length"`
				Provider        string   `json:"provider"`
				EnabledToolsets []string `json:"enabled_toolsets"`
				ContextPinned   bool     `json:"context_pinned"`
			} `json:"kw"`
			Tools   []string           `json:"tools"`
			Avail   map[string]any     `json:"avail"`
			Skills  map[string]any     `json:"skills"`
			MCP     []mcp.ServerStatus `json:"mcp"`
			Behind  *int64             `json:"behind"`
			Label   string             `json:"label"`
			Release *[2]string         `json:"release"`
			Profile *string            `json:"profile"`
			Codex   bool               `json:"codex"`
			Env     map[string]string  `json:"env"`
			Agg     string             `json:"agg"`
		} `json:"scenarios"`
		Cases []struct {
			Name   string `json:"name"`
			Width  int    `json:"width"`
			System string `json:"system"`
			Want   string `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range spec.Cases {
		sc := spec.Scenarios[c.Name]
		t.Run(fmt.Sprintf("%s_%d_%s", c.Name, c.Width, c.System), func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			t.Setenv("ICEBATH_YOLO_MODE", sc.Env["HERMES_YOLO_MODE"])
			resetBannerState(t)
			bannerMemoMu.Lock()
			gitBannerStateCache = bannerMemo{set: true, value: (*BannerGitState)(nil)}
			latestReleaseCache = bannerMemo{set: true, value: sc.Release}
			updateResult = sc.Behind
			bannerMemoMu.Unlock()
			currentUpdateCheck().set()
			bannerVersionLabel = func() string { return sc.Label }
			bannerActiveProfileName = func(context.Context) string {
				if sc.Profile == nil {
					return ""
				}
				return *sc.Profile
			}
			bannerCodexRuntime = func(context.Context) (bool, error) { return sc.Codex, nil }
			bannerMCPConfigured = func(context.Context) bool { return len(sc.MCP) > 0 }
			bannerMoAAggregator = func(context.Context, string) (string, error) { return sc.Agg, nil }
			bannerMCPStatus = func(context.Context) []mcp.ServerStatus { return sc.MCP }
			bannerRecommendedUpdate = func(context.Context, string) (string, error) { return "icebath update", nil }
			bannerManagedUpdate = func(context.Context, string) string { return "nix profile upgrade icebath" }
			bannerTerminalColumns = func() int { return c.Width }
			defs := make([]any, len(sc.Tools))
			for i, name := range sc.Tools {
				defs[i] = map[string]any{"function": map[string]any{"name": name}}
			}
			sessionID := ""
			if sc.Kw.SessionID != nil {
				sessionID = *sc.Kw.SessionID
			}
			var buf bytes.Buffer
			console := &pyrich.Console{Out: &buf, Color: c.System != "none", Width: c.Width, ColorSystem: c.System}
			if c.System == "none" {
				console.ColorSystem = ""
			}
			if err := buildWelcomeBanner(context.Background(), console, welcomeBanner{Model: sc.Kw.Model, Cwd: sc.Kw.Cwd, Tools: defs,
				EnabledToolsets: sc.Kw.EnabledToolsets, SessionID: sessionID, ToolsetForTool: func(name string) string { return spec.Toolsets[name] },
				ContextLength: sc.Kw.ContextLength, Provider: sc.Kw.Provider, Availability: sc.Avail, SkillsByCategory: sc.Skills,
				ContextPinned: sc.Kw.ContextPinned}); err != nil {
				t.Fatal(err)
			}
			got := bannerLinkIDRE.ReplaceAllString(buf.String(), "\x1b]8;id=;")
			want := bannerLinkIDRE.ReplaceAllString(c.Want, "\x1b]8;id=;")
			if got != want {
				t.Errorf("banner differs\n got %q\nwant %q", got, want)
			}
		})
	}
}

// Runs git in dir with a fixed identity, failing the test on error
func bannerGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitBannerStateReadsUpstreamLocalAndCarriedCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	repo := t.TempDir()
	bannerGit(t, repo, "init", "-q", "-b", "main")
	bannerGit(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	bannerGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	bannerGit(t, repo, "tag", "v9.9.9")
	bannerGit(t, repo, "commit", "-q", "--allow-empty", "-m", "two")
	bannerGit(t, repo, "commit", "-q", "--allow-empty", "-m", "three")
	ctx := context.Background()
	state := getGitBannerState(ctx, repo)
	if state == nil || state.Ahead != 2 || len(state.Upstream) != 8 || len(state.Local) != 8 || state.Upstream == state.Local {
		t.Fatalf("state %+v", state)
	}
	release := getLatestReleaseTag(ctx, repo)
	if release == nil || release[0] != "v9.9.9" || release[1] != "https://github.com/NousResearch/hermes-agent/releases/tag/v9.9.9" {
		t.Fatalf("release %v", release)
	}
	bannerCodeIdentity = func(bool) (*pyjson.Object, error) { return orderedObject("short_sha", nil), nil }
	if got := getGitBannerState(ctx, t.TempDir()); got != nil {
		t.Fatalf("state of a tree with no checkout %+v", got)
	}
	t.Cleanup(pm.PatchRepoRoot(repo))
	updaterecord.ResetVersionInfoCache()
	t.Cleanup(updaterecord.ResetVersionInfoCache)
	label := FormatBannerVersionLabel(ctx)
	want := " · upstream " + state.Upstream + " · local " + state.Local + " (+2 carried commits)"
	if !strings.HasPrefix(label, "Icebath Agent v") || !strings.Contains(label, "("+ReleaseDate+")") || !strings.HasSuffix(label, want) {
		t.Fatalf("label %q want suffix %q", label, want)
	}
}

func TestLatestReleaseTagCachesAMissAndARoutedHomeBypassesTheCache(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	t.Cleanup(pm.PatchRepoRoot(t.TempDir()))
	ctx := context.Background()
	if getLatestReleaseTag(ctx, "") != nil {
		t.Fatal("release tag without a checkout")
	}
	bannerMemoMu.Lock()
	cached := latestReleaseCache.set
	bannerMemoMu.Unlock()
	if !cached {
		t.Fatal("a miss was not cached")
	}
	calls := 0
	compute := func() (any, bool) {
		calls++
		return calls, true
	}
	var slot bannerMemo
	if memoBanner(ctx, &slot, compute) != 1 || memoBanner(ctx, &slot, compute) != 1 || calls != 1 {
		t.Fatalf("memo computed %d times", calls)
	}
	routed := home.WithOverride(ctx, t.TempDir())
	if memoBanner(routed, &slot, compute) != 2 || memoBanner(routed, &slot, compute) != 3 {
		t.Fatalf("routed home read the cache after %d computes", calls)
	}
	uncached := bannerMemo{}
	if memoBanner(ctx, &uncached, func() (any, bool) { return "x", false }); uncached.set {
		t.Fatal("an uncacheable result was stored")
	}
}

func TestBannerConfigProbesReadTheActiveHome(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	resetBannerState(t)
	ctx := context.Background()
	if active, err := codexRuntimeActive(ctx); err != nil || active {
		t.Fatalf("codex runtime on an empty home %v %v", active, err)
	}
	doc := "model:\n  openai_runtime: codex_app_server\nmcp_servers:\n  gh:\n    command: gh-mcp\nmoa:\n  presets:\n    deep-think:\n" +
		"      aggregator:\n        provider: openrouter\n        model: anthropic/claude-opus-4.6\n      references:\n" +
		"        - provider: openrouter\n          model: openai/gpt-5\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if active, err := codexRuntimeActive(ctx); err != nil || !active {
		t.Fatalf("codex runtime %v %v", active, err)
	}
	if !mcpConfigured(ctx) {
		t.Fatal("configured MCP server not seen")
	}
	if label, err := moaAggregatorLabel(ctx, "deep-think"); err != nil || label != "claude-opus-4.6" {
		t.Fatalf("aggregator %q %v", label, err)
	}
	if label, err := moaAggregatorLabel(ctx, "absent"); err != nil || label != "" {
		t.Fatalf("aggregator of an absent preset %q %v", label, err)
	}
}

func TestPrefetchUpdateCheckDeliversBehindAndSkipsInsideTests(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	PrefetchUpdateCheck()
	if !currentUpdateCheck().isSet() || getUpdateResult(10*time.Millisecond) != nil {
		t.Fatal("skipped prefetch did not finish with an unknown result")
	}
	resetBannerState(t)
	savedSkip, savedCheck := skipBackgroundPrefetch, bannerUpdateCheck
	t.Cleanup(func() { skipBackgroundPrefetch, bannerUpdateCheck = savedSkip, savedCheck })
	skipBackgroundPrefetch = func() bool { return false }
	bannerUpdateCheck = func(context.Context) (*pyjson.Object, error) { return orderedObject("behind", int64(4)), nil }
	PrefetchUpdateCheck()
	if behind := getUpdateResult(5 * time.Second); behind == nil || *behind != 4 {
		t.Fatalf("behind %v", behind)
	}
	bannerRecommendedUpdate = func(context.Context, string) (string, error) { return "icebath update", nil }
	bannerManagedUpdate = func(context.Context, string) string { return "" }
	ctx := context.Background()
	if notice, err := formatUpdateNotice(ctx, 1); err != nil || notice != "[bold yellow]⚠ 1 commit behind[/][dim yellow] — run [bold]icebath update[/bold] to update[/]" {
		t.Fatalf("notice %q %v", notice, err)
	}
	if notice, _ := formatUpdateNotice(ctx, -1); notice != "[bold yellow]⚠ update available[/]" {
		t.Fatalf("uncounted notice %q", notice)
	}
	if checkViaPyPI() != nil {
		t.Fatal("registry query reported a distance")
	}
}

func TestPrefetchBannerDataWarmsTheCachesOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetBannerState(t)
	t.Cleanup(pm.PatchRepoRoot(t.TempDir()))
	savedSkip := skipBackgroundPrefetch
	t.Cleanup(func() { skipBackgroundPrefetch = savedSkip })
	skipBackgroundPrefetch = func() bool { return false }
	bannerCodeIdentity = func(bool) (*pyjson.Object, error) { return orderedObject("short_sha", "feedc0de"), nil }
	scans := make(chan struct{}, 4)
	bannerFindAllSkills = func(context.Context) ([]map[string]any, error) {
		scans <- struct{}{}
		return []map[string]any{{"name": "s", "category": "c"}}, nil
	}
	PrefetchBannerData()
	PrefetchBannerData()
	select {
	case <-scans:
	case <-time.After(5 * time.Second):
		t.Fatal("prefetch never scanned the skills")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		bannerMemoMu.Lock()
		warm := availableSkillsCache.set && gitBannerStateCache.set && latestReleaseCache.set
		bannerMemoMu.Unlock()
		if warm {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state := GitBannerState(); state == nil || state.Local != "feedc0de" {
		t.Fatalf("warmed git state %+v", state)
	}
	if len(scans) != 0 {
		t.Fatal("second prefetch scanned again")
	}
	ResetAvailableSkillsCache()
	getAvailableSkills(context.Background())
	if len(scans) != 1 {
		t.Fatal("reset cache did not rescan")
	}
}
