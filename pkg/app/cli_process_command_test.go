package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Stand in CLI with an armed resume list on session s1 and an empty config
func dispatchCLI() *CLI {
	c := bareCLI()
	c.pendingResumeSessions = []map[string]any{{"id": "x"}}
	c.sessionID = "s1"
	c.config = map[string]any{}
	return c
}

// Replaces one dispatch table entry for the case, restoring the real handler afterwards
func swapSlashHandler(t *testing.T, name string, fn slashHandler) {
	t.Helper()
	prev := slashDispatch[name]
	slashDispatch[name] = fn
	t.Cleanup(func() { slashDispatch[name] = prev })
}

// Records every pre_command hook call on the plugin manager of the test home
func recordPreCommandHook(t *testing.T) *[]map[string]any {
	t.Helper()
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	calls := &[]map[string]any{}
	m := plugins.GetPluginManager(context.Background())
	m.TrackHook(plugins.NewContext(&plugins.Manifest{Name: "observer", Key: "observer"}, m), "pre_command",
		func(_ context.Context, kwargs map[string]any) (any, error) {
			*calls = append(*calls, kwargs)
			return nil, nil
		}, false)
	return calls
}

// Console streams reading the given answers with the output discarded
func answerConsole(t *testing.T, input string) {
	t.Helper()
	t.Cleanup(SetStreams(&cliui.Console{In: strings.NewReader(input), Out: io.Discard, StdinTTY: func() bool { return false },
		StdoutTTY: func() bool { return false }}, io.Discard))
}

func TestDispatchReturnSemanticsAndSideEffects(t *testing.T) {
	recordPrints(t)
	hook := recordPreCommandHook(t)
	c := dispatchCLI()
	yoloCalls := 0
	swapSlashHandler(t, "yolo", noArg(func(*CLI) { yoloCalls++ }))
	if !c.processCommand("/yolo") {
		t.Fatal("/yolo asked to exit")
	}
	if yoloCalls != 1 || len(*hook) != 1 || (*hook)[0]["command"] != "yolo" {
		t.Fatalf("toggle calls %d hook calls %v", yoloCalls, *hook)
	}
	if c.pendingResumeSessions != nil {
		t.Fatalf("pending resume kept %v", c.pendingResumeSessions)
	}

	c = dispatchCLI()
	var resumeArgs []string
	swapSlashHandler(t, "resume", withArg(func(_ *CLI, cmd string) { resumeArgs = append(resumeArgs, cmd) }))
	if !c.processCommand("/resume 2") {
		t.Fatal("/resume asked to exit")
	}
	if len(resumeArgs) != 1 || resumeArgs[0] != "/resume 2" {
		t.Fatalf("resume calls %q", resumeArgs)
	}
	if len(c.pendingResumeSessions) != 1 || c.pendingResumeSessions[0]["id"] != "x" {
		t.Fatalf("pending resume %v", c.pendingResumeSessions)
	}

	c = dispatchCLI()
	if c.processCommand("/exit") {
		t.Fatal("/exit kept the REPL")
	}

	c = dispatchCLI()
	handoff := false
	swapSlashHandler(t, "handoff", func(*CLI, string) bool { return handoff })
	if c.processCommand("/handoff telegram") {
		t.Fatal("finished handoff kept the REPL")
	}
	handoff = true
	if !c.processCommand("/handoff telegram") {
		t.Fatal("kept handoff left the REPL")
	}

	answerConsole(t, "once\n")
	if c.processCommand("/update") {
		t.Fatal("confirmed update kept the REPL")
	}
	if len(c.pendingRelaunch) != 1 || c.pendingRelaunch[0] != "update" {
		t.Fatalf("pending relaunch %q", c.pendingRelaunch)
	}
	c.pendingRelaunch = nil
	answerConsole(t, "cancel\n")
	if !c.processCommand("/update") {
		t.Fatal("cancelled update left the REPL")
	}
	if c.pendingRelaunch != nil {
		t.Fatalf("cancelled update armed relaunch %q", c.pendingRelaunch)
	}
}

