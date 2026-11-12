package app

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Interrupts the gateway turn for a shutdown signal, then sleeps the ICEBATH_SIGTERM_GRACE window
func interruptAgentForSignal(a cliAgent, sig os.Signal) {
	if a == nil {
		return
	}
	cliLogger.Debug(fmt.Sprintf("interrupting the gateway turn: received signal %s", signalNumber(sig)))
	a.Interrupt()
	if grace := floatEnv("ICEBATH_SIGTERM_GRACE", 1.5); grace > 0 {
		time.Sleep(time.Duration(grace * float64(time.Second)))
	}
}

// Returns the numeric form of a signal the way Python's signum prints
func signalNumber(sig os.Signal) string {
	if s, ok := sig.(syscall.Signal); ok {
		return strconv.Itoa(int(s))
	}
	return sig.String()
}

// Drives a kanban goal mode worker through the goal loop after its first turn
func runKanbanGoalLoopQ(c *CLI, firstResponse string, runTurn func(string) (any, error), log func(string)) error {
	taskID := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_TASK"))
	if taskID == "" {
		return nil
	}
	rawRunID := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_RUN_ID"))
	var workerRunID *int64
	if rawRunID != "" {
		if n, ok := pyIntOf(rawRunID); ok {
			id := int64(n)
			workerRunID = &id
		} else {
			cliLogger.Warn(fmt.Sprintf("invalid ICEBATH_KANBAN_RUN_ID=%s", pyrepr.Repr(rawRunID)))
		}
	}
	var task *kanban.Task
	err := kanban.ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		t, gerr := kanban.GetTask(c.ctx, conn, taskID)
		task = t
		return gerr
	})
	if err != nil {
		return err
	}
	if task == nil {
		return nil
	}
	var parts []string
	if task.Title != "" {
		parts = append(parts, task.Title)
	}
	if task.Body != nil && *task.Body != "" {
		parts = append(parts, *task.Body)
	}
	goalText := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if goalText == "" {
		return nil
	}
	quietTurn := func(prompt string) (any, error) {
		result, rerr := c.agent.RunConversation(c.turnCtx, prompt, agent.RunConversationOptions{ConversationHistory: c.conversationHistory})
		if rerr != nil {
			return nil, rerr
		}
		syncCLISessionIDFromAgent(c)
		resp := cliStr(result["final_response"], "")
		if resp != "" {
			fmt.Fprintln(c.console.Out, resp)
		}
		return map[string]any{"response": resp, "failed": cliTruthy(result["failed"]), "failure_reason": result["failure_reason"]}, nil
	}
	taskStatus := func() (*string, error) {
		var status *string
		err := kanban.ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
			s, serr := kanban.GoalRunStatus(c.ctx, conn, taskID, workerRunID)
			status = s
			return serr
		})
		return status, err
	}
	block := func(reason string) error {
		return kanban.ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
			return kanban.BlockTask(c.ctx, conn, taskID, reason, workerRunID)
		})
	}
	if runTurn == nil {
		runTurn = quietTurn
	}
	if log == nil {
		log = func(m string) { cliLogger.Info(m) }
	}
	maxTurns := goals.DefaultGoalMaxTurns
	if goalMax := kanban.TaskGoalMaxTurns(task); goalMax != nil && *goalMax > 0 {
		maxTurns = int(*goalMax)
	}
	_, err = goals.RunKanbanGoalLoop(c.ctx, goals.GoalLoopOptions{TaskID: taskID, GoalText: goalText, RunTurn: runTurn,
		TaskStatus: taskStatus, Block: block, MaxTurns: maxTurns, FirstResponse: firstResponse, Log: log})
	return err
}

// Runs the -q worker goal loop with follow up turns through chat so the worker log keeps its tool feed
func runKanbanGoalLoopChat(c *CLI, firstResponse string) error {
	log := func(msg string) {
		cliLogger.Info(msg)
		fmt.Fprintln(c.console.Out, msg)
	}
	return runKanbanGoalLoopQ(c, firstResponse, func(p string) (any, error) {
		if response, ok := c.chat(p, nil, false); ok && response != "" {
			return response, nil
		}
		return "", nil
	}, log)
}

// Keeps the CLI session id in sync when mid run compression rotated the agent's session
func syncCLISessionIDFromAgent(c *CLI) {
	if c.agent != nil && c.agent.SessionID() != "" && c.agent.SessionID() != c.sessionID {
		c.sessionID = c.agent.SessionID()
	}
}

