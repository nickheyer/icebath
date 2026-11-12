package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
)

// Scripted curses surface recording every frame it draws
type toggleScreen struct {
	keys   []int
	rows   map[int]string
	frames []map[int]string
}

func (s *toggleScreen) Getch() (int, error) {
	if len(s.keys) == 0 {
		return 0, cliui.ErrInterrupt
	}
	k := s.keys[0]
	s.keys = s.keys[1:]
	return k, nil
}
func (s *toggleScreen) Timeout(int)          {}
func (s *toggleScreen) Clear()               { s.rows = map[int]string{} }
func (s *toggleScreen) GetMaxYX() (int, int) { return 24, 100 }
func (s *toggleScreen) AddNStr(y, x int, text string, n int, _ cliui.Attr) error {
	s.rows[y] = text
	return nil
}
func (s *toggleScreen) Refresh() error {
	frame := map[int]string{}
	for k, v := range s.rows {
		frame[k] = v
	}
	s.frames = append(s.frames, frame)
	return nil
}
func (s *toggleScreen) HasColors() bool             { return true }
func (s *toggleScreen) InitColors(bool) error       { return nil }
func (s *toggleScreen) InitPairs(map[int]int) error { return nil }
func (s *toggleScreen) Colors() int                 { return 256 }

func toggleConsole(open func() (cliui.Screen, func(), error)) *cliui.Console {
	return &cliui.Console{In: strings.NewReader(""), Out: io.Discard, Getenv: func(string) (string, bool) { return "", false },
		StdinTTY: func() bool { return true }, StdoutTTY: func() bool { return false }, OpenScreen: open}
}

// Picker world with bundled telegram and slack platforms, no provider categories and scripted answers
func pickerWorld(t *testing.T, ui *cliui.Console) (context.Context, string, func(config string, answers ...string) map[string]any) {
	t.Helper()
	root, h := pubWorld(t)
	bundled := filepath.Join(root, "core", "plugins")
	for _, leaf := range []string{"telegram", "slack"} {
		vtWrite(t, filepath.Join(bundled, "platforms", leaf, "plugin.yaml"), "name: "+leaf+"-platform\nkind: platform\n")
	}
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	ctx := home.WithOverride(context.Background(), h)
	savedUI, savedTTY, savedCats, savedInput := toggleUI, toggleStdinTTY, toggleProviderCategory, inputLine
	t.Cleanup(func() {
		toggleUI, toggleStdinTTY, toggleProviderCategory, inputLine = savedUI, savedTTY, savedCats, savedInput
	})
	toggleUI = func() *cliui.Console { return ui }
	toggleStdinTTY = func() bool { return true }
	toggleProviderCategory = func(context.Context, *cliui.Console) []providerCategory { return nil }
	run := func(config string, answers ...string) map[string]any {
		vtWrite(t, filepath.Join(h, "config.yaml"), config)
		inputLine = func(string) (string, error) {
			if len(answers) == 0 {
				return "", cliui.ErrEOF
			}
			a := answers[0]
			answers = answers[1:]
			return a, nil
		}
		if err := CmdToggle(ctx, io.Discard); err != nil {
			t.Fatal(err)
		}
		return pubPlugins(t, h)
	}
	return ctx, h, run
}

func toggleRowOf(t *testing.T, ctx context.Context, key string) string {
	t.Helper()
	entries, err := discoverAllPlugins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		if e.Key == key {
			return fmt.Sprint(i + 1)
		}
	}
	t.Fatalf("no row for %s", key)
	return ""
}

func noCurses() (cliui.Screen, func(), error) {
	return nil, nil, fmt.Errorf("%w: setupterm: could not find terminal", cliui.ErrCursesInit)
}

func TestOpenAndExitWithoutChangesWritesNothing(t *testing.T) {
	_, _, run := pickerWorld(t, toggleConsole(noCurses))
	plugins := run("plugins:\n  enabled: []\n  disabled: []\n", "")
	enabled, hasEnabled := plugins["enabled"].([]any)
	disabled, hasDisabled := plugins["disabled"].([]any)
	if len(plugins) != 2 || !hasEnabled || !hasDisabled || len(enabled) != 0 || len(disabled) != 0 {
		t.Fatalf("plugins %v", plugins)
	}
}

