package app

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/plugins"
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
