package cards

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/rules"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Card the messages ask the manager to show over the desk, nil when none does
func overlaid(msgs []tea.Msg) wm.Card {
	m := wm.New(wm.WithMotion(0))
	m.SetSize(140, 50)
	for _, msg := range msgs {
		if in, ok := msg.(wm.Intent); ok {
			m.Update(in)
		}
	}
	return m.Overlay()
}

// Reports whether the messages hold a toast, the dialog closing or any other request of the manager
func intent(msgs []tea.Msg) bool {
	_, ok := find[wm.Intent](msgs)
	return ok
}

// Config card with every tab started, on the tab at i with its focus delivered
func (te *testEnv) config(t *testing.T, i int) (*Config, []tea.Msg) {
	t.Helper()
	c := NewConfig(te.env)
	for _, card := range c.Cards() {
		card.Init()
	}
	if i == 0 {
		_, cmd := c.Update(wm.FocusMsg{Focused: true})
		return c, run(cmd)
	}
	return c, run(c.Select(i))
}

// Tab under the strip as a table
func active(c *Config) *lister {
	card, _ := c.Active()
	return card.(*lister)
}

func TestConfigTabsCarryDistinctGlyphsUnderAPaddedStrip(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 0)
	var titles, glyphs []string
	for _, card := range c.Cards() {
		titles = append(titles, card.Title())
		glyphs = append(glyphs, card.(wm.Glypher).Glyph())
	}
	if strings.Join(titles, " ") != "profiles providers models app rules hooks mcp tools log keys" {
		t.Fatalf("tabs %v", titles)
	}
	for i, g := range glyphs {
		if g == c.Glyph() || slices.Index(glyphs, g) != i {
			t.Fatalf("glyph %s of %s is shared %v", g, titles[i], glyphs)
		}
	}
	lines := strings.Split(plain(c.View(120, 12)), "\n")
	if !strings.Contains(lines[0], "profiles") || strings.TrimSpace(lines[1]) != "" || !strings.Contains(lines[2], "no profiles yet") {
		t.Fatalf("a blank row pads the content from the strip\n%s", strings.Join(lines, "\n"))
	}
}

func TestProfilesStartFromProvidersAndAreEditedSwitchedMovedAndDeleted(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 0)
	if h := c.Hints(); len(h) != 2 || h[1].Action != "config.profile.add" {
		t.Fatalf("an empty table offers to add and nothing that needs a row %+v", h)
	}
	form, ok := overlaid(te.pressed(c, "a")).(*Form)
	if !ok || form.Title() != "NEW PROFILE" {
		t.Fatalf("a opens the new profile dialog %T", form)
	}
	if fd := form.focused(); fd.key != "provider" || fd.value != "anthropic" {
		t.Fatalf("the dialog starts on the provider %+v", fd)
	}
	view := plain(form.View(formWidth, 30))
	for _, want := range []string{"provider", "anthropic", "openai", "custom", "https://api.anthropic.com", "x-api-key", "ANTHROPIC_API_KEY", "save", "cancel", "Built in provider the profile starts from"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the dialog shows %q with every choice and the help of the focus\n%s", want, view)
		}
	}
	te.pressed(form, "tab")
	if msgs := te.pressed(form, "enter"); intent(msgs) || form.bad[form.field("name")] == "" {
		t.Fatal("a missing name keeps the dialog open and marks the field")
	}
	typeText(form, "work")
	for range 8 {
		te.press(form, "tab")
	}
	if form.focused().key != "key" || form.focused().label != "variable" {
		t.Fatalf("tab walks to the key %+v", form.focused())
	}
	te.press(form, "ctrl+u")
	typeText(form, "WORK_KEY")
	msgs := te.pressed(form, "enter")
	if !intent(msgs) {
		t.Fatal("a good dialog saves and closes")
	}
	list := te.app.Instances()
	if len(list) != 1 || list[0].Written.Template != "anthropic" || list[0].Written.BaseURL != "" || list[0].Written.Key.Value != "WORK_KEY" || list[0].Profile.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("the profile keeps only what differs from its provider %+v", list)
	}
	c.Update(credentialsMsg{texts: map[string]string{"profile:work": "set, env WORK_KEY"}})
	view = plain(c.View(140, 12))
	if !strings.Contains(view, "work") || !strings.Contains(view, "anthropic") || !strings.Contains(view, "set, env WORK_KEY") || !strings.Contains(view, "on") {
		t.Fatalf("the row shows the provider, the credential and the state\n%s", view)
	}
	if p, ok := find[SetProfileMsg](te.pressed(c, "enter")); !ok || p.Profile != "work" {
		t.Fatalf("enter switches to the profile %+v", p)
	}
	edit, _ := overlaid(te.pressed(c, "e")).(*Form)
	if edit == nil || edit.Title() != "EDIT work" || edit.field("key").value != "WORK_KEY" || edit.field("name").value != "work" {
		t.Fatalf("e opens the written values %+v", edit)
	}
	edit.field("model").set("claude-x")
	te.app.Primary.Profile.Name = "work"
	msgs = run(edit.save())
	if p, ok := find[SetProfileMsg](msgs); !ok || p.Profile != "work" || te.app.Instances()[0].Profile.Model() != "claude-x" {
		t.Fatalf("saving the profile in use reopens it %+v", te.app.Instances())
	}
	te.app.Primary.Profile.Name = "anthropic"
	home := c.profileForm(nil, "")
	home.field("provider").typeText("c")
	if home.field("provider").value != customProvider || home.field("endpoint").optional || home.field("model").optional {
		t.Fatal("typing jumps to the custom choice, which needs an endpoint and a model")
	}
	home.field("name").set("home")
	home.field("endpoint").set("")
	if run(home.save()) != nil || home.bad[home.field("endpoint")] == "" || home.bad[home.field("model")] == "" {
		t.Fatal("a custom profile asks for its endpoint and model")
	}
	home.field("endpoint").set("http://home:11434")
	home.field("model").set("llama")
	home.field("protocol").set("ollama-chat")
	home.field("vendor").set("ollama")
	if !intent(run(home.save())) || len(te.app.Instances()) != 2 {
		t.Fatalf("a custom profile saves %s", home.err)
	}
	c.profiles.refresh()
	c.profiles.sel = 1
	te.press(c, "K")
	if te.app.Instances()[0].Profile.Name != "home" || c.profiles.sel != 0 {
		t.Fatal("K moves the profile up and the cursor with it")
	}
	te.press(c, "J")
	if te.app.Instances()[1].Profile.Name != "home" || c.profiles.sel != 1 {
		t.Fatal("J moves it back down")
	}
	te.press(c, "space")
	if !te.app.Instances()[1].Profile.Disabled || !strings.Contains(plain(c.View(140, 12)), "off") {
		t.Fatal("space turns the profile off")
	}
	if !intent(te.pressed(c, "enter")) {
		t.Fatal("a profile turned off refuses to be used")
	}
	te.press(c, "o")
	if !strings.Contains(plain(c.View(140, 30)), "profiles.json") {
		t.Fatal("the detail names the file holding the profile")
	}
	te.press(c, "o")
	te.press(c, "d")
	if !slices.Equal(c.Contexts()[:1], []string{keys.Confirm}) || !strings.Contains(plain(c.View(140, 12)), "delete profile home?") {
		t.Fatal("d asks before deleting")
	}
	te.press(c, "n")
	if len(te.app.Instances()) != 2 {
		t.Fatal("n keeps the profile")
	}
	te.press(c, "d")
	te.press(c, "y")
	if len(te.app.Instances()) != 1 {
		t.Fatal("y deletes the profile")
	}
	if _, ok := find[credentialsMsg](te.pressed(c, "r")); !ok {
		t.Fatal("r checks the credentials again")
	}
	keyless := c.profileForm(nil, "anthropic")
	keyless.field("name").set("keyless")
	keyless.field("key_from").set("none")
	if keyless.field("key").visible() || slices.Contains(keyless.field("vendor").choices, noneChoice) || !intent(run(keyless.save())) {
		t.Fatal("a provider profile may drop the key and offers no vendor it cannot keep")
	}
	if in := te.app.Instances(); in[len(in)-1].Written.Key.Kind != profiles.SourceNone {
		t.Fatalf("an explicit none is written over the provider key %+v", in[len(in)-1].Written.Key)
	}
	if !slices.Contains(c.profileForm(nil, customProvider).field("vendor").choices, noneChoice) {
		t.Fatal("a custom endpoint may go without a vendor")
	}
	failing := c.profileForm(nil, "openai")
	failing.field("name").set("work")
	if run(failing.save()) != nil || !strings.Contains(failing.err, "already exists") || !strings.Contains(plain(failing.View(formWidth, 40)), "already exists") {
		t.Fatalf("a refused save keeps the dialog open with the reason %q", failing.err)
	}
	if !intent(te.pressed(failing, "esc")) {
		t.Fatal("esc closes the dialog")
	}
}

