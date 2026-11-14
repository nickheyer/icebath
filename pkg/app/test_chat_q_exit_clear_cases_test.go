package app

import (
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Records every exit clear instead of wiping the console
func recordExitClears(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	prev := clearTerminalOnExit
	t.Cleanup(func() { clearTerminalOnExit = prev })
	clearTerminalOnExit = func(*CLI) { *calls = append(*calls, "clear") }
	return calls
}

func TestPrintExitSummarySkipsClearWhenClearScreenFalse(t *testing.T) {
	recordPrints(t)
	calls := recordExitClears(t)
	c := bareCLI()
	c.conversationHistory = []map[string]any{}
	c.printExitSummary(false)
	for _, call := range *calls {
		if call == "clear" {
			t.Fatal("_clear_terminal_on_exit should NOT be called when clear_screen=False")
		}
	}
}

func TestPrintExitSummaryStillClearsInInteractivePath(t *testing.T) {
	for _, path := range interactiveExits {
		t.Run(path.name, func(t *testing.T) {
			c, _ := resumeHintCLI(t)
			calls := recordExitClears(t)
			c.conversationHistory = []map[string]any{{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}}
			c.sessionStart = time.Now().Add(-time.Minute)
			path.exit(t, c)
			cleared := false
			for _, call := range *calls {
				cleared = cleared || call == "clear"
			}
			if !cleared {
				t.Fatal("Interactive mode should still clear the screen (regression test for #38928)")
			}
		})
	}
}

type explodingStdout struct{}

func (explodingStdout) Write([]byte) (int, error) {
	return 0, errors.New("terminal rejects the escape sequence")
}

// A console whose stdout is a tty but whose writes fail, forcing the clear fallback
func installFallbackConsole(t *testing.T) {
	t.Helper()
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: explodingStdout{}, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return true }}, io.Discard)
	t.Cleanup(restore)
}

// Records every clear fallback child instead of running it
func recordClearRuns(t *testing.T) *[]*exec.Cmd {
	t.Helper()
	calls := &[]*exec.Cmd{}
	prev := runClearCommand
	t.Cleanup(func() { runClearCommand = prev })
	runClearCommand = func(cmd *exec.Cmd) error {
		*calls = append(*calls, cmd)
		return nil
	}
	return calls
}

// Replaces the PATH lookup of the clear fallback
func fakeClearWhich(t *testing.T, which func(string) (string, bool)) {
	t.Helper()
	prev := clearCommandWhich
	t.Cleanup(func() { clearCommandWhich = prev })
	clearCommandWhich = which
}

func TestClearFallbackSpawnsNoShell(t *testing.T) {
	if IsWindows() {
		t.Skip("POSIX clear path, the nt path has its own case")
	}
	fakeClearWhich(t, func(exe string) (string, bool) {
		if exe == "clear" {
			return "/usr/bin/" + exe, true
		}
		return "", false
	})
	calls := recordClearRuns(t)
	installFallbackConsole(t)
	bareCLI().wipeConsoleOnExit()
	if len(*calls) != 1 {
		t.Fatalf("calls %d", len(*calls))
	}
	cmd := (*calls)[0]
	if !reflect.DeepEqual(cmd.Args, []string{"/usr/bin/clear"}) {
		t.Fatalf("argv %v", cmd.Args)
	}
	if cmd.Stdin != nil {
		t.Fatalf("stdin %v, want the null device", cmd.Stdin)
	}
	if cmd.SysProcAttr != nil {
		t.Fatalf("creation flags %+v, want none off Windows", cmd.SysProcAttr)
	}
}

func TestClearFallbackSkipsSpawnWhenNoClear(t *testing.T) {
	if IsWindows() {
		t.Skip("POSIX clear lookup")
	}
	fakeClearWhich(t, func(string) (string, bool) { return "", false })
	calls := recordClearRuns(t)
	installFallbackConsole(t)
	bareCLI().wipeConsoleOnExit()
	if len(*calls) != 0 {
		t.Fatal("no clear binary => nothing to spawn")
	}
}
