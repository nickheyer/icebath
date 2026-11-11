package gateway

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runner surface that builds and runs the side agents of a local session on its frozen route
type tuiSideRunner interface {
	SideBackground(ctx context.Context, env *tools.Env, a *SessionAuthority, policy *LocalSessionPolicy, live *LiveSession, taskID, text string, hold func(agent any)) (string, error)
	SideQuestion(ctx context.Context, a *SessionAuthority, ref SessionRef, policy *LocalSessionPolicy, text string, parent any, history []map[string]any, onFork func(fork any), shouldStop func() bool) (string, error)
	SidePreviewRestart(ctx context.Context, env *tools.Env, a *SessionAuthority, policy *LocalSessionPolicy, live *LiveSession, taskID, prompt string, history []map[string]any, progress func(text string), hold func(agent any)) (string, error)
}

// Background, side question and preview restart verbs of a local session
func tuiSideHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"prompt.background": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return sideBackground(ctx, c, ref, params)
		},
		"prompt.btw": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return sideBtw(ctx, c, ref, params)
		},
		"preview.restart": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return previewRestart(ctx, c, ref, params)
		},
	}
}

// Live session, frozen policy, request text and fresh task id of an admitted side request
func admitSide(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, prefix, required string, optional ...string) (*LiveSession, *LocalSessionPolicy, string, string, error) {
	text, isStr := params.GetOr(required, nil).(string)
	allowed := append([]string{"session_id", "profile", required}, optional...)
	invalid := paramsOutside(params, allowed...) || ref.SessionID == "" || !isStr || strings.TrimSpace(text) == ""
	for _, key := range optional {
		if value := params.GetOr(key, nil); value != nil {
			if _, ok := value.(string); !ok {
				invalid = true
			}
		}
	}
	if invalid {
		return nil, nil, "", "", runtimeRefusal("invalid_params")
	}
	a := c.authority
	if _, ok := a.live(ref.SessionID); !ok {
		return nil, nil, "", "", runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:submit"); err != nil {
		return nil, nil, "", "", err
	}
	c.mu.Lock()
	_, subscribed := c.subscriptions[ref.SessionID]
	c.mu.Unlock()
	if !subscribed {
		return nil, nil, "", "", runtimeRefusal("permission_denied")
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, nil, "", "", err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, nil, "", "", err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, nil, "", "", err
	}
	if policy == nil {
		return nil, nil, "", "", runtimeRefusal("permission_denied")
	}
	return live, policy, text, prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6], nil
}

// Holder slot of a side agent, abandoned once the agent it holds reports so
type sideAgentHold struct {
	mu    sync.Mutex
	agent any
}

// Stores the side agent once built
func (h *sideAgentHold) set(agent any) {
	h.mu.Lock()
	h.agent = agent
	h.mu.Unlock()
}

// Agent the stop paths interrupt, nil until built
func (h *sideAgentHold) HolderAgent() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agent
}

// True when the held agent reports itself abandoned
func (h *sideAgentHold) Abandoned() bool {
	abandoner, ok := h.HolderAgent().(interface{ Abandoned() bool })
	return ok && abandoner.Abandoned()
}

// Background prompt run to completion by a fresh agent on the session's route
func sideBackground(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	live, policy, text, taskID, err := admitSide(ctx, c, ref, params, "bg", "text")
	if err != nil {
		return nil, err
	}
	a := c.authority
	runner, ok := a.Runner.(tuiSideRunner)
	if !ok {
		return nil, attributeError("_resolve_policy_agent_runtime")
	}
	holder := &sideAgentHold{}
	body := func(ctx context.Context) (string, error) {
		var out string
		err := SideScope(ctx, a, policy, taskID, func(scoped context.Context, env *tools.Env) error {
			answer, err := runner.SideBackground(scoped, env, a, policy, live, taskID, text, holder.set)
			out = answer
			return err
		})
		return out, err
	}
	return SpawnSideAgent(ctx, a, ref, taskID, "background.complete", body, nil, holder), nil
}

// Stop handle of a side question, interrupting its cache parity fork and abandoned once stopped with no fork
type sideQuestionStop struct {
	mu      sync.Mutex
	stopped bool
	fork    any
}

// Records the running fork, interrupting it at once when already stopped
func (s *sideQuestionStop) setFork(fork any) {
	s.mu.Lock()
	s.fork = fork
	stopped := s.stopped
	s.mu.Unlock()
	if fork != nil && stopped {
		s.HardInterrupt("Profile runtime is stopping")
	}
}

