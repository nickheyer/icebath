package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	toggleStdinTTY         = func() bool { return xterm.IsTerminal(os.Stdin.Fd()) }
	toggleUI               = cliui.Default
	toggleContextEngines   = DiscoverContextEngines
	toggleProviderCategory = providerCategories
)

// Provider category row of the composite screen with its current label and picker
type providerCategory struct {
	Title     string
	Current   string
	Configure func() (bool, error)
}

// One provider category with its default, current value reader, discovery and saver
type providerCategorySpec struct {
	Title, DefaultLabel, DefaultName string
	Current                          func(h string) string
	Discover                         func(ctx context.Context) ([][2]string, error)
	Save                             func(ctx context.Context, h, value string) error
}

var providerCategorySpecs = []providerCategorySpec{
	{"Memory Provider", "built-in", "", getCurrentMemoryProvider, discoverMemoryProviders, saveMemoryProvider},
	{"Context Engine", "compressor", "compressor", getCurrentContextEngine, discoverContextEngineChoices, saveContextEngine},
}

// Memory provider names and descriptions, a raising discovery answering none
func discoverMemoryProviders(ctx context.Context) ([][2]string, error) {
	discover, err := needService("DiscoverMemoryProviders", func(s *Services) func(context.Context) [][2]string { return s.DiscoverMemoryProviders })
	if err != nil {
		return nil, err
	}
	var out [][2]string
	if err := callGuarded(func() error { out = discover(ctx); return nil }); err != nil {
		return nil, nil
	}
	return out, nil
}

// Repo and user context engines plus the plugin registered one, a repo description winning a name collision
func discoverContextEngineChoices(ctx context.Context) ([][2]string, error) {
	var names []string
	engines := map[string]string{}
	add := func(name, desc string) {
		if _, ok := engines[name]; !ok {
			engines[name] = desc
			names = append(names, name)
		}
	}
	if found, err := toggleContextEngines(ctx); err == nil {
		for _, e := range found {
			add(e.Name, e.Description)
		}
	}
	_ = callGuarded(func() error {
		DiscoverPlugins(ctx, false)
		if name := contextEngineName(ctx, GetPluginContextEngine(ctx)); name != "" {
			add(name, "installed plugin")
		}
		return nil
	})
	out := make([][2]string, 0, len(names))
	for _, n := range names {
		out = append(out, [2]string{n, engines[n]})
	}
	return out, nil
}

// Truthy name attribute of a registered context engine, empty when it has none
func contextEngineName(ctx context.Context, engine any) string {
	switch e := engine.(type) {
	case nil:
		return ""
	case *HostedObject:
		if e == nil {
			return ""
		}
		v, err := e.Get(ctx, "name")
		if err != nil {
			return ""
		}
		s, _ := v.(string)
		return s
	case interface{ Name() string }:
		return e.Name()
	}
	return ""
}

// Radio picker for one provider category, saving and answering true when the choice changed
func configureCategorySpec(ctx context.Context, ui *cliui.Console, spec providerCategorySpec) (bool, error) {
	h := home.Dir(ctx)
	current := spec.Current(h)
	choices, err := spec.Discover(ctx)
	if err != nil {
		return false, err
	}
	names := []string{spec.DefaultName}
	items := []cliui.RadioItem{cliui.PlainItem(spec.DefaultLabel + " (default)")}
	for _, c := range choices {
		names = append(names, c[0])
		label := c[0]
		if c[1] != "" {
			label = c[0] + " — " + c[1]
		}
		items = append(items, cliui.PlainItem(label))
	}
	found := false
	for _, n := range names {
		found = found || n == current
	}
	if !found {
		names = append(names, current)
		items = append(items, cliui.PlainItem(current+" (not found)"))
	}
	selected := 0
	for i, n := range names {
		if n == current {
			selected = i
		}
	}
	idx, err := ui.CursesRadiolist(ctx, spec.Title+" (select one)", items, selected, cliui.RadiolistOptions{})
	if err != nil {
		return false, err
	}
	newValue := names[idx]
	if newValue == current {
		return false, nil
	}
	return true, spec.Save(ctx, h, newValue)
}

// Composite screen rows of every provider category with its current label
func providerCategories(ctx context.Context, ui *cliui.Console) []providerCategory {
	h := home.Dir(ctx)
	out := make([]providerCategory, 0, len(providerCategorySpecs))
	for _, spec := range providerCategorySpecs {
		current := spec.Current(h)
		if current == "" {
			current = spec.DefaultLabel
		}
		out = append(out, providerCategory{Title: spec.Title, Current: current,
			Configure: func() (bool, error) { return configureCategorySpec(ctx, ui, spec) }})
	}
	return out
}