func TestProvidersAreTemplatesUsableAsTheyAre(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 1)
	view := plain(c.View(140, 14))
	if !strings.Contains(view, "openai-chat") || !strings.Contains(view, "anthropic-messages") {
		t.Fatalf("every built in provider is listed\n%s", view)
	}
	form, ok := overlaid(te.pressed(c, "enter")).(*Form)
	if !ok || form.field("provider").value != "anthropic" {
		t.Fatal("enter starts a profile from the provider")
	}
	openai := c.profileForm(nil, "openai")
	if openai.field("endpoint").value != "https://api.openai.com/v1" || openai.field("key").value != "OPENAI_API_KEY" {
		t.Fatal("a provider fills the dialog")
	}
	openai.field("provider").set("mistral")
	if openai.field("endpoint").value != "https://api.mistral.ai" {
		t.Fatal("picking another provider fills its values")
	}
	if p, ok := find[SetProfileMsg](te.pressed(c, "u")); !ok || p.Profile != "anthropic" {
		t.Fatal("u uses the provider as it is")
	}
	te.app.SaveProfile("", profiles.Profile{Name: "own", Template: "anthropic"})
	te.app.Profiles[0].Profile.Name, te.app.Profiles[0].Written.Name = "anthropic", "anthropic"
	if !intent(te.pressed(c, "u")) {
		t.Fatal("a provider shadowed by a profile of its name points at that profile")
	}
	te.press(c, "o")
	if !strings.Contains(plain(c.View(140, 40)), "takes its place") {
		t.Fatal("the detail names the shadow")
	}
}

func TestModelsLoadWhenTheTabShowsAndDetailEachRow(t *testing.T) {
	te := newEnv(t)
	c, msgs := te.config(t, 2)
	loaded, ok := find[modelsLoadedMsg](msgs)
	if !ok || !strings.Contains(plain(c.View(120, 10)), "loading the models anthropic serves") {
		t.Fatal("showing the tab fetches the models at once")
	}
	c.Update(loaded)
	view := plain(c.View(120, 10))
	if !strings.Contains(view, "other") || strings.Contains(view, "models from") || strings.Contains(view, "catalog") {
		t.Fatalf("models list without a note\n%s", view)
	}
	if cmd := c.models.onFocus(c.models); cmd != nil {
		t.Fatal("a list of the profile in use is not fetched again")
	}
	te.press(c, "o")
	detail := plain(c.View(120, 30))
	for _, want := range []string{"context", "max output", "dollars per million", "takes"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the detail shows %q\n%s", want, detail)
		}
	}
	te.press(c, "o")
	te.press(c, "j")
	if pick, ok := find[SetModelMsg](te.pressed(c, "enter")); !ok || pick.Model != "other" {
		t.Fatalf("enter uses the model %+v", pick)
	}
	refetch, ok := find[modelsLoadedMsg](te.pressed(c, "r"))
	if !ok || len(c.ModelList()) != 2 {
		t.Fatal("r fetches again, keeping the list of the same profile meanwhile")
	}
	c.Update(modelsLoadedMsg{profile: "elsewhere", err: errors.New("late")})
	if c.modelErr != nil || c.loading == "" {
		t.Fatal("a list another profile served is dropped")
	}
	refetch.err, refetch.models = errors.New("offline"), nil
	c.Update(refetch)
	if len(c.ModelList()) != 2 || c.modelErr == nil {
		t.Fatal("a failed fetch keeps the list")
	}
	c.list, c.loaded = nil, ""
	if !strings.Contains(plain(c.View(120, 10)), "offline") {
		t.Fatal("the failure shows while nothing is listed")
	}
	c.list, c.loaded = []profiles.Model{{ID: "gpt-5"}}, "openai"
	c.FetchModels()
	if c.ModelList() != nil {
		t.Fatal("fetching for another profile drops the list the old one served")
	}
}

