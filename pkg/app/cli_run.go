package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Standard streams the interactive prompt reads keys from and draws to
var (
	tuiStdin  = func() *os.File { return os.Stdin }
	tuiStdout = func() *os.File { return os.Stdout }
)

// Runs the interactive loop with the prompt pinned under the transcript until the user exits
func (c *CLI) run() {
	if !c.claimActiveSession("cli", true) {
		return
	}
	c.tuiPrintStartup()
	c.tuiInitRunState()
	app := newPTApplication(tuiStdin(), tuiStdout())
	c.tui.app = app
	kb := c.tuiBuildKeyBindings()
	layout, style := c.tuiBuildLayout(kb, app)
	if err := c.tuiBuildApplication(app, layout, kb, style); err != nil {
		panic(err)
	}
	disablePromptToolkitCPRWarning(app)
	app.afterRender = append(app.afterRender, c.petFlushKittyFrame)
	ready := observability.CliPromptReadyHandler(c.ctx)
	app.afterRender = append(app.afterRender, func() { ready(app) })
	c.uiMu.Lock()
	c.app = app
	c.uiMu.Unlock()
	setTUIApp(app)
	app.guardedPaint = true
	applyBracketedPasteTimeoutPatch()
	c.installResizeRecovery(app)

	go c.tuiSpinnerLoop()
	go c.tuiProcessLoop()
	go c.tuiWakeStartup()

	stopSignals := c.tuiInstallSignalHandlers()
	defer stopSignals()

	if !c.tuiStdinUsable() {
		runCleanup(c.ctx, c.env, true)
		c.printExitSummary(true)
		return
	}
	func() {
		defer func() {
			releasePaints()
			setTUIApp(nil)
			c.tuiShutdown()
		}()
		markTUIInputModesActive()
		if c.tui.multilineShortcuts {
			app.onStart = append(app.onStart, func() { enableExtendedEnterKeys(app, nil) })
		}
		c.petStartAnim()
		proxy := &ptStdoutProxy{app: func() *ptApplication { return app }, out: tuiStdout()}
		restoreStdout := agent.PatchStatusStdout(proxy)
		defer func() {
			proxy.Flush()
			restoreStdout()
		}()
		err := app.run()
		switch {
		case err == nil, c.tuiSuppressClosedLoopErrors(err), errors.Is(err, syscall.EPIPE):
		case errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.EBADF), containsAny(err.Error(), "is not registered", "Bad file descriptor", "Invalid argument"):
			fmt.Fprintln(console().Out, c.t("cli.startup.stdin_unusable", map[string]any{"error": err.Error()}))
		default:
			panic(err)
		}
	}()
	if len(c.pendingRelaunch) > 0 {
		relaunch(c.pendingRelaunch, false)
	}
}

// Reports whether the text holds any of the fragments
func containsAny(text string, fragments ...string) bool {
	for _, f := range fragments {
		if strings.Contains(text, f) {
			return true
		}
	}
	return false
}
