package agent

import (
	"context"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	reviewMaxIterations = 16
	reviewDigestTail    = 24
)

// Prompt name chosen by the memory and skills review scope, skills only being the default
var promptNameByScope = map[[2]bool]string{
	{true, true}: "_COMBINED_REVIEW_PROMPT", {true, false}: "_MEMORY_REVIEW_PROMPT",
	{false, true}: "_SKILL_REVIEW_PROMPT", {false, false}: "_SKILL_REVIEW_PROMPT",
}

var defaultReviewPrompts = map[string]string{
	"_COMBINED_REVIEW_PROMPT": combinedReviewPrompt, "_MEMORY_REVIEW_PROMPT": memoryReviewPrompt,
	"_SKILL_REVIEW_PROMPT": skillReviewPrompt,
}

// Fork run the review worker drives, replaced by tests standing in for the provider
var runReviewForkConversation func(ctx context.Context, fork *AIAgent, userMessage string, history []map[string]any) error

func init() { runReviewForkConversation = runForkConversation }

// Runs one review turn on the fork with the snapshot as its history
func runForkConversation(ctx context.Context, fork *AIAgent, userMessage string, history []map[string]any) error {
	_, err := fork.RunConversation(ctx, userMessage, RunConversationOptions{ConversationHistory: history})
	return err
}

// Denies every dangerous command the review fork trips instead of prompting against the parent's console
func bgReviewAutoDeny(command, description string, _ map[string]any) string {
	backgroundReviewLog().Warn(fmt.Sprintf("Background review auto-denied dangerous command: %s (%s)", command, description))
	return "deny"
}

// Answers the fork's dangerous command approvals through callback, or restores the session asker when nil
func setThreadApprovalCallback(fork *AIAgent, callback func(command, description string, kwargs map[string]any) string) {
	if fork == nil || fork.env == nil {
		return
	}
	env := *fork.env
	env.Ask = nil
	if callback != nil {
		env.Ask = subagentApprovalAsker{cb: callback}
	}
	fork.env = &env
}

// Adds or removes the fork on the parent's interrupt targets, removal scoped to that fork
func trackReviewFork(a, reviewAgent *AIAgent, register bool) {
	if reviewAgent == nil {
		return
	}
	withOptionalLock(a.backgroundReviewLock, func() {
		if register {
			a.backgroundReviewAgent = reviewAgent
		} else if a.backgroundReviewAgent == reviewAgent {
			a.backgroundReviewAgent = nil
		}
	})
	withOptionalLock(a.activeChildrenLock, func() {
		if register {
			a.activeChildren = append(a.activeChildren, reviewAgent)
		} else if i := slices.Index(a.activeChildren, reviewAgent); i >= 0 {
			a.activeChildren = slices.Delete(a.activeChildren, i, i+1)
		}
	})
}

// Dispatch whitelist of the review fork and the configured extra tools, memory only when the profile and trigger allow it
func reviewToolWhitelist(ctx context.Context, reviewAgent *AIAgent, taskCfg map[string]any, reviewMemory bool) (map[string]bool, map[string]bool) {
	toolsets := []string{"skills"}
	if (reviewAgent.memoryEnabled || reviewAgent.userProfileEnabled) && reviewMemory {
		toolsets = []string{"memory", "skills"}
	}
	whitelist := map[string]bool{"read_file": true, "search_files": true}
	for _, def := range tools.Default.ToolDefinitions(ctx, reviewAgent.env, tools.DefinitionOptions{Enabled: toolsets, Quiet: true}) {
		if fn, ok := pyjson.Dict(def["function"]); ok {
			if name, ok := fn["name"].(string); ok {
				whitelist[name] = true
			}
		}
	}
	extra := map[string]bool{}
	if raw, ok := backgroundReviewTaskConfig(ctx, taskCfg)["extra_tools"].([]any); ok {
		for _, item := range raw {
			if name, ok := item.(string); ok && pystr.Strip(name) != "" {
				extra[pystr.Strip(name)] = true
			}
		}
	}
	for name := range extra {
		whitelist[name] = true
	}
	return whitelist, extra
}