func TestOnlyFlippedRowsAreWritten(t *testing.T) {
	ctx, h, run := pickerWorld(t, toggleConsole(noCurses))
	vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: []\n  disabled: []\n")
	telegram, slack := toggleRowOf(t, ctx, "platforms/telegram"), toggleRowOf(t, ctx, "platforms/slack")
	plugins := run("plugins:\n  enabled: [some/unshown-plugin]\n  disabled: [platforms/telegram, telegram-platform]\n", telegram, slack, "")
	enabled := pubList(plugins, "enabled")
	sort.Strings(enabled)
	if got := pubList(plugins, "disabled"); strings.Join(got, ",") != "platforms/slack" || strings.Join(enabled, ",") != "platforms/telegram,some/unshown-plugin" {
		t.Fatalf("plugins %v", plugins)
	}
}

func TestCompositeScreenTogglesConfiguresAndSaves(t *testing.T) {
	opened := 0
	var screens []*toggleScreen
	keys := [][]int{{cliui.KeyDown, ' ', cliui.KeyEnd, cliui.KeyEnter}, {'q'}}
	ui := toggleConsole(func() (cliui.Screen, func(), error) {
		s := &toggleScreen{keys: keys[opened], rows: map[int]string{}}
		opened++
		screens = append(screens, s)
		return s, func() {}, nil
	})
	ctx, h, _ := pickerWorld(t, ui)
	vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: []\n  disabled: []\n")
	entries, err := discoverAllPlugins(ctx)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries %v %v", entries, err)
	}
	configured, current := 0, "built-in"
	toggleProviderCategory = func(context.Context, *cliui.Console) []providerCategory {
		return []providerCategory{{Title: "Memory Provider", Current: current, Configure: func() (bool, error) {
			configured++
			current = "honcho"
			return true, nil
		}}}
	}
	var out strings.Builder
	if err := CmdToggle(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if opened != 2 || configured != 1 {
		t.Fatalf("opened %d configured %d", opened, configured)
	}
	first := screens[0].frames[0]
	if first[0] != "Plugins" || first[3] != "  General Plugins" || !strings.HasPrefix(first[4], " → [✓] ") && !strings.HasPrefix(first[4], " → [ ] ") {
		t.Fatalf("first frame %v", first)
	}
	last := screens[1].frames[len(screens[1].frames)-1]
	found := false
	for _, row := range last {
		found = found || strings.Contains(row, "Memory Provider") && strings.Contains(row, "▸ honcho") && strings.HasPrefix(row, " → ")
	}
	if !found {
		t.Fatalf("category row not refreshed %v", last)
	}
	printed := out.String()
	if !strings.Contains(printed, "General plugins: 1 turned on, 0 turned off.") && !strings.Contains(printed, "General plugins: 0 turned on, 1 turned off.") {
		t.Fatalf("printed %s", printed)
	}
	if !strings.Contains(printed, "Memory provider:") || !strings.Contains(printed, "Changes take effect on next session.") {
		t.Fatalf("printed %s", printed)
	}
	plugins := pubPlugins(t, h)
	if len(pubList(plugins, "enabled"))+len(pubList(plugins, "disabled")) != 1 {
		t.Fatalf("one flipped row must be written %v", plugins)
	}
}

func TestCompositeFallbackPersistsTheCanonicalKey(t *testing.T) {
	_, h := pubWorld(t)
	ctx := home.WithOverride(context.Background(), h)
	key := "observability/trace-leaf"
	pubPluginTree(t, filepath.Join(h, "plugins", key), "name: trace-manifest\nversion: '1.0'\n")
	vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: []\n  disabled: ["+key+", trace-leaf, trace-manifest]\n")
	saved := inputLine
	t.Cleanup(func() { inputLine = saved })
	answers := []string{"1", ""}
	inputLine = func(string) (string, error) { a := answers[0]; answers = answers[1:]; return a, nil }
	disabled := map[string]bool{key: true}
	if err := runCompositeFallback(ctx, toggleConsole(noCurses), io.Discard, []string{key}, []string{"trace-manifest"}, map[int]bool{}, disabled, nil, nil); err != nil {
		t.Fatal(err)
	}
	plugins := pubPlugins(t, h)
	if got := pubList(plugins, "enabled"); strings.Join(got, ",") != key {
		t.Fatalf("enabled %v", got)
	}
}