// Maps a one shot turn result onto the process exit code for both -q and -Q
func singleQueryExitCode(result map[string]any, credentialsRateLimited, credentialsTerminal bool) int {
	return TurnExitCode(result, TurnExitOptions{KanbanWorker: kanban.KanbanTaskID() != "",
		CredentialsRateLimited: credentialsRateLimited, CredentialsTerminal: credentialsTerminal,
		TransientReasons: TransientProviderReasons, TerminalReasons: TerminalProviderReasons})
}

// Pops ICEBATH_TURN_AUTHOR from the process environment so tool subprocesses never inherit it
func takeTurnAuthorFromProcessEnv() map[string]any {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	author := provider.TakeTurnAuthorFromEnv(env)
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			if _, kept := env[k]; !kept {
				_ = os.Unsetenv(k)
			}
		}
	}
	if author == nil {
		return nil
	}
	m, _ := pyjson.Plain(author).(map[string]any)
	return m
}

// Runs the quiet one shot turn, prints the response and exits with the automation exit code
func runQuietSingleQuery(c *CLI, effectiveQuery any, emitter *StreamJSONEmitter) {
	author := takeTurnAuthorFromProcessEnv()
	turnReportPath := takeTurnReportPath()
	sessionKey := c.sessionID
	if sessionKey == "" {
		sessionKey = "default"
	}
	var result map[string]any
	var response string
	func() {
		defer bindQuietSessionKey(c.env, sessionKey)()
		var err error
		result, err = c.agent.RunConversation(c.turnCtx, effectiveQuery, agent.RunConversationOptions{ConversationHistory: c.conversationHistory, TurnAuthor: author})
		if tools.IsKeyboardInterrupt(c.turnCtx, err) {
			emitInterruptedSessionEnd(c, "keyboard_interrupt")
			if emitter != nil {
				exitSingleQuery(emitter.EmitResult(map[string]any{"failed": true, "error": "Interrupted"}, c.sessionID, 130))
			}
			fmt.Fprintf(stderrWriter(), "\nsession_id: %s\n", c.sessionID)
			exitSingleQuery(130)
		}
		if err != nil {
			fmt.Fprintln(stderrWriter(), err.Error())
			exitSingleQuery(1)
		}
		syncCLISessionIDFromAgent(c)
		quietRecoverTurn := func(nudge string) {
			history := historyOf(result)
			if history == nil {
				history = c.conversationHistory
			}
			recovered, rerr := c.agent.RunConversation(c.turnCtx, nudge, agent.RunConversationOptions{ConversationHistory: history, TurnAuthor: author})
			if tools.IsKeyboardInterrupt(c.turnCtx, rerr) {
				panic(keyboardInterrupt{})
			}
			if rerr == nil {
				result = recovered
			}
			syncCLISessionIDFromAgent(c)
		}
		if _, rerr := kanban.RecoverFailedKanbanTurns(c.ctx, quietRecoverTurn, func() map[string]any { return result },
			func(msg string) { fmt.Fprintln(stderrWriter(), msg) }); rerr != nil {
			fmt.Fprintln(stderrWriter(), rerr.Error())
			exitSingleQuery(1)
		}
		reportTurn := func(res map[string]any) {
			errText := "agent turn did not run"
			reply := ""
			if res != nil {
				errText = cliStr(res["error"], "")
				reply = cliStr(res["final_response"], "")
			}
			writeTurnReport(c.ctx, turnReportPath, singleQueryExitCode(res, false, false), errText, reply)
		}
		reportTurn(result)
		if result != nil && !cliTruthy(result["failed"]) {
			history := historyOf(result)
			if history == nil {
				history = c.conversationHistory
			}
			followUp := func(text string) map[string]any {
				follow, ferr := c.agent.RunConversation(c.turnCtx, text, agent.RunConversationOptions{ConversationHistory: history, TurnAuthor: author})
				if tools.IsKeyboardInterrupt(c.turnCtx, ferr) {
					panic(keyboardInterrupt{})
				}
				if ferr != nil {
					follow = map[string]any{"failed": true, "error": ferr.Error()}
				}
				if h := historyOf(follow); h != nil {
					history = h
				}
				syncCLISessionIDFromAgent(c)
				return follow
			}
			var continued map[string]any
			func() {
				defer func() { c.quietNotifyLingerDone = true }()
				budget := quietNotifyLingerSeconds(c.env)
				continued = continueQuietNotifyCompletions(c.ctx, c.env, c.sessionID, followUp, c.ownsProcessNotification, maxQuietNotifyRounds, &budget)
			}()
			if continued != nil {
				result = continued
				reportTurn(result)
			}
		}
		response = cliStr(result["final_response"], "")
	}()
	if emitter == nil {
		if response == "" && result != nil && cliTruthy(result["error"]) && (cliTruthy(result["failed"]) || cliTruthy(result["partial"])) {
			fmt.Fprintln(stderrWriter(), i18n.T(c.ctx, "gateway.model.error_prefix", "", map[string]any{"error": result["error"]}))
		} else if response != "" {
			fmt.Fprintln(c.console.Out, response)
		}
	}
	exitCode := singleQueryExitCode(result, false, false)
	if os.Getenv("ICEBATH_KANBAN_GOAL_MODE") == "1" && exitCode == 0 {
		if err := runKanbanGoalLoopQ(c, response, nil, nil); err != nil {
			cliLogger.Debug(fmt.Sprintf("kanban goal loop failed: %v", err))
		}
	}
	if emitter == nil {
		fmt.Fprintf(stderrWriter(), "\nsession_id: %s\n", c.sessionID)
	} else {
		exitCode = emitter.EmitResult(result, c.sessionID, exitCode)
	}
	exitSingleQuery(exitCode)
}

