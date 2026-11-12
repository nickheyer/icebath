package app

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Chat turn a submitted input runs, the method itself in production
var tuiChat = (*CLI).chat

// Worker draining pending input, running idle housekeeping between inputs and dispatching each one
func (c *CLI) tuiProcessLoop() {
	c.tuiProcessLoopWith(c.tuiProcessOneInput, c.tuiIdleTick)
}

// Input loop over the given dispatch and idle steps, the instance attributes the Python loop calls through
func (c *CLI) tuiProcessLoopWith(processOne func(any), idleTick func()) {
	var held *ownQueueHold
	for !c.exitRequested() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					if err, ok := r.(error); ok && isEIO(err) {
						c.markTerminalIOBroken("process_loop")
						cliLogger.Warn(fmt.Sprintf("process_loop EIO — freezing UI paints (#81521): %v", err))
						return
					}
					cliLogger.Warn(fmt.Sprintf("process_loop unhandled error (msg may be lost): %v", r))
				}
			}()
			if held != nil {
				var userInput any
				held, userInput = c.tuiHoldStep(held)
				if userInput != nil {
					processOne(userInput)
				}
				return
			}
			userInput, ok := c.pendingInput.Get(100 * time.Millisecond)
			if !ok {
				c.uiMu.Lock()
				running := c.agentRunning
				c.uiMu.Unlock()
				if !running {
					idleTick()
				}
				return
			}
			held = beginOwnQueueHold(c, userInput)
			if held == nil {
				processOne(userInput)
			}
		}()
	}
}

// One step of holding a local input behind this viewer's own queued admission, the owner's running turn first
func (c *CLI) tuiHoldStep(hold *ownQueueHold) (*ownQueueHold, any) {
	quietly(func() { c.drainProcessNotifications("cli-idle") })
	if item := hold.takeGatewayTurn(c.pendingInput); item != nil {
		return hold, item
	}
	if hold.done() {
		return nil, hold.item
	}
	time.Sleep(50 * time.Millisecond)
	return hold, nil
}

// Runs one idle step, a failure never leaving the loop
func quietly(step func()) {
	defer func() { _ = recover() }()
	step()
}

// Idle housekeeping between inputs while no turn runs
func (c *CLI) tuiIdleTick() {
	c.checkConfigMCPChanges()
	for _, step := range []func(){c.checkTermiosDrift, func() { c.drainProcessNotifications("cli-idle") }, c.maybeFireLoopTick, c.maybeResumeParkedGoal} {
		quietly(step)
	}
}

// Refreshes the live work dock once an input left the pending queue
func (c *CLI) tuiDockDropDequeued() {
	c.uiMu.Lock()
	monitor := c.subagentMonitor
	c.uiMu.Unlock()
	if monitor != nil {
		quietly(monitor.refresh)
	}
}

// Routes one submitted input, a file drop, a resume pick, a shell line, a slash command or a chat turn
func (c *CLI) tuiProcessOneInput(queued any) {
	c.tuiDockDropDequeued()
	userInput, isVoice, isSeeded := c.tuiUnwrapInput(queued)
	if userInput == nil || userInput == "" {
		return
	}
	notification, isNotification := userInput.(tools.TimelineNotification)
	c.statusBarSuppressedAfterResize = false
	var submitImages []string
	if in, ok := userInput.(submittedInput); ok {
		userInput, submitImages = in.Text, append([]string{}, in.Images...)
	}
	text, isText := userInput.(string)
	if isText {
		var hadMouse bool
		text, hadMouse = stripLeakedTerminalResponsesWithMeta(tools.StripLeakedBracketedPasteWrappers(text))
		if hadMouse {
			c.recoverTerminalInputModes("mouse reports leaked into submitted input")
		}
		userInput = text
	}
	if !isVoice && c.typedVoiceStop(userInput) {
		return
	}
	var drop *fileDrop
	if isText && !isSeeded {
		drop = detectFileDrop(text)
	}
	switch {
	case drop != nil:
		name := filepath.Base(drop.Path)
		if drop.IsImage {
			submitImages = append(submitImages, drop.Path)
			text = drop.Remainder
			if text == "" {
				text = "[User attached image: " + name + "]"
			}
			cprint("  " + c.t("cli.tui.auto_attached_image", map[string]any{"name": name}))
		} else {
			cprint("  " + c.t("cli.tui.detected_file", map[string]any{"name": name}))
			text = "[User attached file: " + drop.Path + "]"
			if drop.Remainder != "" {
				text += "\n" + drop.Remainder
			}
		}
		userInput = text
	case isText:
		if len(c.pendingResumeSessions) > 0 && c.consumePendingResumeSelection(text) {
			return
		}
		if !isSeeded {
			if c.handleBangShell(text) {
				return
			}
			if commands.LooksLikeSlashCommand(text) {
				seed, run := c.tuiRunSlashInput(text)
				if !run {
					return
				}
				userInput, text = seed, cliStr(seed, "")
				_, isText = seed.(string)
			}
		}
	}
	if isText && pasteRefRe.MatchString(text) {
		expanded, err := c.expandPasteReferences(text)
		if err != nil {
			panic(err)
		}
		text, userInput = expanded, expanded
	}
	cprint("")
	var message any = userInput
	if isNotification {
		message = notification
	}
	c.printUserMessagePreview(message)
	if n := len(submitImages); n > 0 {
		key := "cli.tui.images_attached_other"
		if n == 1 {
			key = "cli.tui.images_attached_one"
		}
		cprint("  " + dimANSI + c.t(key, map[string]any{"count": n}) + resetANSI)
	}
	c.uiMu.Lock()
	c.agentRunning, c.interactiveTurn = true, true
	c.petTurnError, c.petReasoning = false, false
	app := c.app
	c.uiMu.Unlock()
	c.turnSummaryBegin()
	if app != nil {
		_ = app.Invalidate()
	}
	defer c.tuiAfterTurn()
	tuiChat(c, message, submitImages, isVoice)
}