func TestProviderDiscovery(t *testing.T) {
	h := t.TempDir()
	t.Setenv(home.EnvHome, h)
	vtWrite(t, filepath.Join(h, "config.yaml"), "context:\n  engine: compressor\n")
	if err := saveContextEngine(context.Background(), h, "lcm"); err != nil || getCurrentContextEngine(h) != "lcm" {
		t.Fatalf("save context engine %v %s", err, getCurrentContextEngine(h))
	}
	saved := toggleContextEngines
	t.Cleanup(func() { toggleContextEngines = saved })
	toggleContextEngines = func(context.Context) ([]ContextEngineInfo, error) { return nil, errors.New("no module") }
	if got, err := discoverContextEngineChoices(home.WithOverride(context.Background(), h)); err != nil || len(got) != 0 {
		t.Fatalf("engines %v %v", got, err)
	}
	prev := installedServices()
	t.Cleanup(func() { InstallServices(prev) })
	InstallServices(&Services{DiscoverMemoryProviders: func(context.Context) [][2]string { panic("broken provider scan") }})
	if got, err := discoverMemoryProviders(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("raising discovery %v %v", got, err)
	}
	InstallServices(&Services{})
	var missing *ServiceMissingError
	if _, err := discoverMemoryProviders(context.Background()); !errors.As(err, &missing) {
		t.Fatalf("missing service %v", err)
	}
}

func TestCursesRadiolistNonTTYReturnsDefault(t *testing.T) {
	ui := toggleConsole(noCurses)
	ui.StdinTTY = func() bool { return false }
	items := []cliui.RadioItem{cliui.PlainItem("a"), cliui.PlainItem("b"), cliui.PlainItem("c")}
	if got, err := ui.CursesRadiolist(context.Background(), "Pick one", items, 1, cliui.RadiolistOptions{}); err != nil || got != 1 {
		t.Fatalf("radiolist %d %v", got, err)
	}
}

func TestContextEngineDirsAndLoaderHelpers(t *testing.T) {
	h, bundled := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	ctx := home.WithOverride(context.Background(), h)
	write := func(dir, init, yaml string) {
		vtWrite(t, filepath.Join(dir, "__init__.py"), init)
		if yaml != "" {
			vtWrite(t, filepath.Join(dir, "plugin.yaml"), yaml)
		}
	}
	write(filepath.Join(bundled, "context_engine", "lcm"), "class X: pass\n", "description: bundled lcm\n")
	write(filepath.Join(h, "plugins", "lcm"), "ctx.register_context_engine(x)\n", "description: user lcm\n")
	write(filepath.Join(h, "plugins", "deep"), "\ufefffrom agent.context_engine import ContextEngine\n", "description: 0\n")
	write(filepath.Join(h, "plugins", "plain"), "def register(ctx): pass\n", "")
	write(filepath.Join(h, "plugins", "_hidden"), "ContextEngine\n", "")
	if err := os.MkdirAll(filepath.Join(h, "plugins", "noinit"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs, err := iterEngineDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dirs {
		names = append(names, d[0]+"="+filepath.Base(filepath.Dir(d[1])))
	}
	if strings.Join(names, ",") != "lcm=context_engine,deep=plugins" {
		t.Fatalf("dirs %v", names)
	}
	if ReadPluginDescription(filepath.Join(bundled, "context_engine", "lcm")) != "bundled lcm" || ReadPluginDescription(filepath.Join(h, "plugins", "deep")) != "" {
		t.Fatal("descriptions")
	}
	if d, ok := UserPluginsDir(ctx); !ok || d != filepath.Join(h, "plugins") {
		t.Fatalf("user dir %s %v", d, ok)
	}
	if _, ok := UserPluginsDir(home.WithOverride(context.Background(), t.TempDir())); ok {
		t.Fatal("a missing plugins dir is no user dir")
	}
	if ProbeAvailability(func() (any, error) { return nil, errors.New("boom") }) || ProbeAvailability(func() (any, error) { return nil, nil }) ||
		ProbeAvailability(func() (any, error) { panic("raised") }) || !ProbeAvailability(func() (any, error) { return struct{}{}, nil }) {
		t.Fatal("probe availability")
	}
}

// Answers parse as Python int does, any script digits and single underscores, huge values out of range
func TestToggleAnswerParsesLikePythonInt(t *testing.T) {
	for raw, want := range map[string]int{"3": 3, "+2": 2, "-1": -1, "1_0": 10, "٣": 3, "99999999999999999999999": 0} {
		if got, ok := pyIntAnswer(raw); !ok || got != want {
			t.Fatalf("%q = %d %v", raw, got, ok)
		}
	}
	for _, raw := range []string{"-_1", "1__0", "_1", "1_", "x", "1.0", ""} {
		if _, ok := pyIntAnswer(raw); ok {
			t.Fatalf("%q parsed", raw)
		}
	}
}