// Fork phase progress the worker's error and cleanup paths read
type reviewForkState struct {
	reviewAgent    *AIAgent
	reviewMessages []map[string]any
	reviewUsage    map[string]any
}

// Builds the fork, runs the prompt under the whitelist, records usage and releases the fork's clients
func runReviewFork(ctx context.Context, a *AIAgent, messagesSnapshot []map[string]any, prompt string, taskCfg map[string]any,
	run *backgroundReviewRun, st *reviewForkState, reviewMemory, explicit bool) error {
	fork, _, routed, err := buildCacheParityFork(ctx, a, taskCfg, reviewMaxIterations, backgroundReviewOrigin)
	if err != nil {
		return err
	}
	st.reviewAgent = fork
	fork.reviewAttended = explicit
	setThreadApprovalCallback(fork, bgReviewAutoDeny)
	trackReviewFork(a, fork, true)
	whitelist, extra := reviewToolWhitelist(ctx, fork, taskCfg, reviewMemory)
	extraNames := make([]string, 0, len(extra))
	for name := range extra {
		extraNames = append(extraNames, name)
	}
	slices.Sort(extraNames)
	extraList := strings.Join(extraNames, ", ")
	denyExtra, promptExtra := "", ""
	if len(extra) > 0 {
		denyExtra = " Configured extra tools also allowed: " + extraList + "."
		promptExtra = " Exception — these configured tools are also allowed: " + extraList + "."
	}
	memoryPhraseDeny, memoryPhrasePrompt := "", "skill "
	if whitelist["memory"] {
		memoryPhraseDeny, memoryPhrasePrompt = " and memory for notes (add only)", "memory and skill "
	}
	runCtx := plugins.WithToolWhitelist(ctx, whitelist, "Background review denied non-whitelisted tool: "+
		"{tool_name}. Allowed here: skill_view/skills_list/read_file/search_files to read, "+
		"skill_manage(action='patch'|...) to change skills"+memoryPhraseDeny+"."+denyExtra+" Do not retry {tool_name}.")
	func() {
		defer func() { _ = recover() }()
		skills.ResetBackgroundReviewReadMarks(fork.env)
	}()
	err = func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
			if st.reviewAgent != nil {
				if st.reviewUsage == nil {
					st.reviewUsage = map[string]any{}
				}
				for k, v := range snapshotReviewUsage(st.reviewAgent) {
					st.reviewUsage[k] = v
				}
				recordReviewUsageToParent(ctx, a, st.reviewUsage, "", "")
			}
			trackReviewFork(a, st.reviewAgent, false)
			finishBackgroundReviewRun(a, run)
		}()
		if run != nil && !run.beginRequest(fork) {
			return nil
		}
		history := messagesSnapshot
		if routed {
			history = digestHistory(messagesSnapshot, reviewDigestTail)
		}
		userMessage := prompt + "\n\nYou can only call " + memoryPhrasePrompt + "management tools. Other tools will be denied " +
			"at runtime — do not attempt them." + promptExtra
		return runReviewForkConversation(runCtx, fork, userMessage, history)
	}()
	if err != nil {
		return err
	}
	st.reviewMessages = sessionMessageDicts(fork.liveSessionMessages())
	releaseForkClients(ctx, fork)
	st.reviewAgent = nil
	return nil
}

// Session messages of an agent as message dicts
func sessionMessageDicts(messages []*state.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		out = append(out, state.MessageToDict(m))
	}
	return out
}

// SessionMessageDicts returns the session messages as plain row maps
func SessionMessageDicts(messages []*state.Message) []map[string]any {
	return sessionMessageDicts(messages)
}

