package app

import (
	"strings"
	"testing"
	"time"
)

// Opens the named blocking prompt on the CLI
func openBlockingPrompt(c *CLI, name string) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	switch name {
	case "_approval_state":
		c.approvalState = &approvalModalState{}
	case "_clarify_state":
		c.clarifyState = &clarifyModalState{}
	case "_secret_state":
		c.secretState = &secretModalState{}
	case "_sudo_state":
		c.sudoState = &sudoModalState{}
	case "_slash_confirm_state":
		c.slashConfirmState = &slashConfirmModalState{}
	}
}

// Runs fn on its own goroutine and fails when it has not returned within three seconds
func returnsPromptly(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s never returned", what)
	}
}

func TestPromptPaintYieldsMonitorButOrdinaryPaintDoesNot(t *testing.T) {
	for _, paintSource := range []string{"modal", "tick"} {
		t.Run(paintSource, func(t *testing.T) {
			for _, name := range []string{"_approval_state", "_clarify_state", "_secret_state", "_sudo_state", "_slash_confirm_state"} {
				t.Setenv("ICEBATH_HOME", t.TempDir())
				dockCli := dockCLI(t, nil, "")
				installDock(dockCli)
				dockCli.subagentMonitor.mu.Lock()
				dockCli.subagentMonitor.entries = []map[string]any{{}}
				dockCli.subagentMonitor.mu.Unlock()
				if !dockCli.tui.subagentDockWidget.filter() {
					t.Fatalf("%s dock hidden without a prompt", name)
				}
				openBlockingPrompt(dockCli, name)
				if dockCli.tui.subagentDockWidget.filter() {
					t.Fatalf("%s dock crowds the blocking prompt", name)
				}
				cli := bareCLI()
				monitor := newSubagentMonitor(cli)
				cli.subagentMonitor = monitor
				h := newMonitorHarness(t, monitor, 80, 40)
				monitor.app = h.app
				h.start(cli.t("cli.subagents.roster_empty", nil)[:20])
				monitor.mu.Lock()
				monitor.entries = []map[string]any{{"subagent_id": "painted", "goal": "ordinary paint", "elapsed": 0, "status": "running", "last_tool": ""}}
				monitor.mu.Unlock()
				returnsPromptly(t, "ordinary paint", cli.paintNow)
				h.waitShows("ordinary paint")
				if h.exited(100 * time.Millisecond) {
					t.Fatalf("%s ordinary paints must not dismiss the monitor", name)
				}
				openBlockingPrompt(cli, name)
				if paintSource == "tick" {
					returnsPromptly(t, "monitor tick", monitor.tick)
				} else {
					returnsPromptly(t, "prompt paint", cli.paintNow)
				}
				if !h.exited(time.Second) {
					t.Fatalf("%s remained hidden behind the monitor, screen:\n%s", name, strings.Join(h.screen(), "\n"))
				}
			}
		})
	}
}

func TestSecretCallbackYieldsMonitorAndRestoresComposer(t *testing.T) {
	c, fake := newModalTestCLI(t, nil)
	fake.SetBuffer("keep my draft", 4)
	monitor := newSubagentMonitor(c)
	c.subagentMonitor = monitor
	h := newMonitorHarness(t, monitor, 80, 40)
	monitor.app = h.app
	h.start(c.t("cli.subagents.roster_empty", nil)[:20])
	type outcome struct {
		result map[string]any
		err    error
	}
	results := make(chan outcome, 1)
	go func() {
		r, err := c.secretCaptureCallback("FIXTURE_SECRET", "Owned fixture", nil)
		results <- outcome{r, err}
	}()
	answered := false
	t.Cleanup(func() {
		if !answered {
			c.submitSecretResponse("")
			<-results
		}
	})
	if !h.exited(time.Second) {
		t.Fatal("secret arrival did not immediately reclaim input")
	}
	if fake.BufferText() != "" {
		t.Fatalf("draft must not be submitted as a secret, buffer %q", fake.BufferText())
	}
	waitFor(t, "secret panel", func() bool { return c.secretModal() != nil })
	c.submitSecretResponse("")
	answered = true
	var got outcome
	select {
	case got = <-results:
	case <-time.After(3 * time.Second):
		t.Fatal("secret callback never returned")
	}
	if got.err != nil || got.result["skipped"] != true {
		t.Fatalf("result %v %v", got.result, got.err)
	}
	if fake.BufferText() != "keep my draft" || fake.BufferCursor() != 4 {
		t.Fatalf("draft %q cursor %d", fake.BufferText(), fake.BufferCursor())
	}
}