// Dispatches a slash command, returning the agent seed it left to run as a chat turn
func (c *CLI) tuiRunSlashInput(userInput string) (seed any, run bool) {
	cprint("\n⚙️  " + userInput)
	interrupted := func() (interrupted bool) {
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); ok && errors.Is(err, errKeyboardInterrupt) {
					interrupted = true
					return
				}
				panic(r)
			}
		}()
		if !c.processCommand(userInput) {
			c.setShouldExit(false)
			if c.app != nil && c.app.IsRunning() {
				c.app.Exit()
			}
		}
		return false
	}()
	if interrupted {
		chatConsolePrint("\n[dim]" + c.t("cli.tui.command_interrupted", nil) + "[/dim]")
		return nil, false
	}
	seed, c.pendingAgentSeed = c.pendingAgentSeed, nil
	if seed == nil || seed == "" {
		return nil, false
	}
	return seed, true
}

// errKeyboardInterrupt is the Ctrl C of a slow slash command
var errKeyboardInterrupt = errors.New("KeyboardInterrupt")

// Bookkeeping after a chat turn returned, however it ended
func (c *CLI) tuiAfterTurn() {
	c.uiMu.Lock()
	c.agentRunning, c.petReasoning = false, false
	c.spinnerText, c.lastScrollbackTool = "", ""
	c.toolStartTime = 0
	c.pendingToolInfo = nil
	c.runningToolLabels = nil
	app, interrupted := c.app, c.lastTurnInterrupted
	c.uiMu.Unlock()
	c.petReactTurnEnd()
	c.turnSummaryEmit()
	c.uiMu.Lock()
	c.interactiveTurn = false
	c.uiMu.Unlock()
	if app != nil {
		_ = app.Invalidate()
	}
	if interrupted {
		c.recoverTerminalAfterInterrupt()
	}
	c.drainInterruptQueueToPendingInput()
	for _, step := range []struct {
		hook func()
		what string
	}{{c.maybeContinueGoalAfterTurn, "goal continuation"}, {c.maybeCompleteLoopTickAfterTurn, "loop completion"}} {
		hook, what := step.hook, step.what
		func() {
			defer func() {
				if r := recover(); r != nil {
					cliLogger.Debug(fmt.Sprintf("%s hook failed: %v", what, r))
				}
			}()
			hook()
		}()
	}
	c.voiceLock.Lock()
	restart := c.voiceMode && c.voiceContinuous && !c.voiceRecording
	tts, done, barge := c.voiceTTS, c.voiceTTSDone, c.voiceBargeCapture
	c.voiceLock.Unlock()
	if restart {
		go func() {
			if tts && done != nil {
				done.Wait(60 * time.Second)
				sleepSeconds(0.3)
			}
			if barge != nil && barge.IsSet() {
				return
			}
			if err := c.voiceStartRecording(); err != nil {
				cprint(dimANSI + c.t("cli.voice.autorestart_failed", map[string]any{"error": err}) + resetANSI)
				return
			}
			if app != nil {
				_ = app.Invalidate()
			}
		}()
	}
	quietly(func() { c.drainProcessNotifications("cli-post-turn") })
}