// Prints the deduplicated review summary and forwards it to the background review callback
func publishReviewSummary(ctx context.Context, a *AIAgent, actions []string) {
	var unique []string
	for _, action := range actions {
		if !slices.Contains(unique, action) {
			unique = append(unique, action)
		}
	}
	summary := strings.Join(unique, " · ")
	a.safePrint(i18n.T(ctx, "display.review.summary_cli", "", map[string]any{"summary": summary}), false)
	if pyTruthy(a.backgroundReviewCallback) {
		func() {
			defer func() { _ = recover() }()
			if cb, ok := a.backgroundReviewCallback.(func(string)); ok {
				cb(i18n.T(ctx, "display.review.summary_callback", "", map[string]any{"summary": summary}))
			}
		}()
	}
}

// Review worker, skipping a run cancelled before start or a fork that could never emit tool calls
var runReviewInThread = func(ctx context.Context, a *AIAgent, messagesSnapshot []map[string]any, prompt string, taskCfg map[string]any,
	run *backgroundReviewRun, reviewMemory, explicit bool) {
	if run != nil && run.cancelRequestedSet() {
		finishBackgroundReviewRun(a, run)
		return
	}
	if !parentCanEmitToolCalls(a) && !resolveReviewRuntime(ctx, a, taskCfg).Routed {
		backgroundReviewLog().Warn(fmt.Sprintf("Background review skipped: provider %s cannot emit Hermes tool calls, "+
			"so the review fork could not write memories or skills. Set "+
			"auxiliary.background_review.{provider,model} to route the review to a normal model.", pyStrRepr(a.provider)))
		return
	}
	st := &reviewForkState{reviewUsage: map[string]any{}}
	silent := tools.ThreadScopedSilence(ctx)
	defer func() {
		trackReviewFork(a, st.reviewAgent, false)
		finishBackgroundReviewRun(a, run)
		if st.reviewAgent != nil {
			releaseForkClients(silent, st.reviewAgent)
		}
	}()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if err := runReviewFork(silent, a, messagesSnapshot, prompt, taskCfg, run, st, reviewMemory, explicit); err != nil {
			return err
		}
		actions, serr := summarizeBackgroundReviewActions(ctx, st.reviewMessages, messagesSnapshot, a.memoryNotifications)
		if serr != nil {
			backgroundReviewLog().Warn(fmt.Sprintf("summarize_background_review_actions returned partial results "+
				"after exception (treating as empty); suppressing AttributeError "+
				"that previously aborted the entire review (#59437): %v", serr))
			actions = []string{}
		}
		logReviewCompletion(st.reviewUsage, classifyReviewResult(ctx, actions))
		if len(actions) > 0 {
			publishReviewSummary(ctx, a, actions)
		}
		return nil
	}()
	if err == nil {
		return
	}
	backgroundReviewLog().Warn(fmt.Sprintf("Background memory/skill review failed: %v", err))
	if len(st.reviewUsage) > 0 {
		logReviewCompletion(st.reviewUsage, "error")
	}
	a.emitAuxiliaryFailure(i18n.T(ctx, "display.review.aux_failure_label", "", nil), err)
}

// Prompt override the agent carries under name, else the module prompt
func reviewPromptFor(a *AIAgent, name string) string {
	if prompt, ok := a.reviewPrompts[name]; ok {
		return prompt
	}
	return defaultReviewPrompts[name]
}

// Review worker target and its prompt, the focus appended and the worker resolved when the target runs
var spawnBackgroundReviewThread = func(ctx context.Context, a *AIAgent, kw backgroundReviewKwargs, run *backgroundReviewRun) (func(context.Context), string, error) {
	taskCfg := kw.taskCfg
	if taskCfg == nil {
		taskCfg = backgroundReviewTaskConfig(ctx, nil)
	}
	prompt := reviewPromptFor(a, promptNameByScope[[2]bool{kw.reviewMemory, kw.reviewSkills}])
	focus := ""
	if kw.focus != nil {
		focus = pystr.Strip(*kw.focus)
	}
	if focus != "" {
		prompt = prompt + "\n\nThe user explicitly requested this review with the following " +
			"focus — prioritize it over the general instructions above:\n" + focus
	}
	target := func(threadCtx context.Context) {
		runReviewInThread(threadCtx, a, kw.messagesSnapshot, prompt, taskCfg, run, kw.reviewMemory, kw.explicit)
	}
	return target, prompt, nil
}