// Stops the question and interrupts its fork when one runs
func (s *sideQuestionStop) HardInterrupt(message string) {
	s.mu.Lock()
	s.stopped = true
	fork := s.fork
	s.mu.Unlock()
	if fork != nil {
		requestHardInterruptWith(fork, message, "")
	}
}

// Whether a stop was requested
func (s *sideQuestionStop) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// Stopped with no fork running, what is left publishes and writes nothing
func (s *sideQuestionStop) Abandoned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped && s.fork == nil
}

// The handle itself is what the stop paths interrupt
func (s *sideQuestionStop) HolderAgent() any { return s }

// Side question over a snapshot of the conversation, the live history and FIFO untouched
func sideBtw(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	_, policy, text, taskID, err := admitSide(ctx, c, ref, params, "btw", "text")
	if err != nil {
		return nil, err
	}
	a := c.authority
	runner, ok := a.Runner.(tuiSideRunner)
	if !ok {
		return nil, attributeError("_resolve_policy_agent_runtime")
	}
	parent, err := a.agent(ref)
	if err != nil {
		return nil, err
	}
	if nilAgent(parent) {
		parent = nil
	}
	var snapshot []map[string]any
	if held, ok := parent.(interface{ RetiringSessionMessages() []map[string]any }); ok {
		snapshot = append(snapshot, held.RetiringSessionMessages()...)
	}
	target := ""
	if len(snapshot) == 0 {
		if target, err = transcriptTarget(ctx, a, ref); err != nil {
			return nil, err
		}
	}
	handle := &sideQuestionStop{}
	body := func(ctx context.Context) (string, error) {
		history := snapshot
		if len(history) == 0 {
			rows, err := a.DB.GetMessagesAsConversation(ctx, target, state.ConversationOptions{})
			if err != nil {
				return "", err
			}
			history = rows
		}
		var out string
		err := SideScope(ctx, a, policy, taskID, func(scoped context.Context, env *tools.Env) error {
			answer, err := runner.SideQuestion(scoped, a, ref, policy, text, parent, history, handle.setFork, handle.isStopped)
			out = answer
			return err
		})
		return out, err
	}
	return SpawnSideAgent(ctx, a, ref, taskID, "btw.complete", body, pyjson.ObjectOf("question", text), handle), nil
}

const previewRestartHistoryNote = "The conversation history above is from the user's main session — including the commands you (the assistant) previously ran to start servers, edit files, or check ports. Use it to figure out exactly which server should be running at this Preview URL. The user did not start a brand new task; recover what they had working."

var previewRestartRules = []string{
	"Restart exactly the app intended for the Preview URL, not Icebath Desktop itself.",
	"The Preview URL and port are the target. Preserve that target unless you conclude it is impossible.",
	"If the prior conversation shows a specific command that bound this URL/port, prefer re-running THAT exact command (in the same cwd) over guessing a new one.",
	"First inspect what process, if any, owns the Preview URL port. If a stale server exists, inspect its cwd and prefer that cwd over the Icebath/Desktop process cwd.",
	"The Current working directory is only a hint. Do not assume it is the preview app root when the port owner or files indicate another root.",
	"If the console shows a module-script MIME error for src/main.tsx or similar, a static server is serving source files. Do not restart python -m http.server or any dumb static server for that app.",
	"For module-script MIME failures, inspect package.json/vite config in the candidate app root and start the real dev server/bundler (for example npm/pnpm/yarn dev) so module transforms happen.",
	"Before declaring success, verify the Preview URL responds with the intended app, not Icebath Desktop. If it serves Icebath/Desktop UI or another unrelated app, stop that process and report failure.",
	"Do not modify files. Do not ask the user unless blocked.",
	"Prefer existing project scripts or commands when they are clear.",
	"If a stale process owns the needed port, handle it safely.",
	"Start long-running servers detached/in the background, then return immediately.",
	"Do not run a foreground dev server command that blocks this background task.",
	"Keep the final response short: what command/server was started, or why it could not be restarted.",
}

// Hidden restart agent's task for the preview URL, its console and whether history rides along
func PreviewRestartPrompt(url, cwd, consoleContext string, hasHistory bool) string {
	lines := []string{"The desktop preview pane cannot load a local server URL.", "Preview URL: " + strings.TrimSpace(url)}
	if cwd == "" {
		lines = append(lines, "Current working directory: (unknown)")
	} else {
		lines = append(lines, "Current working directory: "+cwd)
	}
	if consoleContext != "" {
		lines = append(lines, "Preview console:\n"+consoleContext)
	}
	if hasHistory {
		lines = append(lines, previewRestartHistoryNote)
	}
	return strings.Join(append(lines, previewRestartRules...), "\n")
}