// Handles SIGHUP and SIGTERM by interrupting the agent and exiting the prompt loop cleanly
func (c *CLI) tuiSignalHandler(sig os.Signal) {
	cliLogger.Debug(fmt.Sprintf("Received signal %v, triggering graceful shutdown", sig))
	armExitWatchdogOnShutdownSignal(c.ctx)
	c.uiMu.Lock()
	running, app := c.agentRunning, c.app
	c.uiMu.Unlock()
	if running {
		interruptAgentForSignal(c.agent, sig)
	}
	if app != nil && app.IsRunning() {
		app.Exit()
	}
}

// Prints the startup output, the banner, advisories, resumed history, the welcome line and tips
func (c *CLI) tuiPrintStartup() {
	detectLightMode()
	if _, rows := pyshutil.GetTerminalSize(80, 24); rows > 2 {
		fmt.Fprint(console().Out, strings.Repeat("\n", rows-1))
	}
	if err := c.ensureGatewaySession(); err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		fmt.Fprintln(stderrWriter(), "Error: "+err.Error())
		panic(singleQueryExit{code: 2})
	}
	c.showBanner()
	c.showSecurityAdvisories()
	func() {
		defer func() {
			if r := recover(); r != nil {
				cliLogger.Debug(fmt.Sprintf("first-run setup offer failed: %v", r))
			}
		}()
		c.maybeOfferFirstRunSetup()
	}()
	if console().StdinIsTTY() {
		quietly(func() { offerSandboxImageSwitch(c, cprint) })
		quietly(func() { offerAutoPrune(c, cprint) })
	}
	if c.resumed && c.preloadResumedSession() {
		c.displayResumedHistory()
	}
	skin := config.GetActiveSkin(c.ctx)
	welcome := skin.GetBranding("welcome", c.t("cli.tui.welcome", nil))
	c.consolePrint("[" + skin.GetColor("banner_text", "#FFF8DC") + "]" + welcome + "[/]")
	c.tuiStartupPrewarmAndWarnings(skin)
	c.printRandomTip()
	c.tuiStartupBackgroundMaintenance()
	skillsForLine := c.preloadedSkills
	if len(skillsForLine) == 0 {
		skillsForLine = c.preloadSkillsRequested
	}
	if len(skillsForLine) > 0 && !c.startupSkillsLineShown {
		c.consolePrint("[bold " + accentHex() + "]" + c.t("cli.tui.activated_skills_label", nil) + "[/] " + strings.Join(skillsForLine, ", "))
		c.startupSkillsLineShown = true
	}
	c.consolePrint("")
}

// Warms the model picker cache and prints the redaction off and leftover OpenClaw banners
func (c *CLI) tuiStartupPrewarmAndWarnings(skin *config.SkinConfig) {
	provider.PrewarmPickerCacheAsync(c.ctx)
	if raw := envOr("ICEBATH_REDACT_SECRETS", "true"); !isTruthyWord(raw) {
		c.consolePrint("[bold red]" + c.t("cli.tui.redaction_disabled_title", nil) + "[/] " +
			c.t("cli.tui.redaction_disabled_body", map[string]any{"value": pyrich.Escape(raw), "setting": "[cyan]security.redact_secrets: true[/]"}))
	}
	if !tools.IsSeen(c.config, tools.OnboardingOpenclawResidueFlag) && tools.DetectOpenclawResidue("") {
		c.consolePrint("[" + skin.GetColor("banner_dim", "#B8860B") + "]" + tools.OpenclawResidueHintCLI() + "[/]")
		tools.MarkSeen(c.ctx, home.ConfigPath(home.Dir(c.ctx)), tools.OnboardingOpenclawResidueFlag)
	}
}

// Environment value, the fallback when unset
func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