// Returns the messages a turn result carries as the next turn's history, nil when it has none
func historyOf(result map[string]any) []map[string]any {
	if result == nil {
		return nil
	}
	switch msgs := result["messages"].(type) {
	case []map[string]any:
		if len(msgs) > 0 {
			return msgs
		}
	case []*state.Message:
		if len(msgs) > 0 {
			return agent.SessionMessageDicts(msgs)
		}
	case []any:
		out := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				out = append(out, mm)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// Attaches one shot images natively when the model supports vision, else pre describes them as text
func routeSingleQueryImages(c *CLI, query string, effectiveQuery any, images, imageURLs []string) any {
	if len(images) == 0 && len(imageURLs) == 0 {
		return effectiveQuery
	}
	imgMode := tools.DecideImageInputMode(c.ctx, c.env, strings.TrimSpace(c.provider), strings.TrimSpace(c.model), strings.TrimSpace(c.requestedProvider))
	textFallback := func() any {
		if len(images) > 0 {
			return c.preprocessImagesWithVision(query, images, false)
		}
		return effectiveQuery
	}
	if imgMode != "native" {
		return textFallback()
	}
	parts, _, err := tools.BuildNativeContentParts(c.env, query, images, imageURLs)
	if err != nil {
		return textFallback()
	}
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok && m["type"] == "image_url" {
			return parts
		}
	}
	return textFallback()
}

// Adds the image paths and URLs of a Kanban worker's task body to the first turn's attachments
func collectKanbanTaskImages(c *CLI, images *[]string) []string {
	imageURLs := []string{}
	taskID := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_TASK"))
	if taskID == "" {
		return imageURLs
	}
	var task *kanban.Task
	err := kanban.ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		t, gerr := kanban.GetTask(c.ctx, conn, taskID)
		task = t
		return gerr
	})
	if err != nil {
		cliLogger.Debug(fmt.Sprintf("kanban image-ref extraction failed: %v", err))
		return imageURLs
	}
	if task == nil || task.Body == nil || *task.Body == "" {
		return imageURLs
	}
	paths, urls := tools.ExtractImageRefs(*task.Body)
	seen := map[string]bool{}
	for _, p := range *images {
		seen[p] = true
	}
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			*images = append(*images, p)
		}
	}
	return append(imageURLs, urls...)
}

// Kills the worker's foreground process group and exits without cleanup
func killForegroundAndExit() {
	tools.KillLiveForegroundProcesses(true)
	osExit(0)
}

// Routes SIGINT, SIGTERM and SIGHUP through the agent interrupt before unwinding, kanban workers hard exit
func installSingleQuerySignalHandlers(c *CLI) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range ch {
			cliLogger.Debug(fmt.Sprintf("Received signal %s in single-query mode", signalNumber(sig)))
			armExitWatchdogOnShutdownSignal(c.ctx)
			interruptAgentForSignal(c.agent, sig)
			if os.Getenv("ICEBATH_KANBAN_TASK") != "" {
				time.AfterFunc(5*time.Second, killForegroundAndExit)
				func() {
					defer func() { _ = recover() }()
					flushOneShotSessionStore(c)
				}()
				observability.FlushBeforeHardExit(c.ctx, 3*time.Second)
				flushLoggingAndStdio()
				killForegroundAndExit()
			}
			c.turnCancel()
		}
	}()
	c.signalStop = func() { signal.Stop(ch); close(ch) }
}