func TestSettingsShowEveryChoiceAndSlideNumbers(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 3)
	s := c.app
	view := plain(c.View(120, 16))
	for _, want := range []string{"default", "acceptEdits", "plan", "bypassPermissions", "blackwall", "contrast", "light", "plain", "true", "false", "━", "128k"} {
		if !strings.Contains(view, want) {
			t.Fatalf("settings show %q\n%s", want, view)
		}
	}
	for _, row := range strings.Split(view, "\n") {
		if strings.Contains(row, "max_rounds") || strings.Contains(row, "temperature") || strings.Contains(row, "compact_threshold") {
			if !strings.Contains(row, " - ") || strings.Contains(row, "default") {
				t.Fatalf("an unset number shows a dash %q", row)
			}
		}
	}
	if !strings.Contains(view, "How tool calls are approved") {
		t.Fatal("the help of the selected setting shows")
	}
	knob, ok := find[KnobMsg](te.pressed(c, "l"))
	if !ok || knob.Key != "mode" || knob.Value != "acceptEdits" || !knob.Quiet {
		t.Fatalf("right picks the next choice and saves it quietly %+v", knob)
	}
	te.press(c, "j")
	if h := s.Hints(); !slices.ContainsFunc(h, func(x wm.Hint) bool { return x.Action == "config.app.edit" }) {
		t.Fatal("a number offers typing")
	}
	typeText(c, "2048")
	c.View(120, 16)
	if !s.editing || !slices.Equal(s.Contexts(), []string{keys.Edit}) || s.Cursor() == nil {
		t.Fatal("a digit starts typing the number")
	}
	knob, ok = find[KnobMsg](te.pressed(c, "enter"))
	if !ok || knob.Key != "max_tokens" || knob.Value != "2048" || s.editing {
		t.Fatalf("enter saves the typed number %+v", knob)
	}
	te.knobs["max_tokens"] = "2048"
	te.press(c, "enter")
	te.press(c, "ctrl+u")
	typeText(c, "5")
	if !intent(te.pressed(c, "enter")) || !s.editing {
		t.Fatal("a number off the range is refused and typing goes on")
	}
	te.press(c, "esc")
	c.View(120, 16)
	if s.editing || s.current().value != "2048" {
		t.Fatal("esc restores the number")
	}
	te.press(c, "j")
	knob, _ = find[KnobMsg](te.pressed(c, "right"))
	if knob.Key != "max_rounds" || knob.Value != "50" {
		t.Fatalf("stepping an unset number starts from its start %+v", knob)
	}
	te.knobs["max_rounds"] = "50"
	c.View(120, 16)
	if h := s.Hints(); !slices.ContainsFunc(h, func(x wm.Hint) bool { return x.Action == "config.app.unset" }) {
		t.Fatal("a set optional number offers unset")
	}
	knob, _ = find[KnobMsg](te.pressed(c, "backspace"))
	if knob.Key != "max_rounds" || knob.Value != "" {
		t.Fatalf("backspace unsets %+v", knob)
	}
	te.press(c, "j")
	knob, _ = find[KnobMsg](te.pressed(c, "l"))
	if knob.Key != "temperature" || knob.Value != "1.00" {
		t.Fatalf("temperature steps in hundredths %+v", knob)
	}
	var track, light formHit
	for _, h := range s.hits {
		if h.field.key == "motion" && h.hit.choice == hitTrack {
			track = h
		}
		if h.field.key == "theme" && h.hit.choice >= 0 && h.field.choices[h.hit.choice] == "light" {
			light = h
		}
	}
	knob, _ = find[KnobMsg](run(second(s.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: track.x0, Y: track.y}))))
	if knob.Key != "motion" || knob.Value != "0.00" || s.current().key != "motion" {
		t.Fatalf("a click on the left end of the track sets the low end %+v", knob)
	}
	knob, _ = find[KnobMsg](run(second(s.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: light.x0 + 1, Y: light.y}))))
	if knob.Key != "theme" || knob.Value != "light" {
		t.Fatalf("a click on a choice picks it %+v", knob)
	}
	s.sel = 7
	knob, _ = find[KnobMsg](te.pressed(c, "h"))
	if knob.Key != "cascade" || knob.Value != "true" {
		t.Fatalf("booleans are a choice of true and false %+v", knob)
	}
	s.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if s.sel != 6 {
		t.Fatal("the wheel moves the cursor")
	}
	te.env.Theme = theme.Plain()
	if !strings.Contains(plain(s.View(120, 16)), " > ") {
		t.Fatal("the plain theme marks the cursor in ascii")
	}
}