func TestUnknownCommandFallsThrough(t *testing.T) {
	rec := recordPrints(t)
	c := dispatchCLI()
	if !c.processCommand("/definitely-not-a-command x") {
		t.Fatal("unknown command asked to exit")
	}
	out := strings.Join(rec.plain(), "\n")
	if out != "Unknown command /definitely-not-a-command — nothing was sent.\nType /help for the full list." {
		t.Fatalf("unregistered fallback output %q", out)
	}
}

func TestPendingDisarmedByOtherCommand(t *testing.T) {
	recordPrints(t)
	c := resumeCLI(t)
	c.pendingResumeSessions = []map[string]any{{"id": "sess_002", "title": "Coding"}}
	c.processCommand("/help")
	if c.pendingResumeSessions != nil {
		t.Fatalf("pending resume kept %v", c.pendingResumeSessions)
	}
}

// CLI over a fresh home whose config holds the quick commands, with no bundles and every print recorded
func quickCLI(t *testing.T, quick any) (*CLI, *printRecorder) {
	t.Helper()
	t.Setenv("COLUMNS", "500")
	rec := recordPrints(t)
	previousBundles := slashSkillBundles
	slashSkillBundles = func(context.Context) skills.SkillCommands { return skills.SkillCommands{} }
	t.Cleanup(func() { slashSkillBundles = previousBundles })
	c := bareCLI()
	c.console = console()
	c.config = map[string]any{"quick_commands": quick}
	c.pendingInput = &inputQueue{}
	return c, rec
}

// Python exception a command raises out of the dispatch, nil when it returns
func raisedBy(t *testing.T, c *CLI, command string) (raised *commands.RaisedError) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			var isRaised bool
			if raised, isRaised = rec.(*commands.RaisedError); !isRaised {
				t.Fatalf("%s panicked with %v", command, rec)
			}
		}
	}()
	c.processCommand(command)
	return nil
}

func TestQuickCommandsRaiseThePythonErrorsOfAMalformedConfig(t *testing.T) {
	for _, tc := range []struct {
		quick any
		msg   string
	}{
		{nil, "argument of type 'NoneType' is not a container or iterable"},
		{5, "argument of type 'int' is not a container or iterable"},
		{2.5, "argument of type 'float' is not a container or iterable"},
		{true, "argument of type 'bool' is not a container or iterable"},
		{[]any{"zzz"}, "list indices must be integers or slices, not str"},
		{"xzzzx", "string indices must be integers, not 'str'"},
		{map[string]any{"zzz": "echo"}, "'str' object has no attribute 'get'"},
		{map[string]any{"zzz": nil}, "'NoneType' object has no attribute 'get'"},
		{map[string]any{"zzz": []any{1}}, "'list' object has no attribute 'get'"},
		{map[string]any{"zzz": map[string]any{"type": "alias", "target": nil}}, "'NoneType' object has no attribute 'strip'"},
		{map[string]any{"zzz": map[string]any{"type": "alias", "target": 5}}, "'int' object has no attribute 'strip'"},
		{map[string]any{"zzz": map[string]any{"type": "alias", "target": []any{"/status"}}}, "'list' object has no attribute 'strip'"},
	} {
		c, _ := quickCLI(t, tc.quick)
		if raised := raisedBy(t, c, "/zzz"); raised == nil || raised.Msg != tc.msg {
			t.Fatalf("%v raised %+v, want %s", tc.quick, raised, tc.msg)
		}
		if _, _, err := runSlashWorkerCommand(c, "/zzz"); err == nil || err.Error() != tc.msg {
			t.Fatalf("%v answered the slash worker with %v, want the str of the exception %q", tc.quick, err, tc.msg)
		}
	}
	for _, quick := range []any{[]any{"other", 7}, "abc"} {
		c, rec := quickCLI(t, quick)
		if raised := raisedBy(t, c, "/zzz"); raised != nil {
			t.Fatalf("%v raised %+v", quick, raised)
		}
		if !strings.Contains(printedText(rec), "Unknown command /zzz") {
			t.Fatalf("%v printed %q", quick, printedText(rec))
		}
	}
}