// Neutralizes every stdout writing callback so -Q stdout carries only the final response
func configureQuietAgent(a *GatewayAgent) {
	a.SetQuietMode(true)
	a.SetSuppressStatusOutput(true)
	a.silencePresentation()
	a.SetToolProgressMode("off")
}

// Seeds an interactive session on a TTY, else runs the one shot turn and exits with its code
func runSingleQueryMode(c *CLI, query, image string, quiet, oneshot, streamJSON bool) {
	if shouldSeedInteractive(query, image, quiet, oneshot) {
		seededQuery, seededImages, err := collectQueryImages(query, image)
		if err != nil {
			fmt.Fprintln(stderrWriter(), err.Error())
			exitSingleQuery(1)
		}
		cliLogger.Info(fmt.Sprintf("Seeding interactive session with -q prompt (%d chars, %d images)", len(seededQuery), len(seededImages)))
		c.seededFirstMessage = newSeededQueryMessage(seededQuery, seededImages, "")
		c.run()
		return
	}
	c.singleQueryMode = true
	_ = os.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" && !kanban.RegisterCurrentWorkerFromEnv(c.ctx) {
		fmt.Fprintln(stderrWriter(), i18n.T(c.ctx, "cli.single_query.kanban_run_reclaimed", "", nil))
		panic(singleQueryExit{code: 0})
	}
	if !c.claimActiveSession("cli", quiet) {
		exitSingleQuery(1)
	}
	defer finalizeSingleQuery(c)
	query, images, err := collectQueryImages(query, image)
	if err != nil {
		fmt.Fprintln(stderrWriter(), err.Error())
		exitSingleQuery(1)
	}
	imageURLs := collectKanbanTaskImages(c, &images)
	observability.RecordCliOneShotReady(c.ctx)
	if quiet {
		c.toolProgressMode = "off"
		var emitter *StreamJSONEmitter
		if streamJSON {
			emitter = NewStreamJSONEmitter(c.model, c.sessionID)
		}
		if c.ensureRuntimeCredentials() {
			effectiveQuery := routeSingleQueryImages(c, query, query, images, imageURLs)
			turnRoute := c.resolveTurnAgentConfig(effectiveQuery)
			if turnRoute.signature != c.activeAgentRouteSignature {
				c.agent = nil
			}
			if c.initAgent(turnRoute.model, turnRoute.runtime, turnRoute.requestOverrides) {
				configureQuietAgent(agentGateway(c.agent))
				if emitter != nil {
					emitter.Attach(c.agent)
				}
				runQuietSingleQuery(c, effectiveQuery, emitter)
			}
		}
		failCode := singleQueryExitCode(nil, c.credentialsRateLimited, c.credentialsTerminal)
		if emitter != nil {
			emitter.EmitResult(map[string]any{"failed": true, "error": "credentials or agent init failed"}, c.sessionID, failCode)
		}
		exitSingleQuery(failCode)
	}
	queryLabel := query
	if queryLabel == "" && len(images) > 0 {
		queryLabel = "[image attached]"
	}
	if queryLabel != "" {
		fmt.Fprintln(c.console.Out, pyrich.Render(fmt.Sprintf("[bold blue]%s[/] %s", i18n.T(c.ctx, "cli.single_query.query_label", "", nil), queryLabel), c.console.ShouldUseColor()))
	}
	c.showSecurityAdvisories()
	c.singleQueryTurnAuthor = takeTurnAuthorFromProcessEnv()
	response, _ := c.chat(query, images, false)
	if _, rerr := kanban.RecoverFailedKanbanTurns(c.ctx, func(nudge string) { response, _ = c.chat(nudge, nil, false) },
		func() map[string]any { return c.lastTurnResult }, func(msg string) { fmt.Fprintln(stderrWriter(), msg) }); rerr != nil {
		fmt.Fprintln(stderrWriter(), rerr.Error())
		exitSingleQuery(1)
	}
	if os.Getenv("ICEBATH_KANBAN_GOAL_MODE") == "1" && singleQueryExitCode(c.lastTurnResult, false, false) == 0 {
		if err := runKanbanGoalLoopChat(c, response); err != nil {
			cliLogger.Debug(fmt.Sprintf("kanban goal loop failed: %v", err))
		}
	}
	c.printExitSummary(false)
	exitSingleQuery(singleQueryExitCode(c.lastTurnResult, c.credentialsRateLimited, c.credentialsTerminal))
}
