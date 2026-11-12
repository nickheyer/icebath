package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds a live turn waits for the review to acknowledge its cancellation, a test shortens it
var backgroundReviewCancelTimeoutSeconds = 2.0

const (
	reviewSupersededMessage      = "superseded by a new live turn"
	reviewSupersededToolReason   = "background review superseded"
	reviewMaxInputTokensCap      = 600_000
	reviewInputContextFraction   = 0.75
	reviewMaxInputTokensFallback = 120_000
)

// Per agent review notice latches and prompt overrides keyed by prompt name
type backgroundReviewState struct {
	warnedBgReviewRouting         bool
	warnedBgReviewReasoningEffort bool
	reviewPrompts                 map[string]string
}

// Per review cancellation and request completion handshake
type backgroundReviewRun struct {
	cancelRequested  pythread.Event
	requestDone      pythread.Event
	mu               sync.Mutex
	reviewAgent      any
	requestFinished  bool
	cancelDispatched bool
}

// Admits the first provider capable review phase unless the run was cancelled or finished
func (r *backgroundReviewRun) beginRequest(reviewAgent any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelRequested.IsSet() || r.requestFinished {
		return false
	}
	r.reviewAgent = reviewAgent
	return true
}

// Fences startup and returns the admitted fork once, nil when none is running
func (r *backgroundReviewRun) cancel() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelRequested.Set()
	if r.reviewAgent == nil || r.cancelDispatched {
		return nil
	}
	r.cancelDispatched = true
	return r.reviewAgent
}

// Latches request completion once, the caller publishes the done event
func (r *backgroundReviewRun) markRequestFinished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requestFinished {
		return false
	}
	r.requestFinished, r.reviewAgent = true, nil
	return true
}

// Whether a live turn requested this run cancelled
func (r *backgroundReviewRun) cancelRequestedSet() bool { return r.cancelRequested.IsSet() }

// Review run installed on the agent, nil when none is
func currentBackgroundReviewRun(a *AIAgent) *backgroundReviewRun {
	run, _ := a.backgroundReviewRun.(*backgroundReviewRun)
	return run
}

// Installs a fresh run token on the agent, nil while an earlier review still holds its request phase
func prepareBackgroundReviewRun(a *AIAgent) *backgroundReviewRun {
	if a == nil {
		return nil
	}
	run := &backgroundReviewRun{}
	if a.backgroundReviewLock == nil {
		a.backgroundReviewLock = &sync.Mutex{}
	}
	a.backgroundReviewLock.Lock()
	defer a.backgroundReviewLock.Unlock()
	if current := currentBackgroundReviewRun(a); current != nil && !current.requestDone.IsSet() {
		return nil
	}
	a.backgroundReviewRun = run
	return run
}

// Publishes one run's request exit without clearing a successor run
func finishBackgroundReviewRun(a *AIAgent, run *backgroundReviewRun) {
	if run == nil || !run.markRequestFinished() {
		return
	}
	withOptionalLock(a.backgroundReviewLock, func() {
		if currentBackgroundReviewRun(a) == run {
			a.backgroundReviewRun = nil
		}
	})
	run.requestDone.Set()
}

// Runs fn under the lock, or bare when the lock is absent
func withOptionalLock(lock *sync.Mutex, fn func()) {
	if lock == nil {
		fn()
		return
	}
	lock.Lock()
	defer lock.Unlock()
	fn()
}

// Requests the review abort on its own goroutine so a wedged abort hook cannot stall the live turn
func interruptBackgroundReview(reviewAgent any, message, toolReason string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				backgroundReviewLog().Debug(fmt.Sprintf("Failed to cancel in-flight background review (%s): %v", message, r))
			}
		}()
		RequestHardInterruptOf(reviewAgent, &message, &toolReason)
	}()
}

// Waits for the review's request exit event, replaced by tests observing the wait
var awaitReviewAck = func(run *backgroundReviewRun, timeout float64) bool { return run.requestDone.Wait(timeout) }

// Cancels the running review for a new live turn and waits briefly for its acknowledgement
func cancelBackgroundReviewForLiveTurn(a *AIAgent) {
	a.CancelBackgroundReview(reviewSupersededMessage, reviewSupersededToolReason)
}

// Cancels the running review with the given reason and waits a bounded time, then lets the caller proceed
func (a *AIAgent) CancelBackgroundReview(message, toolReason string) {
	var run *backgroundReviewRun
	var legacy *AIAgent
	withOptionalLock(a.backgroundReviewLock, func() {
		run = currentBackgroundReviewRun(a)
		legacy = a.backgroundReviewAgent
	})
	var reviewAgent any
	if run == nil {
		if legacy != nil {
			reviewAgent = legacy
		}
	} else {
		reviewAgent = run.cancel()
	}
	if reviewAgent != nil {
		interruptBackgroundReview(reviewAgent, message, toolReason)
	}
	if run == nil {
		return
	}
	if !awaitReviewAck(run, backgroundReviewCancelTimeoutSeconds) {
		backgroundReviewLog().Warn(fmt.Sprintf("Background review did not acknowledge cancellation within %.1fs; proceeding (%s)",
			backgroundReviewCancelTimeoutSeconds, message))
	}
}