func TestQuickAliasCycleRaisesTheRecursionErrorOfOneTypedCommand(t *testing.T) {
	c, rec := quickCLI(t, map[string]any{
		"loop1": map[string]any{"type": "alias", "target": "/loop2"},
		"loop2": map[string]any{"type": "alias", "target": "loop1 more"},
		"a":     map[string]any{"type": "alias", "target": "/b one"},
		"b":     map[string]any{"type": "exec"},
	})
	if raised := raisedBy(t, c, "/loop1 x"); raised == nil || raised.Msg != "maximum recursion depth exceeded" {
		t.Fatalf("alias cycle raised %+v", raised)
	}
	for range 2 {
		if raised := raisedBy(t, c, "/a two"); raised != nil {
			t.Fatalf("a repeated alias chain raised %+v", raised)
		}
	}
	if got := strings.Count(printedText(rec), "Quick command '/b' has no command defined"); got != 2 {
		t.Fatalf("alias chain ran %d times: %q", got, printedText(rec))
	}
	if _, _, err := runSlashWorkerCommand(c, "/loop2"); err == nil || err.Error() != "maximum recursion depth exceeded" {
		t.Fatalf("slash worker answered %v", err)
	}
}

func TestQuickCommandRepliesAreLocalizedOnTheCommandPath(t *testing.T) {
	quick := map[string]any{
		"qa": map[string]any{"type": "alias"}, "blank": map[string]any{"type": "alias", "target": "   "},
		"qt": map[string]any{"type": "x"}, "untyped": map[string]any{"command": "echo"},
		"qe": map[string]any{"type": "exec"}, "emptystr": map[string]any{"type": "exec", "command": ""},
		"zero": map[string]any{"type": "exec", "command": 0}, "no": map[string]any{"type": "exec", "command": false},
	}
	for lang := range i18n.BundledLanguageInfo {
		c, rec := quickCLI(t, quick)
		t.Setenv("ICEBATH_LANGUAGE", lang)
		i18n.ResetLanguageCache()
		catalog := i18n.LoadBundled(lang)
		for _, tc := range []struct{ line, key, command string }{
			{"/QA", "cli.quick.no_target", "/qa"}, {"/BLANK arg", "cli.quick.no_target", "/blank"},
			{"/qt", "cli.quick.unsupported_type", "/qt"}, {"/untyped", "cli.quick.unsupported_type", "/untyped"},
			{"/qe", "cli.quick.no_command", "/qe"}, {"/emptystr", "cli.quick.no_command", "/emptystr"},
			{"/zero", "cli.quick.no_command", "/zero"}, {"/no", "cli.quick.no_command", "/no"},
		} {
			want, err := i18n.PyFormat(catalog[tc.key], map[string]any{"command": tc.command})
			if err != nil || catalog[tc.key] == "" {
				t.Fatalf("%s lacks %s: %v", lang, tc.key, err)
			}
			if raised := raisedBy(t, c, tc.line); raised != nil {
				t.Fatalf("%s %s raised %+v", lang, tc.line, raised)
			}
			lines := rec.plain()
			if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) != want {
				t.Fatalf("%s %s printed %q, want %q", lang, tc.line, lines, want)
			}
		}
	}
	i18n.ResetLanguageCache()
}