// Interactive composite screen of the general plugins and the provider plugin categories
func CmdToggle(ctx context.Context, out io.Writer) error {
	console := NewConsole(out)
	h := home.Dir(ctx)
	entries, err := discoverAllPlugins(ctx)
	if err != nil {
		return err
	}
	expected, err := pluginSelectionVersion(h)
	if err != nil {
		return err
	}
	enabled, disabled := getEnabledSet(h), getDisabledSet(h)
	keys := make([]string, len(entries))
	labels := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
		labels[i] = e.Name
		if e.Description != "" {
			labels[i] = e.Name + " — " + e.Description
		}
		if e.Source == "bundled" {
			labels[i] += " [bundled]"
		}
	}
	selected, err := effectivePluginSelection(h, entries, enabled, disabled)
	if err != nil {
		return err
	}
	ui := toggleUI()
	categories := toggleProviderCategory(ctx, ui)
	if !toggleStdinTTY() {
		console.Print("[dim]Interactive mode requires a terminal.[/dim]")
		return nil
	}
	err = runCompositeUI(ctx, ui, keys, labels, selected, disabled, categories, console, &expected)
	if errors.Is(err, cliui.ErrCursesInit) {
		return runCompositeFallback(ctx, ui, out, keys, labels, selected, disabled, categories, &expected)
	}
	return err
}

// Row indices ticked on open by the load time rule rather than list membership
func effectivePluginSelection(h string, entries []PluginEntry, enabled, disabled map[string]bool) (map[int]bool, error) {
	active := categoryActiveNames(h)
	out := map[int]bool{}
	for i, e := range entries {
		status, err := entryStatus(h, e, enabled, disabled, active)
		if err != nil {
			return nil, err
		}
		if status == "enabled" {
			out[i] = true
		}
	}
	return out, nil
}