// The auxiliary background_review block of a config tree, empty on any shape mismatch
func reviewTaskBlock(cfg map[string]any) map[string]any {
	aux, ok := cfg["auxiliary"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	task, ok := aux["background_review"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return task
}

// Preloaded task config when given, else the background_review block read from config, empty on failure
func backgroundReviewTaskConfig(ctx context.Context, taskCfg map[string]any) map[string]any {
	if taskCfg != nil {
		return taskCfg
	}
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return map[string]any{}
	}
	return reviewTaskBlock(tree)
}

// Seventy five percent of the review fork context window capped at the historical ceiling, a fixed fallback when unknown
func contextDerivedReviewInputBudget(reviewAgent *AIAgent) int {
	if reviewAgent == nil || reviewAgent.contextCompressor == nil {
		return reviewMaxInputTokensFallback
	}
	window := reviewAgent.contextCompressor.Base().ContextLength
	if window <= 0 {
		return reviewMaxInputTokensFallback
	}
	return min(reviewMaxInputTokensCap, max(1, int(float64(window)*reviewInputContextFraction)))
}

// Aggregate input token budget of one review fork, zero for unlimited when the configured value is not positive
func reviewInputTokenBudget(ctx context.Context, taskCfg map[string]any, reviewAgent *AIAgent) int {
	task := backgroundReviewTaskConfig(ctx, taskCfg)
	raw, ok := task["max_input_tokens"]
	if !ok {
		return contextDerivedReviewInputBudget(reviewAgent)
	}
	budget, ok := pyIntCoerce(raw)
	if !ok {
		return contextDerivedReviewInputBudget(reviewAgent)
	}
	if budget > 0 {
		return budget
	}
	return 0
}

// Whether automatic reviews are enabled with the task config, disabled for safe workers and finite turns
var loadBackgroundReviewSettings = func(ctx context.Context) (bool, map[string]any) {
	if config.SafeWorkerEnabled() {
		return false, map[string]any{}
	}
	if tools.FiniteTurnRequired != nil {
		if required, known := tools.FiniteTurnRequired(lifecycleEnv(ctx)); known && required {
			return false, map[string]any{}
		}
	}
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		backgroundReviewLog().Warn(fmt.Sprintf("Failed to read background_review.enabled; leaving automatic review enabled (fail-open): %v", err))
		return true, map[string]any{}
	}
	task := reviewTaskBlock(tree)
	return isTruthyValue(task["enabled"], true), task
}

// Runtime resolution the routed review path reads
var reviewResolveRuntimeProvider = provider.ResolveRuntimeProvider

// Provider, model and credentials a review fork runs on, routed when the config names a different model
type reviewRuntime struct {
	Provider, Model, APIKey, BaseURL, APIMode, Command string
	CredentialPool                                     *provider.Pool
	RequestOverrides                                   map[string]any
	MaxTokens                                          *int
	Args                                               []string
	Routed                                             bool
}

// Review fork runtime, the parent live runtime unless background_review names a different concrete model
var resolveReviewRuntime = func(ctx context.Context, a *AIAgent, taskCfg map[string]any) reviewRuntime {
	parentRuntime := a.currentMainRuntime()
	apiMode := parentRuntime["api_mode"]
	if apiMode == "codex_app_server" {
		apiMode = "codex_responses"
	}
	overrides := map[string]any{}
	for k, v := range a.requestOverrides {
		overrides[k] = v
	}
	parent := reviewRuntime{
		Provider: a.provider, Model: a.model, APIKey: parentRuntime["api_key"], BaseURL: parentRuntime["base_url"],
		APIMode: apiMode, CredentialPool: a.credentialPool, RequestOverrides: overrides, MaxTokens: a.maxTokens,
		Command: a.acpCommand, Args: append([]string{}, a.acpArgs...),
	}
	task := backgroundReviewTaskConfig(ctx, taskCfg)
	field := func(key string) string {
		v, ok := task[key]
		if !ok {
			return ""
		}
		return strings.TrimSpace(pyrepr.Str(v))
	}
	taskProvider, taskModel, taskBaseURL, taskAPIKey := field("provider"), field("model"), field("base_url"), field("api_key")
	if taskProvider == "" || taskProvider == "auto" || taskModel == "" || (taskProvider == a.provider && taskModel == a.model) {
		return parent
	}
	rp, err := func() (rt *provider.Runtime, err error) {
		defer func() {
			if r := recover(); r != nil {
				rt, err = nil, fmt.Errorf("%v", r)
			}
		}()
		return reviewResolveRuntimeProvider(ctx, home.Dir(ctx), taskProvider, taskModel, taskBaseURL, taskAPIKey)
	}()
	if err != nil {
		warnReviewRoutingFallback(ctx, a, taskProvider, taskModel, err)
		return parent
	}
	routed := reviewRuntime{
		Provider: rp.Provider, Model: rp.Model, APIKey: rp.APIKey, BaseURL: rp.BaseURL, APIMode: string(rp.APIMode),
		CredentialPool: rp.CredentialPool, Command: rp.Command, RequestOverrides: map[string]any{},
		Args: append([]string{}, rp.Args...), Routed: true,
	}
	if routed.Provider == "" {
		routed.Provider = taskProvider
	}
	if routed.Model == "" {
		routed.Model = taskModel
	}
	for k, v := range rp.RequestOverrides {
		routed.RequestOverrides[k] = v
	}
	return routed
}