func TestRulesAreWrittenThroughADialog(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 4)
	if !strings.Contains(plain(c.View(120, 10)), "no rules, a adds one") {
		t.Fatal("the empty tab says how to add")
	}
	form, ok := overlaid(te.pressed(c, "a")).(*Form)
	if !ok || form.Title() != "NEW RULE" {
		t.Fatal("a opens the rule dialog")
	}
	view := plain(form.View(formWidth, 20))
	for _, want := range []string{"decision", "allow", "ask", "deny", "tool", "Read", "other", "pattern", "every call", "scope", "project", "user"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the rule dialog shows %q\n%s", want, view)
		}
	}
	te.press(form, "tab")
	typeText(form, "R")
	if form.field("tool").value != "Read" || !strings.Contains(form.field("pattern").help, "Path glob") {
		t.Fatal("typing picks a tool and the pattern help follows it")
	}
	te.press(form, "tab")
	typeText(form, "src/**")
	if !intent(te.pressed(form, "enter")) {
		t.Fatal(form.err)
	}
	set, err := rules.LoadFile(filepath.Join(te.dir, harness.ProjectDir, harness.RulesFile))
	if err != nil || len(set.Allow) != 1 || set.Allow[0] != "Read(src/**)" || len(te.app.Rules.Rules(policy.Allow)) != 1 {
		t.Fatalf("the rule lands in the project file and in force %+v %v", set, err)
	}
	if view := plain(c.View(120, 10)); !strings.Contains(view, "src/**") || !strings.Contains(view, "project") {
		t.Fatalf("the row shows the pattern and the scope\n%s", view)
	}
	edit, _ := overlaid(te.pressed(c, "enter")).(*Form)
	if edit == nil || edit.field("pattern").value != "src/**" || edit.field("scope") != nil {
		t.Fatal("enter edits the rule in its own file")
	}
	edit.field("decision").set("deny")
	edit.field("tool").set("other")
	if run(edit.save()) != nil || edit.bad[edit.field("glob")] == "" {
		t.Fatal("other asks for a tool glob")
	}
	edit.field("glob").set("mcp__*")
	edit.field("pattern").set("")
	if !intent(run(edit.save())) {
		t.Fatal(edit.err)
	}
	if got := te.app.Rules.Rules(policy.Deny); len(got) != 1 || got[0] != "mcp__*" || len(te.app.Rules.Rules(policy.Allow)) != 0 {
		t.Fatalf("the edit replaces the rule %v", got)
	}
	if !strings.Contains(plain(c.View(120, 10)), "every call") {
		t.Fatal("a rule without a pattern reads as every call")
	}
	again := c.ruleForm(&c.ruleList[0])
	if again.field("tool").value != "other" || again.field("glob").value != "mcp__*" {
		t.Fatal("a glob reopens under other")
	}
	te.press(c, "d")
	te.press(c, "y")
	if len(c.ruleList) != 0 || len(te.app.Rules.Rules(policy.Deny)) != 0 {
		t.Fatal("the rule deletes after asking")
	}
	os.WriteFile(filepath.Join(te.dir, harness.ProjectDir, harness.RulesFile), []byte("{"), 0o644)
	if !intent(run(c.loadRules())) {
		t.Fatal("a broken rules file is reported")
	}
}

func TestHooksAreWrittenThroughADialog(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 5)
	form, _ := overlaid(te.pressed(c, "a")).(*Form)
	if form == nil || form.field("event").value != string(hooks.PreToolUse) || !form.field("matcher").visible() {
		t.Fatal("a opens the hook dialog on PreToolUse with a matcher")
	}
	form.field("matcher").set("(")
	form.field("command").set("guard.sh")
	if run(form.save()) != nil || form.bad[form.field("matcher")] == "" {
		t.Fatal("a matcher that does not compile is refused in place")
	}
	form.field("matcher").set("Bash")
	form.at = 3
	te.press(form, "right")
	if form.field("timeout").value != "60" {
		t.Fatalf("the timeout slider steps from the default %q", form.field("timeout").value)
	}
	if !intent(run(form.save())) {
		t.Fatal(form.err)
	}
	cfg, err := hooks.LoadFile(filepath.Join(te.dir, harness.ProjectDir, harness.HooksFile))
	if err != nil || cfg[hooks.PreToolUse][0].Matcher != "Bash" || cfg[hooks.PreToolUse][0].Hooks[0].Timeout != 60 || len(te.app.Config.Hooks[hooks.PreToolUse]) != 1 {
		t.Fatalf("the hook lands in the project file and in force %+v %v", cfg, err)
	}
	if view := plain(c.View(120, 10)); !strings.Contains(view, "guard.sh") || !strings.Contains(view, "60s") {
		t.Fatalf("the row shows the hook\n%s", view)
	}
	edit, _ := overlaid(te.pressed(c, "e")).(*Form)
	edit.field("event").set(string(hooks.Stop))
	if edit.field("matcher").visible() {
		t.Fatal("events without a tool hide the matcher")
	}
	if !intent(run(edit.save())) {
		t.Fatal(edit.err)
	}
	if e := c.hookList[0]; e.Event != hooks.Stop || e.Matcher != "" {
		t.Fatalf("the edit moves the hook and drops the matcher %+v", e)
	}
	te.press(c, "d")
	te.press(c, "y")
	if len(c.hookList) != 0 || len(te.app.Config.Hooks) != 0 {
		t.Fatal("the hook deletes after asking")
	}
}