// Client cwd as an existing absolute directory with the user expanded, empty for anything else
func ValidatedPreviewCwd(cwd string) string {
	if cwd == "" || strings.ContainsRune(cwd, 0) {
		return ""
	}
	expanded := cwd
	if strings.HasPrefix(cwd, "~") {
		name, rest, _ := strings.Cut(cwd[1:], string(filepath.Separator))
		if filepath.Separator != '/' {
			if before, after, cut := strings.Cut(cwd[1:], "/"); cut && len(before) < len(name) {
				name, rest = before, after
			}
		}
		var userHome string
		if name == "" {
			userHome, _ = os.UserHomeDir()
		} else if account, err := user.Lookup(name); err == nil {
			userHome = account.HomeDir
		}
		if userHome != "" {
			expanded = userHome
			if len(cwd) > len(name)+1 {
				expanded = filepath.Join(userHome, rest)
			}
		}
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return ""
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return ""
	}
	return absolute
}

// Recent parent history back to the last user turn for the restart agent, reasoning dropped and tool output capped
func PreviewRestartHistory(messages []map[string]any, maxMessages, maxToolChars int) []map[string]any {
	if len(messages) == 0 {
		return []map[string]any{}
	}
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] == "user" {
			lastUser = i
			break
		}
	}
	start := max(0, len(messages)-maxMessages)
	if lastUser >= 0 {
		start = min(start, lastUser)
	}
	trimmed := []map[string]any{}
	for _, msg := range messages[start:] {
		switch msg["role"] {
		case "user", "assistant", "tool", "system":
		default:
			continue
		}
		copied := make(map[string]any, len(msg))
		for key, value := range msg {
			if key != "reasoning" {
				copied[key] = value
			}
		}
		if content, isStr := copied["content"].(string); msg["role"] == "tool" && isStr {
			if runes := []rune(content); len(runes) > maxToolChars {
				copied["content"] = string(runes[:maxToolChars]) + "\n... (truncated, original " + pyrepr.Str(len(runes)) + " chars)"
			}
		}
		trimmed = append(trimmed, copied)
	}
	return trimmed
}

// Desktop preview restart, a hidden terminal and file agent bringing the preview URL's server back up
func previewRestart(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	live, policy, url, taskID, err := admitSide(ctx, c, ref, params, "preview", "url", "cwd", "context")
	if err != nil {
		return nil, err
	}
	a := c.authority
	runner, ok := a.Runner.(tuiSideRunner)
	if !ok {
		return nil, attributeError("_resolve_policy_agent_runtime")
	}
	field := func(key string) string {
		value := params.GetOr(key, nil)
		if !pyrepr.Truthy(value) {
			return ""
		}
		return strings.TrimSpace(pyrepr.Str(value))
	}
	cwd, consoleContext := field("cwd"), field("context")
	target, err := transcriptTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	holder := &sideAgentHold{}
	progress := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if current, ok := a.live(ref.SessionID); ok && current != nil {
			current.EventStream.Publish(ref.SessionID, pyjson.ObjectOf("task_id", taskID, "text", text), "preview.restart.progress", nil)
		}
	}
	body := func(ctx context.Context) (string, error) {
		previewCwd := ValidatedPreviewCwd(cwd)
		var stored []map[string]any
		if target != "" {
			rows, err := a.DB.GetMessagesAsConversation(ctx, target, state.ConversationOptions{})
			if err != nil {
				return "", err
			}
			stored = rows
		}
		history := PreviewRestartHistory(stored, 24, 1200)
		prompt := PreviewRestartPrompt(url, cwd, consoleContext, len(history) > 0)
		var out string
		err := SideScope(ctx, a, policy, taskID, func(scoped context.Context, env *tools.Env) error {
			if previewCwd != "" {
				if err := tools.RegisterTaskEnvOverrides(env, taskID, map[string]any{"cwd": previewCwd}); err != nil {
					return err
				}
			}
			defer tools.ClearTaskEnvOverrides(env, taskID)
			progress("Starting hidden restart agent")
			answer, err := runner.SidePreviewRestart(scoped, env, a, policy, live, taskID, prompt, history, progress, holder.set)
			out = answer
			return err
		})
		return out, err
	}
	return SpawnSideAgent(ctx, a, ref, taskID, "preview.restart.complete", body, nil, holder), nil
}