// Logs every unresolvable review route and tells the user once per agent that reviews run on the main model
func warnReviewRoutingFallback(ctx context.Context, a *AIAgent, taskProvider, taskModel string, err error) {
	errorLine := ""
	if text := err.Error(); text != "" {
		errorLine = strings.SplitN(text, "\n", 2)[0]
	}
	backgroundReviewLog().Warn(fmt.Sprintf("auxiliary.background_review.provider=%s (model %s) could not be resolved: %s — background reviews "+
		"run on the main model %s/%s instead.", pyStrRepr(taskProvider), pyStrRepr(taskModel), errorLine, a.provider, a.model))
	message := i18n.T(ctx, "display.review.routing_fallback_warning", "", map[string]any{
		"task_provider": taskProvider, "task_model": taskModel, "error": errorLine, "provider": a.provider, "model": a.model,
	})
	if a.warnedBgReviewRouting {
		return
	}
	a.warnedBgReviewRouting = true
	func() {
		defer func() { _ = recover() }()
		a.emitWarning(message)
	}()
}

// HermesToolCallSupport is a client that declares whether it can emit hermes tool calls
type HermesToolCallSupport interface{ SupportsHermesToolCalls() bool }

// Whether a fork inheriting the parent client could act, false only for a client declaring no tool call support
func parentCanEmitToolCalls(a *AIAgent) bool {
	if client, ok := a.client.(HermesToolCallSupport); ok {
		return client.SupportsHermesToolCalls()
	}
	return true
}

// Trimmed text of a message, joining text blocks of list content
func reviewMsgText(m map[string]any) string {
	switch c := m["content"].(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		parts := make([]string, 0, len(c))
		for _, b := range c {
			if block, ok := b.(map[string]any); ok {
				text, _ := block["text"].(string)
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return ""
}

// Keeps the recent tail verbatim, never opening on a tool row, and folds older turns into one user digest
func digestHistory(messagesSnapshot []map[string]any, tail int) []map[string]any {
	msgs := append([]map[string]any{}, messagesSnapshot...)
	var keep []map[string]any
	for {
		if len(msgs) <= tail {
			return msgs
		}
		keep = msgs[len(msgs)-tail:]
		if keep[0] == nil || keep[0]["role"] != "tool" {
			break
		}
		tail++
	}
	var lines []string
	for _, m := range msgs[:len(msgs)-len(keep)] {
		if m == nil {
			continue
		}
		role, text := m["role"], strings.ReplaceAll(reviewMsgText(m), "\n", " ")
		if role == "user" && text != "" {
			lines = append(lines, "USER: "+runePrefix(text, 300))
		} else if role == "assistant" {
			if calls := reviewToolCalls(m["tool_calls"]); len(calls) > 0 {
				var names []string
				for _, tc := range calls {
					call, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					name := "?"
					if fn, ok := call["function"].(map[string]any); ok {
						if n, has := fn["name"]; has {
							name = pyrepr.Str(n)
						}
					}
					names = append(names, name)
				}
				lines = append(lines, "ASSISTANT[tools: "+strings.Join(names, ", ")+"]")
			}
			if text != "" {
				lines = append(lines, "ASSISTANT: "+runePrefix(text, 200))
			}
		}
	}
	digest := "[Earlier conversation digest — older turns summarised to bound the " +
		"review's cold-write cost on the routed aux model. Recent turns " +
		"follow verbatim below.]\n" + strings.Join(lines, "\n")
	return append([]map[string]any{{"role": "user", "content": digest}}, keep...)
}

// First n code points of s
func runePrefix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Tool call entries of a message as a generic list
func reviewToolCalls(v any) []any {
	switch calls := v.(type) {
	case []any:
		return calls
	case []map[string]any:
		out := make([]any, len(calls))
		for i, c := range calls {
			out[i] = c
		}
		return out
	}
	return nil
}