// Skills of the given names in the home the test set, each a one line fixture
func writeSlashSkills(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		dir := filepath.Join(os.Getenv("ICEBATH_HOME"), "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: fixture "+name+"\n---\n\nBody of "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrefixExpansionLinesAreLocalizedOnTheCommandPath(t *testing.T) {
	c, rec := quickCLI(t, map[string]any{})
	writeSlashSkills(t, "zzalpha", "zzbravo", "qqlong-one", "qqx")
	var ran []string
	previous := slashRunSkill
	slashRunSkill = func(_ *CLI, baseCmd string, _ map[string]any, rest string) { ran = append(ran, baseCmd+"|"+rest) }
	t.Cleanup(func() { slashRunSkill = previous })
	c.processCommand("/ZZ some args")
	lines := rec.plain()
	if len(lines) < 2 || lines[len(lines)-2] != "Ambiguous command: /zz some args" || lines[len(lines)-1] != "Did you mean: /zzalpha, /zzbravo?" {
		t.Fatalf("ambiguous lines %q", lines)
	}
	c.processCommand("/qq Keep Case")
	if len(ran) != 1 || ran[0] != "/qqx|Keep Case" {
		t.Fatalf("unique shortest expansion ran %v", ran)
	}
	t.Setenv("ICEBATH_LANGUAGE", "de")
	i18n.ResetLanguageCache()
	t.Cleanup(i18n.ResetLanguageCache)
	want, err := i18n.PyFormat(i18n.LoadBundled("de")["cli.command.ambiguous"], map[string]any{"command": "/zz"})
	if err != nil || want == "" {
		t.Fatalf("german ambiguous line %q %v", want, err)
	}
	c.processCommand("/zz")
	if lines = rec.plain(); lines[len(lines)-2] != want {
		t.Fatalf("german ambiguous %q want %q", lines[len(lines)-2], want)
	}
}

func TestQuickCommandWinsOverASameNamedSkillAndBundle(t *testing.T) {
	c, rec := quickCLI(t, map[string]any{"mygif": map[string]any{"type": "alias"}})
	writeSlashSkills(t, "mygif")
	var ran []string
	prevBundles, prevBundle, prevSkill := slashSkillBundles, slashRunSkillBundle, slashRunSkill
	slashSkillBundles = func(context.Context) skills.SkillCommands { return skills.SkillCommands{"/mygif": {"name": "gifs"}} }
	slashRunSkillBundle = func(*CLI, string, map[string]any, string) { ran = append(ran, "bundle") }
	slashRunSkill = func(*CLI, string, map[string]any, string) { ran = append(ran, "skill") }
	t.Cleanup(func() { slashSkillBundles, slashRunSkillBundle, slashRunSkill = prevBundles, prevBundle, prevSkill })
	c.processCommand("/mygif")
	if lines := rec.plain(); len(ran) != 0 || len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) != "Quick command '/mygif' has no target defined" {
		t.Fatalf("quick command lost to %v, printed %q", ran, lines)
	}
}

func TestBoundCommandOnTheWorkerPathSeedsTheNextTurn(t *testing.T) {
	c, _ := quickCLI(t, map[string]any{"imp": map[string]any{"type": "alias", "target": "/boundfix audit the header"}})
	c.sessionID = "s-bound"
	var sessions []string
	commands.Register(commands.Def{Name: "boundfix", Plugin: "fixture", Handler: func(ctx context.Context, inv *commands.Invocation) error {
		host := inv.Host.(interface {
			SubmitUserTurn(context.Context, string) error
			TurnSession() (string, string)
		})
		task, platform := host.TurnSession()
		sessions = append(sessions, task+"|"+platform)
		switch inv.Args {
		case "fail":
			return errors.New("skill missing")
		case "nothing":
			return nil
		}
		return host.SubmitUserTurn(ctx, "TURN "+inv.Args)
	}})
	t.Cleanup(func() { commands.Default.Remove("boundfix") })
	for line, want := range map[string]string{"/imp": "TURN audit the header", "/boundfix polish": "TURN polish"} {
		if _, seed, err := runSlashWorkerCommand(c, line); err != nil || seed != want {
			t.Fatalf("%s seeded %v %v, want %q", line, seed, err, want)
		}
	}
	if len(sessions) != 2 || sessions[0] != "s-bound|" || sessions[1] != sessions[0] {
		t.Fatalf("turn sessions %v", sessions)
	}
	for line, want := range map[string]string{"/boundfix fail": "Plugin command error: skill missing", "/boundfix nothing": "Plugin command error: /boundfix built no turn"} {
		if output, seed, err := runSlashWorkerCommand(c, line); err != nil || seed != "" || !strings.Contains(output, want) {
			t.Fatalf("%s answered %q seed %v %v", line, output, seed, err)
		}
	}
	shadowed, rec := quickCLI(t, map[string]any{"boundfix": map[string]any{"type": "alias"}})
	shadowed.processCommand("/boundfix polish")
	if lines := rec.plain(); shadowed.pendingAgentSeed != nil || len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) != "Quick command '/boundfix' has no target defined" {
		t.Fatalf("a same named quick command lost to the bound command: %v %q", shadowed.pendingAgentSeed, lines)
	}
}