// Reports whether a word reads as on
func isTruthyWord(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Starts the startup maintenance pass off the main goroutine
func (c *CLI) tuiStartupBackgroundMaintenance() { go c.runStartupMaintenance() }

// Curator pass of the startup maintenance, swapped by tests
var maybeRunCurator = func(c *CLI, idleForSeconds *float64, onSummary func(string)) {
	memory.MaybeRunCurator(c.ctx, idleForSeconds, onSummary)
}

// Runs the curator pass due at startup, its summary printed dim
func (c *CLI) runStartupMaintenance() {
	quietly(func() {
		idle := math.Inf(1)
		maybeRunCurator(c, &idle, func(msg string) { c.consolePrint("[dim #6b7684]💾 " + msg + "[/]") })
	})
}

// Builds the prompt application, emacs editing by default and vi under display.vim_mode
func (c *CLI) tuiBuildApplication(app *ptApplication, layout []*ptWindow, kb *ptKeyBindings, style *styleDict) error {
	if out := selectClassicCLIPTOutput(os.Stdout); out != nil {
		app.enableCPR, app.fallbackSize = out.enableCPR, out.size
	} else {
		app.enableCPR = true
	}
	app.layout = layout
	app.keyBindings = kb
	if c.vimMode {
		ptLoadViBindings(app)
	}
	app.disableKitty = isGhosttyTerminal(nil)
	app.refreshInterval = pyFloat(mapValueOr(cliMap(CLIConfig(c.ctx), "display"), "cli_refresh_interval", 0))
	return app.setStyle(style)
}

// Routes SIGTERM and SIGHUP to a graceful shutdown, returning the function that stops listening
func (c *CLI) tuiInstallSignalHandlers() func() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, shutdownSignals()...)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-ch:
				c.tuiSignalHandler(sig)
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// Reports whether standard input can be read, printing why when it cannot
func (c *CLI) tuiStdinUsable() bool {
	if _, err := os.Stdin.Stat(); err != nil {
		fmt.Fprintln(console().Out, c.t("cli.tui.stdin_unavailable", nil))
		return false
	}
	return true
}

// The session quitting closes, the agent's when one exists
func (c *CLI) closingSessionID() string {
	if c.agent != nil {
		return c.agent.SessionID()
	}
	if c.gatewaySessionOpen {
		return c.sessionID
	}
	return ""
}

// Teardown after the prompt exits, interrupting the agent, stopping voice, closing the session, cleanup and the exit summary
func (c *CLI) tuiShutdown() {
	c.setShouldExit(false)
	c.petStopAnim()
	fmt.Fprintln(console().Out, dimANSI+c.t("cli.tui.shutting_down", nil)+resetANSI)
	c.uiMu.Lock()
	running := c.agentRunning
	c.uiMu.Unlock()
	if c.agent != nil && running && !c.detachOnExit {
		quietly(func() { requestHardInterrupt(c.agent) })
	}
	if rec := c.voiceRecorderHandle(); rec != nil {
		quietly(rec.Shutdown)
		c.voiceRecorder = nil
	}
	quietly(func() { media.CleanupTempRecordings(3600) })
	c.env.Ask = nil
	skills.SetSecretCaptureCallback(nil)
	browser.SetUnlockPromptCallback(c.ctx, nil)
	browser.SetSaveLoginPromptCallback(c.ctx, nil)
	browser.SetCodePromptCallback(c.ctx, nil)
	lockVaultSessions(c)
	c.persistActiveSessionBeforeClose()
	if sid := c.closingSessionID(); c.sessionDB != nil && sid != "" {
		if err := c.sessionDB.EndSession(c.ctx, sid, "cli_close"); err != nil {
			cliLogger.Debug(fmt.Sprintf("Could not close session in DB: %v", err))
		}
		if !c.deleteSessionOnExit {
			func() {
				defer func() {
					if r := recover(); r != nil {
						cliLogger.Debug(fmt.Sprintf("Could not prune empty session: %v", r))
					}
				}()
				c.discardSessionIfEmpty(sid)
			}()
		} else {
			deleted, err := c.sessionDB.DeleteSession(c.ctx, sid)
			switch {
			case err != nil:
				cliLogger.Debug(fmt.Sprintf("Could not delete session on exit: %v", err))
			case deleted:
				cprint("  " + dimANSI + c.t("cli.tui.session_deleted", map[string]any{"session_id": pyrich.Escape(sid)}) + resetANSI)
			default:
				cprint("  " + dimANSI + c.t("cli.tui.session_delete_missing", map[string]any{"session_id": pyrich.Escape(sid)}) + resetANSI)
			}
		}
	}
	signalWatchdogMu.Lock()
	signalled := signalWatchdogArmed
	signalWatchdogMu.Unlock()
	activeWorktreeMu.Lock()
	worktree := activeWorktree
	activeWorktreeMu.Unlock()
	if !signalled && len(worktree) > 0 {
		info := make(map[string]any, len(worktree))
		for k, v := range worktree {
			info[k] = v
		}
		quietly(func() {
			tools.AskExitChoice(c.ctx, info)
			tools.ApplyExitChoice(c.ctx, info, cprint)
		})
	}
	runCleanup(c.ctx, c.env, true)
	c.printExitSummary(false)
	c.releaseActiveSession()
}

// Signals that end the prompt gracefully
func shutdownSignals() []os.Signal { return []os.Signal{syscall.SIGTERM, syscall.SIGHUP} }