func TestServersAreWrittenSwitchedAndReconnectedFromTheTab(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 6)
	if h := c.Hints(); len(h) != 2 || h[1].Action != "config.mcp.add" {
		t.Fatalf("no server, nothing to reconnect %+v", h)
	}
	form, _ := overlaid(te.pressed(c, "a")).(*Form)
	if form == nil || form.field("command").visible() == false || form.field("url").visible() {
		t.Fatal("a opens the server dialog on stdio")
	}
	form.field("name").set("web")
	form.field("transport").typeText("h")
	if !form.field("url").visible() || form.field("command").visible() {
		t.Fatal("http shows the url in place of the command")
	}
	form.field("url").set("http://127.0.0.1:1/mcp")
	form.field("headers").set("Authorization: Bearer x; X-Team: blue")
	form.field("enabled").set("false")
	if !intent(run(form.save())) {
		t.Fatal(form.err)
	}
	list, _ := te.app.ServerEntries()
	if len(list) != 1 || list[0].Config.Headers["X-Team"] != "blue" || !list[0].Config.Disabled {
		t.Fatalf("the server lands in the project config %+v", list)
	}
	view := plain(c.View(140, 10))
	if !strings.Contains(view, "web") || !strings.Contains(view, "http") || !strings.Contains(view, "off") || !strings.Contains(view, "project") {
		t.Fatalf("the row shows the server\n%s", view)
	}
	if !intent(te.pressed(c, "R")) {
		t.Fatal("a server turned off asks to be turned on before a reconnect")
	}
	te.pressed(c, "space")
	if list, _ := te.app.ServerEntries(); list[0].Config.Disabled {
		t.Fatal("space turns the server on")
	}
	c.View(140, 10)
	if st := te.app.MCP.Status(); len(st) != 1 || st[0].Config.Disabled {
		t.Fatalf("the server restarts on in the background %+v", st)
	}
	if r, ok := find[ReconnectMsg](te.pressed(c, "R")); !ok || r.Server != "web" {
		t.Fatal("R reconnects a server that is on")
	}
	te.press(c, "o")
	if !strings.Contains(plain(c.View(140, 30)), "Authorization, X-Team") {
		t.Fatal("the detail names the headers without their values")
	}
	te.press(c, "o")
	stdio := c.serverForm(nil)
	stdio.field("name").set("files")
	stdio.field("command").set("npx")
	stdio.field("args").set(`-y "server files" --root /tmp`)
	stdio.field("env").set("TOKEN=abc DEBUG=1")
	stdio.field("enabled").set("false")
	stdio.field("scope").set("user")
	if !intent(run(stdio.save())) {
		t.Fatal(stdio.err)
	}
	list, _ = te.app.ServerEntries()
	at := slices.IndexFunc(list, func(e harness.ServerEntry) bool { return e.Config.Name == "files" })
	if len(list) != 2 || at < 0 || len(list[at].Config.Args) != 4 || list[at].Config.Args[1] != "server files" || list[at].Config.Env["DEBUG"] != "1" || te.app.ScopeOf(list[at].Path) != "user" {
		t.Fatalf("a stdio server keeps quoted arguments and its environment %+v", list)
	}
	reopened := c.serverForm(&list[at].Config)
	if reopened.field("args").value != `-y "server files" --root /tmp` || reopened.field("env").value != "DEBUG=1 TOKEN=abc" {
		t.Fatalf("the dialog writes the arguments back as typed %q %q", reopened.field("args").value, reopened.field("env").value)
	}
	reopened.field("env").set("broken")
	if run(reopened.save()) != nil || reopened.bad[reopened.field("env")] == "" {
		t.Fatal("an environment word without = is refused in place")
	}
	te.press(c, "d")
	te.pressed(c, "y")
	if list, _ := te.app.ServerEntries(); len(list) != 1 || len(te.app.MCP.Status()) != 1 {
		t.Fatal("the server deletes after asking and closes in the background")
	}
}

func TestArgumentWordsSplitAndJoin(t *testing.T) {
	for _, line := range []string{`a b`, `"a b" c`, `'x y' "q\"t"`, `back\ slash`, ``} {
		words, err := splitArgs(line)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := splitArgs(joinArgs(words))
		if !slices.Equal(words, again) {
			t.Fatalf("%q splits to %q and back to %q", line, words, again)
		}
	}
	if _, err := splitArgs(`"open`); err == nil {
		t.Fatal("an unclosed quote is refused")
	}
	if h, err := splitHeaders("A: 1; ;B:2"); err != nil || h["A"] != "1" || h["B"] != "2" || joinHeaders(h) != "A: 1; B: 2" {
		t.Fatalf("headers %v %v", h, err)
	}
	if _, err := splitHeaders("nocolon"); err == nil {
		t.Fatal("a header without a colon is refused")
	}
}

func TestToolsTurnOnAndOffFromTheTab(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 7)
	before := len(te.env.Tools.Definitions())
	te.press(c, "space")
	if len(te.env.Tools.Definitions()) != before-1 || !strings.Contains(plain(c.View(140, 12)), "off") {
		t.Fatal("space turns the tool off for the model")
	}
	got, err := harness.ReadConfigFile(filepath.Join(te.dir, "user", harness.ConfigFile))
	if err != nil || len(got.Tools.Disabled) != 1 {
		t.Fatalf("the tool switch is saved %+v %v", got.Tools, err)
	}
	te.press(c, "space")
	if len(te.env.Tools.Definitions()) != before {
		t.Fatal("space turns it back on")
	}
	if !strings.Contains(plain(c.View(140, 12)), "built in") {
		t.Fatal("rows name where the tool comes from")
	}
}

func TestLogShowsTheJournalNewestLast(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 8)
	if !strings.Contains(plain(c.View(120, 10)), "nothing logged yet") {
		t.Fatal("an empty log says so")
	}
	te.env.Journal.Add(harness.LevelInfo, "run r1", "started")
	te.env.Journal.Add(harness.LevelError, "mcp s", "gone")
	view := plain(c.View(120, 10))
	if !strings.Contains(view, "started") || !strings.Contains(view, "gone") || c.log.sel != 1 {
		t.Fatalf("the log lists the journal and follows the newest line\n%s", view)
	}
	te.env.Journal.Add(harness.LevelWarn, "notice", strings.Repeat("long ", 40))
	c.View(120, 10)
	if c.log.sel != 2 {
		t.Fatal("the cursor follows while it sits on the newest line")
	}
	te.press(c, "o")
	if !strings.Contains(plain(c.View(120, 30)), "from notice") {
		t.Fatal("the detail shows the whole line")
	}
	te.press(c, "c")
	if len(te.env.Journal.Lines()) != 0 {
		t.Fatal("c clears the log")
	}
}