// Sorted indices of a minus b
func sortedDiff(a, b map[int]bool) []int {
	var out []int
	for i := range a {
		if !b[i] {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// Writes only the rows flipped this session through admission, answering the turned on and turned off keys
func persistPluginSelection(ctx context.Context, keys []string, chosen, initial map[int]bool, disabledSet map[string]bool, expected *string) ([]string, []string, error) {
	h := home.Dir(ctx)
	var on, off []string
	for _, i := range sortedDiff(chosen, initial) {
		on = append(on, keys[i])
	}
	for _, i := range sortedDiff(initial, chosen) {
		off = append(off, keys[i])
	}
	if len(on) == 0 && len(off) == 0 {
		return nil, nil, nil
	}
	if expected == nil {
		v, err := pluginSelectionVersion(h)
		if err != nil {
			return nil, nil, err
		}
		expected = &v
	}
	newEnabled := getEnabledSet(h)
	newDisabled := map[string]bool{}
	for k := range disabledSet {
		newDisabled[k] = true
	}
	entries, err := discoverAllPlugins(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, group := range []struct {
		keys   []string
		enable bool
	}{{on, true}, {off, false}} {
		for _, key := range group.keys {
			aliases := []string{}
			for a := range aliasesFrom(key, entries) {
				aliases = append(aliases, a)
			}
			sort.Strings(aliases)
			applyActivation(newEnabled, newDisabled, key, aliases, group.enable)
		}
	}
	if err := admitAndSavePluginSets(ctx, h, newEnabled, newDisabled, admitSetOptions{Action: "Save plugin selection", ExpectedConfig: *expected}); err != nil {
		return nil, nil, err
	}
	shownOn, shownOff := "none", "none"
	if len(on) > 0 {
		shownOn = pyListRepr(on)
	}
	if len(off) > 0 {
		shownOff = pyListRepr(off)
	}
	logInfo(fmt.Sprintf("plugins picker: enabled %s; disabled %s", shownOn, shownOff))
	return on, off, nil
}

// Curses screen of checkboxes and category action rows, the selection saved on exit
func runCompositeUI(ctx context.Context, ui *cliui.Console, keys, labels []string, selected map[int]bool, disabled map[string]bool, categories []providerCategory, console *Console, expected *string) error {
	chosen := map[int]bool{}
	for i := range selected {
		chosen[i] = true
	}
	nPlugins, nCategories := len(keys), len(categories)
	total := nPlugins + nCategories
	providersChanged := false
	mod := func(a, n int) int { return ((a % n) + n) % n }
	nav := map[int]func(c, p int) int{}
	for _, k := range []int{cliui.KeyUp, 'k'} {
		nav[k] = func(c, p int) int { return mod(c-1, total) }
	}
	for _, k := range []int{cliui.KeyDown, 'j'} {
		nav[k] = func(c, p int) int { return mod(c+1, total) }
	}
	for _, k := range []int{cliui.KeyNPage, 'f'} {
		nav[k] = func(c, p int) int { return min(total-1, c+p) }
	}
	for _, k := range []int{cliui.KeyPPage, 'b'} {
		nav[k] = func(c, p int) int { return max(0, c-p) }
	}
	nav[cliui.KeyHome] = func(c, p int) int { return 0 }
	nav[cliui.KeyEnd] = func(c, p int) int { return total - 1 }

	scr, closeScreen, err := ui.OpenFullScreen()
	if err != nil {
		return err
	}
	defer func() { closeScreen() }()
	initColors := func() error {
		gray := cliui.ColorWhite
		if scr.Colors() > 8 {
			gray = 8
		}
		return scr.InitPairs(map[int]int{1: cliui.ColorGreen, 2: cliui.ColorYellow, 3: cliui.ColorCyan, 4: gray})
	}
	attr := func(bold bool, pair int) cliui.Attr {
		if scr.HasColors() {
			return cliui.Attr{Bold: bold, Pair: pair}
		}
		return cliui.Attr{Bold: bold}
	}
	type line struct {
		text string
		attr cliui.Attr
	}
	row := func(text string, idx, cursor, pair int) line {
		if idx == cursor {
			return line{" → " + text, attr(true, pair)}
		}
		return line{"   " + text, cliui.Attr{}}
	}
	configureCategory := func(ci int) error {
		closeScreen()
		changed, err := categories[ci].Configure()
		if err != nil {
			return err
		}
		if changed {
			providersChanged = true
			categories[ci].Current = toggleProviderCategory(ctx, ui)[ci].Current
		}
		if scr, closeScreen, err = ui.OpenFullScreen(); err != nil {
			closeScreen = func() {}
			return err
		}
		return initColors()
	}
	bodyLines := func(cursor, offset, visible int) []line {
		var lines []line
		if nPlugins > 0 {
			lines = append(lines, line{"  General Plugins", attr(true, 2)})
			for i := offset; i < min(nPlugins, offset+max(visible, 0)); i++ {
				check := " "
				if chosen[i] {
					check = "✓"
				}
				lines = append(lines, row("["+check+"] "+labels[i], i, cursor, 1))
			}
		}
		lines = append(lines, line{"", cliui.Attr{}})
		if nCategories > 0 {
			lines = append(lines, line{"  Provider Plugins", attr(true, 2)})
			for ci, c := range categories {
				lines = append(lines, row(fmt.Sprintf("  %-24s ▸ %s", c.Title, c.Current), nPlugins+ci, cursor, 3))
			}
		}
		return lines
	}
	if err := initColors(); err != nil {
		return err
	}
	cursor, offset := 0, 0
	for done := false; !done; {
		scr.Clear()
		maxY, maxX := scr.GetMaxYX()
		_ = scr.AddNStr(0, 0, "Plugins", maxX-1, attr(true, 2))
		_ = scr.AddNStr(1, 0, "  ↑↓/j/k navigate  PgUp/PgDn page  SPACE toggle  ENTER configure/confirm  ESC done", maxX-1, cliui.Attr{Dim: true})
		visible := maxY - 4
		if cursor < offset {
			offset = cursor
		} else if cursor >= offset+visible {
			offset = cursor - visible + 1
		}
		lines := bodyLines(cursor, offset, visible)
		lines = lines[:min(len(lines), max(0, maxY-4))]
		for i, l := range lines {
			if l.text != "" {
				_ = scr.AddNStr(i+3, 0, l.text, maxX-1, l.attr)
			}
		}
		if err := scr.Refresh(); err != nil {
			return err
		}
		key, err := scr.Getch()
		if err != nil {
			return err
		}
		switch move, ok := nav[key]; {
		case ok:
			if total > 0 {
				cursor = move(cursor, max(1, maxY-5))
			}
		case key == ' ' || key == cliui.KeyEnter || key == 10 || key == 13:
			switch {
			case cursor >= nPlugins:
				if cursor-nPlugins < nCategories {
					if err := configureCategory(cursor - nPlugins); err != nil {
						return err
					}
				}
			case key == ' ':
				chosen[cursor] = !chosen[cursor]
				if !chosen[cursor] {
					delete(chosen, cursor)
				}
			default:
				done = true
			}
		case key == 27 || key == 'q':
			done = true
		}
	}
	closeScreen()
	closeScreen = func() {}
	ui.FlushStdin()
	on, off, err := persistPluginSelection(ctx, keys, chosen, selected, disabled, expected)
	var refused *AdmissionRefused
	if errors.As(err, &refused) {
		console.Print("[red]✗[/red] Plugin selection refused, not saved: " + refused.Error())
		console.Print("[dim]config.yaml and the active environment are unchanged. Run `icebath pm install` to resolve, then retry.[/dim]")
		return nil
	}
	if err != nil {
		return err
	}
	if len(on) > 0 || len(off) > 0 {
		console.Print(fmt.Sprintf("\n[green]✓[/green] General plugins: %d turned on, %d turned off.", len(on), len(off)))
	} else if nPlugins > 0 {
		console.Print("\n[dim]General plugins unchanged.[/dim]")
	}
	if providersChanged {
		h := home.Dir(ctx)
		memory := getCurrentMemoryProvider(h)
		if memory == "" {
			memory = "built-in"
		}
		console.Print(fmt.Sprintf("[green]✓[/green] Memory provider: [bold]%s[/bold]  Context engine: [bold]%s[/bold]", memory, getCurrentContextEngine(h)))
	}
	if nPlugins > 0 || providersChanged {
		console.Print("[dim]Changes take effect on next session.[/dim]")
	}
	console.Print("")
	return nil
}

// Python int of a stripped answer, false where int() raises ValueError
func pyIntAnswer(val string) (int, bool) {
	text := strings.ReplaceAll(val, "_", "")
	if text == "" || strings.HasPrefix(val, "_") || strings.HasSuffix(val, "_") || strings.Contains(val, "__") {
		return 0, false
	}
	n, err := strconv.Atoi(text)
	return n, err == nil
}

// Text menu standing in for the composite screen when curses is unavailable
func runCompositeFallback(ctx context.Context, ui *cliui.Console, out io.Writer, keys, labels []string, selected map[int]bool, disabled map[string]bool, categories []providerCategory, expected *string) error {
	fmt.Fprintln(out, ui.Color("\n  Plugins", cliui.Yellow))
	if len(keys) > 0 {
		chosen := map[int]bool{}
		for i := range selected {
			chosen[i] = true
		}
		fmt.Fprintln(out, ui.Color("\n  General Plugins", cliui.Yellow))
		fmt.Fprintln(out, ui.Color("  Toggle by number, Enter to confirm.\n", cliui.Dim))
		for {
			for i, label := range labels {
				marker := "[ ]"
				if chosen[i] {
					marker = ui.Color("[✓]", cliui.Green)
				}
				fmt.Fprintf(out, "  %s %2d. %s\n", marker, i+1, label)
			}
			fmt.Fprintln(out)
			answer, err := inputLine(ui.Color("  Toggle # (or Enter to confirm): ", cliui.Dim))
			if err != nil {
				return nil
			}
			val := pystr.Strip(answer)
			if val == "" {
				break
			}
			n, ok := pyIntAnswer(val)
			if !ok {
				return nil
			}
			if idx := n - 1; idx >= 0 && idx < len(keys) {
				if chosen[idx] {
					delete(chosen, idx)
				} else {
					chosen[idx] = true
				}
			}
			fmt.Fprintln(out)
		}
		if err := savePluginSelectionFallback(ctx, out, keys, chosen, selected, disabled, expected); err != nil {
			return err
		}
	}
	if len(categories) > 0 {
		fmt.Fprintln(out, ui.Color("\n  Provider Plugins", cliui.Yellow))
		for ci, c := range categories {
			fmt.Fprintf(out, "  %d. %s [%s]\n", ci+1, c.Title, c.Current)
		}
		fmt.Fprintln(out)
		if answer, err := inputLine(ui.Color("  Configure # (or Enter to skip): ", cliui.Dim)); err == nil {
			if val := pystr.Strip(answer); val != "" {
				if n, ok := pyIntAnswer(val); ok {
					if ci := n - 1; ci >= 0 && ci < len(categories) {
						if _, err := categories[ci].Configure(); err != nil && !errors.Is(err, cliui.ErrInterrupt) && !errors.Is(err, cliui.ErrEOF) {
							return err
						}
					}
				}
			}
		}
	}
	fmt.Fprintln(out)
	return nil
}

// Saves the text menu selection through admission, printing a refusal
func savePluginSelectionFallback(ctx context.Context, out io.Writer, keys []string, chosen, initial map[int]bool, disabled map[string]bool, expected *string) error {
	_, _, err := persistPluginSelection(ctx, keys, chosen, initial, disabled, expected)
	var refused *AdmissionRefused
	if errors.As(err, &refused) {
		fmt.Fprintln(out, "  Plugin selection refused, not saved: "+refused.Error())
		fmt.Fprintln(out, "  config.yaml and the active environment are unchanged.")
		return nil
	}
	return err
}
