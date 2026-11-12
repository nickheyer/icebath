package app

import (
	"context"
	"encoding/hex"
	"fmt"
	"iter"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Rich console a side result prints through, the chat console under the TUI or the CLI's own otherwise
type sideConsole interface {
	Print(markup ...string)
	PrintPanel(p pyrich.Panel)
}

// Context of a freshly started worker thread with empty context vars and goroutine local storage
func (c *CLI) freshThreadContext() context.Context {
	return pyctx.With(tools.WithThreadLocals(context.WithoutCancel(c.ctx)), pyctx.New())
}

// Invalidates the running TUI with a brief pause for the redraw, then prints a blank separator
func (c *CLI) refreshTUIBeforePrint() {
	if c.app != nil {
		_ = c.app.Invalidate()
		time.Sleep(50 * time.Millisecond)
	}
	printLines("")
}

// Prints a worker result between accent rules and its body in a skinned panel, or the empty note
func (c *CLI) printSideResultPanel(headerLines []string, body, titleSuffix, emptyNote string, console sideConsole) {
	c.refreshTUIBeforePrint()
	var richConsole sideConsole = console
	if console == nil {
		richConsole = newChatConsole()
	}
	rule := "[" + accentHex() + "]" + strings.Repeat("─", 40) + "[/]"
	richConsole.Print(rule)
	if console == nil {
		cp(headerLines...)
	} else {
		for _, line := range headerLines {
			console.Print(line)
		}
	}
	richConsole.Print(rule)
	if body == "" {
		if console == nil {
			cp(emptyNote)
		} else {
			console.Print(emptyNote)
		}
		return
	}
	label, respColor, respText := "☤ Icebath", "#CD7F32", "#FFF8DC"
	if skin := config.GetActiveSkin(c.ctx); skin != nil {
		label = skin.GetBranding("response_label", "☤ Icebath")
		respColor = maybeRemapForLightMode(skin.GetColor("response_border", "#CD7F32"))
		respText = maybeRemapForLightMode(skin.GetColor("banner_text", "#FFF8DC"))
	}
	richConsole.PrintPanel(pyrich.Panel{
		Content: renderFinalAssistantContent(body, c.finalResponseMarkdown), Title: "[" + respColor + " bold]" + label + " " + titleSuffix + "[/]",
		TitleAlign: "left", BorderStyle: respColor, Style: respText, Box: "horizontals", Padding: &[2]int{1, 4}, Width: scrollbackBoxWidth()})
}

// Shape of one side result worker for /bg, /btw and /login
type sideWorkerSpec struct {
	name, failLabel, titleSuffix, emptyNote string
	headerLines                             []string
	bell                                    bool
	onDone                                  func()
	console                                 sideConsole
}

// Worker goroutine that prints the produced body in a side result panel, or the failure line, then refreshes the TUI
func (c *CLI) sideWorker(produce func() (string, error), spec sideWorkerSpec) func() {
	return func() {
		fail := func(err any) {
			c.refreshTUIBeforePrint()
			line := "  " + c.commandsT("side_result.failed", map[string]any{"label": spec.failLabel, "error": err})
			if spec.console != nil {
				spec.console.Print(pyrich.Escape(line))
			} else {
				cp(line)
			}
		}
		defer func() {
			if spec.onDone != nil {
				spec.onDone()
			}
			if c.app != nil {
				_ = c.invalidateWithin(0)
			}
		}()
		defer func() {
			if r := recover(); r != nil {
				fail(sideFailureText(r))
			}
		}()
		body, err := produce()
		if err != nil {
			fail(sideFailureText(err))
			return
		}
		c.printSideResultPanel(spec.headerLines, body, spec.titleSuffix, spec.emptyNote, spec.console)
		if spec.bell {
			c.ringBell(false, c.commandsT("side_result.complete", map[string]any{"label": spec.failLabel}), "")
		}
	}
}

// Registers and starts the /bg result worker, its exit dropping the task and clearing an unowned spinner
func (c *CLI) startBackgroundWorker(taskID string, taskNum int, preview string, produce func() (string, error)) {
	done := make(chan struct{})
	run := c.sideWorker(produce, sideWorkerSpec{
		name: "bg-task-" + taskID, failLabel: c.commandsT("background.label", map[string]any{"number": taskNum}),
		headerLines: []string{"  " + c.commandsT("background.complete", map[string]any{"number": taskNum}),
			"  " + c.commandsT("background.prompt", map[string]any{"preview": preview})},
		titleSuffix: c.commandsT("background.title_suffix", map[string]any{"number": taskNum}),
		emptyNote:   "  " + c.commandsT("background.no_response", nil), bell: true,
		onDone: func() {
			c.uiMu.Lock()
			delete(c.backgroundTasks, taskID)
			if !c.agentRunning {
				c.spinnerText = ""
			}
			c.uiMu.Unlock()
			close(done)
		},
	})
	c.uiMu.Lock()
	c.backgroundTasks[taskID] = done
	c.uiMu.Unlock()
	go run()
}

// Background task id of the bg_HHMMSS_hex6 shape
func newBackgroundTaskID(now time.Time) string {
	id := uuid.New()
	return "bg_" + now.Format("150405") + "_" + hex.EncodeToString(id[:])[:6]
}

// Handles /bg, running the prompt in its own side agent on a worker whose answer prints here
func (c *CLI) handleBackgroundCommand(cmd string) {
	prompt := commandArg(cmd, false)
	if prompt == "" {
		cp(catalogLines(c.commandsT("background.usage", nil))...)
		return
	}
	c.backgroundTaskCounter++
	taskNum := c.backgroundTaskCounter
	preview := ellipsize(prompt, 60)
	if connection := gatewayConnectionFor(c); connection != nil {
		taskID, wait, err := gatewaySideStart(c.ctx, connection, c.sessionID, "background", prompt)
		if err != nil {
			panic(err)
		}
		cp("  "+c.commandsT("background.started", map[string]any{"number": taskNum, "preview": preview}),
			"  "+c.commandsT("background.task_id", map[string]any{"task_id": taskID}), "  "+c.commandsT("background.keep_chatting", nil)+"\n")
		c.startBackgroundWorker(taskID, taskNum, preview, wait)
		return
	}
	taskID := newBackgroundTaskID(time.Now())
	if !c.ensureRuntimeCredentials() {
		cp("  " + c.commandsT("background.no_credentials", nil))
		return
	}
	cp("  "+c.commandsT("background.started", map[string]any{"number": taskNum, "preview": preview}),
		"  "+c.commandsT("background.task_id", map[string]any{"task_id": taskID}), "  "+c.commandsT("background.keep_chatting", nil)+"\n")
	route := c.resolveTurnAgentConfig(prompt)
	rt := route.runtime
	produce := func() (string, error) {
		ctx := c.freshThreadContext()
		bgEnv := *c.env
		bgEnv.Values = &sync.Map{}
		tools.DeclareStatelessChannel(&bgEnv)
		bgEnv.Ask = cliAsker{c: c}
		browser.SetUnlockPromptCallback(ctx, c.vaultUnlockCallback)
		browser.SetSaveLoginPromptCallback(ctx, c.vaultSaveLoginCallback)
		browser.SetCodePromptCallback(ctx, c.vaultCodeCallback)
		skills.SetSecretCaptureCallback(c.secretCaptureCallback)
		defer func() {
			bgEnv.Ask = nil
			skills.SetSecretCaptureCallback(nil)
			browser.SetUnlockPromptCallback(ctx, nil)
			browser.SetSaveLoginPromptCallback(ctx, nil)
			browser.SetCodePromptCallback(ctx, nil)
		}()
		bgAgent, err := agent.NewAIAgent(ctx, agent.AIAgentOptions{
			Model: route.model, ACPCommand: rt.Command, ACPArgs: rt.Args, MaxIterations: c.maxTurns,
			APIKey: rt.APIKey, BaseURL: rt.BaseURL, Provider: rt.Provider, APIMode: rt.APIMode,
			EnabledToolsets: c.enabledToolsets, QuietMode: true, VerboseLogging: false, SessionID: taskID, Platform: "cli",
			SideAgent: true, ReasoningConfig: c.reasoningConfig, ServiceTier: c.serviceTier,
			RequestOverrides: route.requestOverrides, ProvidersAllowed: c.providersOnly, ProvidersIgnored: c.providersIgnore,
			ProvidersOrder: c.providersOrder, ProviderSort: c.providerSort, ProviderRequireParameters: c.providerRequireParams,
			ProviderDataCollection: c.providerDataCollection, OpenRouterMinCodingScore: c.openRouterMinCodingScore,
			FallbackModel: c.fallbackModel, Env: &bgEnv, Cwd: bgEnv.Cwd,
			ThinkingCallback: func(text string) {
				c.uiMu.Lock()
				running := c.agentRunning
				if !running {
					c.spinnerText = text
				}
				c.uiMu.Unlock()
				if !running && c.app != nil {
					_ = c.app.Invalidate()
				}
			},
		})
		if err != nil {
			return "", err
		}
		bgAgent.SetPrintFn(func(string) {})
		defer func() { _ = bgAgent.Close() }()
		result, err := bgAgent.RunConversation(ctx, prompt, agent.RunConversationOptions{TaskID: taskID})
		if err != nil {
			return "", err
		}
		response := ""
		if result != nil {
			response = cliStr(result["final_response"], "")
			if response == "" && pyrepr.Truthy(result["error"]) {
				response = c.gatewayT("model.error_prefix", map[string]any{"error": result["error"]})
			}
		}
		return response, nil
	}
	c.startBackgroundWorker(taskID, taskNum, preview, produce)
}

// Pulls the first state of a sign in and returns it, the rest of the sequence and the abandon func
func firstSignInState(states iter.Seq[provider.SignInState]) (provider.SignInState, iter.Seq[provider.SignInState], func()) {
	next, stop := iter.Pull(states)
	first, ok := next()
	if !ok {
		stop()
		return nil, nil, stop
	}
	rest := func(yield func(provider.SignInState) bool) {
		defer stop()
		for {
			state, ok := next()
			if !ok || !yield(state) {
				return
			}
		}
	}
	return first, rest, stop
}

// Handles /login, printing the device code and finishing the sign in on a worker without blocking input
func (c *CLI) handleLoginCommand(cmdOriginal string) {
	var console sideConsole
	if c.app == nil {
		console = &pyrich.Console{Out: c.console.Out, Color: c.console.ShouldUseColor(), Terminal: c.console.StdoutIsTTY(), Width: terminalColumns()}
	}
	cp("  " + provider.LoginStarting)
	h := home.Dir(c.ctx)
	env := provider.PoolEnv{Store: provider.NewStore(h), Secrets: config.NewSecrets(h), Scope: provider.SecretScopeFrom(c.ctx),
		Config: provider.ConfigLoader(h), Log: cliLogger, Ctx: c.ctx}
	first, rest, abandon := firstSignInState(provider.RunSignIn(env, provider.SignInOptions{Timeout: 8 * time.Second}))
	if first == nil {
		return
	}
	if first.Terminal() {
		abandon()
		cp("  " + first.Copy())
		return
	}
	code, ok := first.(provider.SignInCode)
	if !ok {
		abandon()
		panic(fmt.Errorf("AttributeError: '%s' object has no attribute 'link'", first.Kind()))
	}
	var buf strings.Builder
	if err := provider.RenderSignInCLICode(&buf, code, false, true); err != nil {
		abandon()
		panic(err)
	}
	cp(strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")...)
	settle := func(state provider.SignInState) error {
		completed, ok := state.(provider.SignInCompleted)
		if state.Kind() != "completed" || !ok || !completed.ModelChanged {
			return nil
		}
		if c.model == provider.GuestModel {
			c.model = completed.Model
		}
		return nil
	}
	label := c.commandsT("login.label", nil)
	run := c.sideWorker(func() (string, error) {
		return provider.DrainSignInCopy(rest, true, settle), nil
	}, sideWorkerSpec{name: "login", failLabel: label, headerLines: []string{"  " + label},
		titleSuffix: c.commandsT("login.title_suffix", nil), emptyNote: "  " + c.commandsT("login.no_result", nil), console: console})
	go run()
}

// Handles /btw, answering a side question about the conversation from a history snapshot on a worker
func (c *CLI) handleBtwCommand(cmd string) {
	question := commandArg(cmd, false)
	if question == "" {
		cp(catalogLines(c.commandsT("btw.usage", nil))...)
		return
	}
	if connection := gatewayConnectionFor(c); connection != nil {
		_, wait, err := gatewaySideStart(c.ctx, connection, c.sessionID, "btw", question)
		if err != nil {
			panic(err)
		}
		preview := ellipsize(question, 60)
		cp(append(catalogLines(c.gatewayT("btw.started", map[string]any{"preview": preview})), "")...)
		run := c.sideWorker(wait, sideWorkerSpec{name: "btw-side-question", failLabel: "/btw",
			headerLines: []string{"  " + c.commandsT("btw.header", map[string]any{"preview": preview})},
			titleSuffix: c.commandsT("btw.title_suffix", nil), emptyNote: "  " + c.commandsT("btw.no_answer", nil)})
		go run()
		return
	}
	if !c.ensureRuntimeCredentials() {
		cp("  " + c.commandsT("btw.no_credentials", nil))
		return
	}
	historySnapshot := append([]map[string]any{}, c.conversationHistory...)
	route := c.resolveTurnAgentConfig(question)
	rt := route.runtime
	var parentSession any
	if c.agent != nil {
		parentSession = c.agent.SessionID()
	}
	mainRuntime := map[string]any{
		"model": route.model, "provider": rt.Provider, "base_url": rt.BaseURL, "api_key": rt.APIKey, "api_mode": rt.APIMode,
		"session_id": parentSession,
	}
	preview := ellipsize(question, 60)
	cp(append(catalogLines(c.gatewayT("btw.started", map[string]any{"preview": preview})), "")...)
	produce := func() (string, error) {
		opts := agent.NewSideQuestionOptions()
		opts.MainRuntime = mainRuntime
		return agent.AnswerSideQuestion(c.freshThreadContext(), question, historySnapshot, opts)
	}
	run := c.sideWorker(produce, sideWorkerSpec{name: "btw-side-question", failLabel: "/btw",
		headerLines: []string{"  " + c.commandsT("btw.header", map[string]any{"preview": preview})},
		titleSuffix: c.commandsT("btw.title_suffix", nil), emptyNote: "  " + c.commandsT("btw.no_answer", nil)})
	go run()
}

// Message of a value raised in a worker, the error text for an error
func sideFailureText(v any) string {
	if err, ok := v.(error); ok {
		return pyrepr.ExceptionMessage(err)
	}
	return fmt.Sprint(v)
}