func TestKeysTabRebindsTypesAndSaves(t *testing.T) {
	te := newEnv(t)
	path := filepath.Join(te.dir, "keys", keys.FileName)
	km, err := keys.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	te.env.Keys = km
	c, _ := te.config(t, 9)
	run(c.ShowKeys())
	if tab, i := c.Active(); i != 9 || tab.Title() != "keys" {
		t.Fatal("ShowKeys selects the keys tab")
	}
	view := plain(c.View(140, 20))
	if !strings.Contains(view, "palette") || !strings.Contains(view, "alt+space") || strings.Contains(view, "↑") || strings.Contains(view, "⌫") {
		t.Fatalf("keys read by their typed names\n%s", view)
	}
	if c.Status() != path {
		t.Fatalf("the frame trailer names the keybind file %q", c.Status())
	}
	home, _ := os.UserHomeDir()
	if homed(filepath.Join(home, ".config", "icebath", keys.FileName)) != "~/.config/icebath/"+keys.FileName || homed("/etc/x") != "/etc/x" {
		t.Fatal("paths under home read with a tilde")
	}
	te.press(c, "/")
	typeText(c, "window.zoom")
	te.press(c, "enter")
	l := c.keys
	if len(l.shown) != 1 || l.data[l.index()][1] != "window.zoom" {
		t.Fatalf("filtered to one action %d", len(l.shown))
	}
	te.press(c, "enter")
	if !c.Raw() || !slices.Equal(c.Contexts(), []string{keys.Capture}) || !strings.Contains(plain(c.View(140, 20)), "press a key for window.zoom") {
		t.Fatalf("enter waits for a key %v", c.Contexts())
	}
	if _, ok := find[keys.ChangedMsg](te.pressed(c, "alt+z")); !ok || km.First("window.zoom") != "alt+z" || c.Raw() {
		t.Fatalf("the captured chord binds the action %q", km.First("window.zoom"))
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"window.zoom": ["alt+z"]`) {
		t.Fatalf("the file holds the new bind\n%s", data)
	}
	if !strings.Contains(plain(c.View(140, 20)), "*") {
		t.Fatal("a custom bind is marked")
	}
	te.press(c, "t")
	if !l.Editing() || l.edit.Text != "alt+z" {
		t.Fatal("t types the keys of the action")
	}
	te.press(c, "ctrl+u")
	typeText(c, "alt+shift+z, ctrl+pgdn")
	te.press(c, "enter")
	if km.DisplayAll("window.zoom") != "alt+shift+z ctrl+pgdown" {
		t.Fatalf("typed keys bind by name %q", km.DisplayAll("window.zoom"))
	}
	te.press(c, "t")
	te.press(c, "ctrl+u")
	typeText(c, "alt+space")
	if !intent(te.pressed(c, "enter")) || km.DisplayAll("window.zoom") != "alt+shift+z ctrl+pgdown" {
		t.Fatal("a key another action holds is refused")
	}
	te.press(c, "a")
	te.press(c, "tab")
	if km.DisplayAll("window.zoom") != "alt+shift+z ctrl+pgdown tab" {
		t.Fatalf("a adds a chord %q", km.DisplayAll("window.zoom"))
	}
	te.press(c, "enter")
	te.press(c, "esc")
	if c.Raw() {
		t.Fatal("esc cancels the capture")
	}
	te.press(c, "o")
	if detail := plain(c.View(140, 30)); !strings.Contains(detail, "default alt+f") || !strings.Contains(detail, "global, everywhere") {
		t.Fatalf("the detail shows the default and the context\n%s", detail)
	}
	te.press(c, "o")
	te.press(c, "d")
	if km.First("window.zoom") != "" {
		t.Fatal("d unbinds")
	}
	te.press(c, "r")
	if !km.AtDefault("window.zoom") {
		t.Fatal("r resets")
	}
	l.View(140, 20)
	l.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 3, Y: render.TableHead})
	if l.capture == nil {
		t.Fatal("clicking the selected row starts a rebind")
	}
	l.Update(wm.FocusMsg{})
	if l.capture != nil {
		t.Fatal("losing focus stops waiting")
	}
}

func TestDetailWrapsWholeAndScrolls(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 7)
	l := c.tools
	te.press(c, "o")
	if !l.detailShown() || !slices.Contains(l.Contexts(), keys.Detail) {
		t.Fatal("the detail opens and brings its scroll keys")
	}
	words := strings.Join(strings.Fields(l.detail(l.index())[0]), " ")
	lines := strings.Split(plain(c.View(48, 80)), "\n")
	var body []string
	for _, line := range lines[2+l.pane.top:] {
		r := []rune(line)
		body = append(body, string(r[:max(len(r)-2, 0)]))
	}
	if got := strings.Join(strings.Fields(strings.Join(body, " ")), " "); !strings.Contains(got, words) {
		t.Fatalf("every word of the description survives the wrap\nwant %q\nin %q", words, got)
	}
	lines = strings.Split(plain(c.View(60, 16)), "\n")
	if l.pane.total <= l.pane.rows || !strings.HasSuffix(lines[2+l.pane.top], "⇞") || !strings.HasSuffix(lines[len(lines)-1], "⇟") {
		t.Fatalf("a long detail gets a scrollbar\n%s", strings.Join(lines, "\n"))
	}
	te.press(c, "pgdown")
	if l.pane.scroll == 0 {
		t.Fatal("pgdown pages the detail")
	}
	te.press(c, "pgup")
	if l.pane.scroll != 0 {
		t.Fatal("pgup pages it back")
	}
	l.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, Y: l.pane.top + 1})
	if l.pane.scroll != 1 || l.sel != 0 {
		t.Fatal("the wheel over the detail scrolls it")
	}
	l.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, Y: render.TableHead})
	if l.sel != 1 {
		t.Fatal("the wheel over the table moves the selection")
	}
	c.View(60, 16)
	if l.pane.scroll != 0 {
		t.Fatal("another row starts its detail at the top")
	}
	te.press(c, "o")
	if l.detailShown() {
		t.Fatal("o folds the detail")
	}
}

func TestFieldsChooseStepTypeAndCheck(t *testing.T) {
	th := theme.Blackwall()
	choice := &field{label: "pick", kind: choiceField, choices: []string{"one", "two", "three"}, value: "one"}
	choice.nudge(-1)
	choice.nudge(1)
	choice.nudge(1)
	choice.nudge(1)
	if choice.value != "three" {
		t.Fatalf("choices step and hold at the ends %q", choice.value)
	}
	choice.typeText("o")
	if choice.value != "one" || choice.edit("clear") || choice.unset() {
		t.Fatal("typing jumps to the next choice it starts and choices take no editing")
	}
	rows, hits := chips(choice.choices, "two", true, 12, th)
	if len(rows) != 2 || len(hits) != 3 || hits[2].row != 1 || hits[2].x0 != 0 {
		t.Fatalf("chips wrap at the width %q %+v", rows, hits)
	}
	n := &field{label: "count", kind: numberField, integer: true, optional: true, lo: 10, hi: 100, step: 10, start: 40}
	n.nudge(1)
	if n.value != "40" {
		t.Fatal("an unset number steps to its start")
	}
	n.nudge(1)
	n.setNumber(1000)
	if n.value != "100" {
		t.Fatalf("numbers hold inside the range %q", n.value)
	}
	n.setNumber(33)
	if n.value != "30" {
		t.Fatalf("numbers snap to the step %q", n.value)
	}
	n.set("")
	n.typeText("4x5")
	if n.value != "45" || n.problem() != "" {
		t.Fatalf("numbers take digits only %q", n.value)
	}
	n.set("5")
	if !strings.Contains(n.problem(), "between 10 and 100") {
		t.Fatal("a number off the range is a problem")
	}
	n.set("12.5")
	if !strings.Contains(n.problem(), "whole") {
		t.Fatal("an integer takes no fraction")
	}
	n.set("abc")
	if !strings.Contains(n.problem(), "must be a number") {
		t.Fatal("a word is no number")
	}
	if !n.unset() || n.value != "" || n.problem() != "" {
		t.Fatal("an optional number unsets")
	}
	row, hits, _ := n.slider(60, look{focused: true}, sliderColumns([]*field{n}), th)
	if !strings.Contains(plain(row), "-") || len(hits) != 2 || hits[1].choice != hitTrack {
		t.Fatalf("an unset number shows a dash beside its track %q", plain(row))
	}
	n.click(hits[1], hits[1].x1-1)
	if n.value != "100" {
		t.Fatalf("a click on the right end of the track sets the high end %q", n.value)
	}
	if decimals(0.05) != 2 || decimals(1) != 0 || decimals(0.1) != 1 {
		t.Fatal("decimals")
	}
	text := &field{label: "name", kind: textField, value: "hello world", cursor: 11}
	text.edit("delete.word")
	if text.value != "hello " {
		t.Fatalf("delete word %q", text.value)
	}
	text.edit("home")
	text.typeText(">")
	text.edit("end")
	text.edit("delete.back")
	if text.value != ">hello" || text.problem() != "" {
		t.Fatalf("the cursor edits in place %q", text.value)
	}
	text.nudge(-3)
	if text.cursor != 3 {
		t.Fatal("left and right move the cursor of text")
	}
	text.set("")
	if text.problem() != "name is required" || text.unset() {
		t.Fatal("a required text field asks for a value")
	}
	text.check = func(v string) error { return errors.New("no " + v) }
	text.set("x")
	if text.problem() != "no x" {
		t.Fatal("checks run on the value")
	}
	secret := &field{kind: secretField, value: "abc", cursor: 3}
	if row, _ := secret.box(20, look{}, th); strings.Contains(plain(row), "abc") || !strings.Contains(plain(row), "•••") {
		t.Fatal("secrets are masked")
	}
	long := &field{kind: textField, value: strings.Repeat("a", 30) + "END", cursor: 33}
	if row, x := long.box(12, look{focused: true, typing: true}, th); !strings.Contains(plain(row), "END") || x != 10 {
		t.Fatalf("the box scrolls to keep the cursor in view %q %d", plain(row), x)
	}
}

func TestFormWalksFieldsAndRespondsToTheMouse(t *testing.T) {
	te := newEnv(t)
	saved := 0
	name := &field{key: "name", label: "name", kind: textField, help: "what it is called"}
	kind := &field{key: "kind", label: "kind", kind: choiceField, choices: []string{"a", "b"}, value: "a"}
	extra := &field{key: "extra", label: "extra", kind: textField, optional: true, show: func() bool { return kind.value == "b" }}
	f := newForm(te.env, "TEST", "▪", []*field{name, kind, extra}, func() (tea.Cmd, error) { saved++; return nil, nil })
	if f.Key() != "form:TEST" || f.Glyph() != "▪" || !slices.Equal(f.Contexts(), []string{keys.Form}) {
		t.Fatal("identity")
	}
	if w, h := f.MinSize(); w != formWidth || h < 6 {
		t.Fatalf("size %d %d", w, h)
	}
	te.press(f, "tab")
	te.press(f, "tab")
	if f.focused() != name {
		t.Fatal("tab wraps over the hidden field")
	}
	if !strings.Contains(plain(f.View(60, 12)), "what it is called") || f.Cursor() == nil {
		t.Fatal("the help and the cursor of the focus show")
	}
	f.Update(tea.PasteMsg{Content: "pasted"})
	if name.value != "pasted" {
		t.Fatal("paste types into the focus")
	}
	f.View(60, 12)
	for _, h := range f.hits {
		if h.field == kind && h.hit.choice == 1 {
			f.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: h.x0, Y: h.y})
		}
	}
	if kind.value != "b" || f.focused() != kind || !extra.visible() {
		t.Fatal("a click picks the choice and shows the field it reveals")
	}
	if hs := f.Hints(); !slices.ContainsFunc(hs, func(h wm.Hint) bool { return h.Action == "form.left" }) {
		t.Fatal("a choice offers left and right")
	}
	f.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if f.focused() != extra {
		t.Fatal("the wheel walks the fields")
	}
	if hs := f.Hints(); !slices.ContainsFunc(hs, func(h wm.Hint) bool { return h.Action == "form.unset" }) {
		t.Fatal("an optional field offers unset")
	}
	f.View(60, 12)
	var save formHit
	for _, h := range f.hits {
		if h.button == saveButton {
			save = h
		}
	}
	if !intent(run(second(f.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: save.x0, Y: save.y})))) || saved != 1 {
		t.Fatal("the save button saves and closes")
	}
	tall := newForm(te.env, "TALL", "▪", nil, func() (tea.Cmd, error) { return nil, nil })
	for i := range 12 {
		tall.fields = append(tall.fields, &field{key: string(rune('a' + i)), label: string(rune('a' + i)), kind: textField, optional: true})
	}
	tall.at = 11
	tall.View(60, 8)
	if tall.scroll == 0 || tall.Cursor() == nil {
		t.Fatal("a dialog taller than its room scrolls to the focus")
	}
	if _, ok := find[wm.Intent](te.pressed(tall, "esc")); !ok {
		t.Fatal("esc closes")
	}
	refused := newForm(te.env, "NO", "▪", []*field{{key: "x", label: "x", kind: textField, value: "v"}}, func() (tea.Cmd, error) { return nil, errors.New("disk full") })
	if run(refused.save()) != nil || !strings.Contains(plain(refused.View(60, 10)), "disk full") {
		t.Fatal("a refused save shows why and stays")
	}
}

func TestDialogsSpaceFieldsToTheirRoomAndKeepWellsApart(t *testing.T) {
	blocks := []stack{{rows: 2}, {rows: 1, well: true}, {rows: 1, well: true}, {rows: 3}, {rows: 1}}
	if all := spaceOut(blocks, 10); !slices.Equal(all, []bool{false, true, true, true, true}) || touching(blocks, all) {
		t.Fatalf("room for every gap spaces every pair %v", all)
	}
	if some := spaceOut(blocks, 2); !slices.Equal(some, []bool{false, true, false, false, true}) || !touching(blocks, some) {
		t.Fatalf("short room spreads gaps around wrapped blocks first and leaves the rest tight %v", some)
	}
	if none := spaceOut(blocks, 0); slices.Contains(none, true) {
		t.Fatalf("no room no gaps %v", none)
	}
	te := newEnv(t)
	saved := 0
	a := &field{key: "a", label: "alpha", kind: textField, optional: true, help: "first"}
	b := &field{key: "b", label: "beta", kind: textField, optional: true}
	c := &field{key: "c", label: "gamma", kind: choiceField, choices: []string{"x", "y"}, value: "x"}
	f := newForm(te.env, "SPACED", "▪", []*field{a, b, c}, func() (tea.Cmd, error) { saved++; return nil, nil })
	_, h := f.MinSize()
	tall := strings.Split(plain(f.View(60, h)), "\n")
	if !strings.Contains(tall[0], "alpha") || strings.TrimSpace(tall[1]) != "" || !strings.Contains(tall[2], "beta") || strings.Contains(tall[2], "-") || !strings.HasPrefix(tall[0], "┃") {
		t.Fatalf("with room each field stands apart, the focus barred and empty inputs in faint wells\n%s", strings.Join(tall, "\n"))
	}
	var save formHit
	for _, hh := range f.hits {
		if hh.button == saveButton {
			save = hh
		}
	}
	f.MinSize()
	if run(second(f.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: save.x0, Y: save.y}))) == nil || saved != 1 {
		t.Fatal("measuring the dialog leaves the clicks of its last paint in place")
	}
	short := strings.Split(plain(f.View(60, h-2)), "\n")
	if !strings.Contains(short[1], "beta") || !strings.Contains(short[1], "-") {
		t.Fatalf("without room the fields close up and inputs that would touch drop their wells\n%s", strings.Join(short, "\n"))
	}
	box := plain(f.View(60, 4))
	if !strings.Contains(box, "save") {
		t.Fatalf("the buttons stay in view when the fields scroll\n%s", box)
	}
}

func TestSlidersShareColumns(t *testing.T) {
	te := newEnv(t)
	c, _ := te.config(t, 3)
	view := plain(c.View(120, 40))
	starts, ends := map[int]bool{}, map[int]bool{}
	for _, row := range strings.Split(view, "\n") {
		i := strings.IndexAny(row, "━─●")
		if i < 0 || strings.Contains(row, "profiles") {
			continue
		}
		j := strings.LastIndexAny(row, "━─●")
		starts[ansi.StringWidth(row[:i])] = true
		ends[ansi.StringWidth(row[:j])] = true
	}
	if len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("every track starts and ends in the same column %v %v\n%s", starts, ends, view)
	}
}

func TestListerAsksAndOffersOnlyWhatApplies(t *testing.T) {
	te := newEnv(t)
	ran := 0
	l := &lister{env: te.env, title: "t", id: "t", ctx: keys.Rules, cols: []render.Column{{Title: "A"}}, rows: func() [][]string { return nil }}
	l.free = map[string]bool{"config.rule.add": true}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"config.rule.add":    func(*lister, int) tea.Cmd { ran++; return nil },
		"config.rule.delete": func(*lister, int) tea.Cmd { ran += 10; return nil },
	}
	l.empty = func() string { return "empty tab" }
	if !strings.Contains(plain(l.View(40, 5)), "empty tab") {
		t.Fatal("the empty line shows")
	}
	if h := l.Hints(); len(h) != 1 || h[0].Action != "config.rule.add" {
		t.Fatalf("without rows only free actions show %+v", h)
	}
	l.act("config.rule.delete")
	l.act("config.rule.add")
	if ran != 1 {
		t.Fatal("actions that need a row wait for one")
	}
	yes := 0
	l.confirm("sure?", func() tea.Cmd { yes++; return nil })
	if h := l.Hints(); len(h) != 2 || h[0].Action != "confirm.yes" || !strings.Contains(plain(l.View(40, 5)), "sure?") {
		t.Fatal("a question takes the foot and the legend")
	}
	l.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 3})
	l.act("confirm.yes")
	l.act("confirm.yes")
	if yes != 1 || l.ask != nil {
		t.Fatal("yes runs once")
	}
	l.confirm("again?", func() tea.Cmd { yes++; return nil })
	l.Update(wm.FocusMsg{})
	if l.ask != nil || yes != 1 {
		t.Fatal("losing focus drops the question")
	}
}
