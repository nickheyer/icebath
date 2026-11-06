package builtin

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Claude tool names the edit hooks report for the icebath write tools
var hookToolNames = map[string]string{"write_file": "Write", "patch": "Edit"}

var hookTruthy = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

const (
	impeccableEditTimeout = 5 * time.Second
	impeccableStopTimeout = 30 * time.Second
	hallmarkLintTimeout   = 10 * time.Second
)

// Launcher file name for the host platform
func impeccableLauncherName() string {
	if runtime.GOOS == "windows" {
		return "impeccable.cmd"
	}
	return "impeccable"
}

// Paths a write tool reports it wrote, else the path argument, resolved against the session cwd
func editedPaths(result string, args any, cwd string) []string {
	parsed, _ := pyjson.Loads(result)
	var raw []any
	if obj, ok := pyjson.AsObject(parsed); ok {
		if _, has := obj.Get("error"); has {
			return nil
		}
		if modified, ok := obj.Get("files_modified"); ok {
			if list, ok := modified.([]any); ok {
				raw = list
			}
		}
		if raw == nil {
			v, _ := obj.Get("resolved_path")
			raw = []any{v}
		}
	}
	usable := false
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			usable = true
		}
	}
	if !usable {
		raw = []any{argValue(args, "path")}
	}
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			p := s
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, p)
			}
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}

// One key of a tool argument mapping in either plain or ordered form
func argValue(args any, key string) any {
	switch a := args.(type) {
	case map[string]any:
		return a[key]
	case *pyjson.Object:
		v, _ := a.Get(key)
		return v
	}
	return nil
}

// Result with findings under a key when it is a JSON object, else appended after a blank line
func withFindings(result, key, text string) string {
	parsed, _ := pyjson.Loads(result)
	if obj, ok := pyjson.AsObject(parsed); ok {
		obj.Set(key, text)
		if out, err := pyjson.Dumps(obj, pyjson.EnsureASCII(false)); err == nil {
			return out
		}
	}
	return result + "\n\n" + text
}

// The additionalContext string of a Claude hook answer, empty when absent
func additionalContext(output *pyjson.Object) string {
	if output == nil {
		return ""
	}
	specific, _ := output.Get("hookSpecificOutput")
	obj, ok := pyjson.AsObject(specific)
	if !ok {
		return ""
	}
	v, _ := obj.Get("additionalContext")
	s, _ := v.(string)
	return s
}

// Runs a hook command with the event on stdin and returns its stdout parsed as an object, nil when silent
func runHookCommand(ctx context.Context, cmd *exec.Cmd, event *pyjson.Object) (*pyjson.Object, error) {
	payload, err := pyjson.Dumps(event)
	if err != nil {
		return nil, err
	}
	var stdout bytes.Buffer
	cmd.Stdin = strings.NewReader(payload)
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if _, isExit := runErr.(*exec.ExitError); runErr != nil && !isExit {
		return nil, runErr
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return nil, nil
	}
	parsed, err := pyjson.Loads(out)
	if err != nil {
		return nil, err
	}
	obj, _ := pyjson.AsObject(parsed)
	return obj, nil
}

// Session cwd of a plugin context, the process cwd outside a session
func pluginSessionCwd(pctx *plugins.Context) func(context.Context) string {
	return func(ctx context.Context) string {
		cwd := ""
		if pctx != nil {
			cwd, _ = pctx.SessionCwd(ctx)
		}
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		return cwd
	}
}

// The impeccable design detector wired onto transform_tool_result and pre_verify
type designHook struct {
	mu               sync.Mutex
	sessionLaunchers map[string]string
	sessionCwd       func(context.Context) string
	homeDir          func(context.Context) string
	trusted          func(ctx context.Context, root string) bool
	materialized     func() string
	getenv           func(string) string
}

// Hook state over the active home, the skills trust list and the materialized launcher
func newDesignHook(sessionCwd func(context.Context) string) *designHook {
	return &designHook{
		sessionLaunchers: map[string]string{},
		sessionCwd:       sessionCwd,
		homeDir:          home.Dir,
		trusted: func(ctx context.Context, root string) bool {
			return skills.NewScope(home.Dir(ctx), nil).IsProjectRootTrusted(root)
		},
		materialized: func() string {
			t, err := Materialize()
			if err != nil {
				return ""
			}
			return t.Host("impeccable/skills/impeccable/scripts/" + impeccableLauncherName())
		},
		getenv: os.Getenv,
	}
}

func (h *designHook) disabled() bool {
	return hookTruthy[strings.ToLower(strings.TrimSpace(h.getenv("IMPECCABLE_HOOK_DISABLED")))]
}

// Nearest trusted project install above start, then the home install, then the materialized tree
func (h *designHook) findLauncher(ctx context.Context, start string) string {
	rel := filepath.Join("skills", "impeccable", "scripts", impeccableLauncherName())
	current, err := filepath.Abs(start)
	if err != nil {
		current = start
	}
	for {
		candidate := filepath.Join(current, ".icebath", rel)
		if isFile(candidate) && h.trusted(ctx, current) {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	if homeLauncher := filepath.Join(h.homeDir(ctx), rel); isFile(homeLauncher) {
		return homeLauncher
	}
	if m := h.materialized(); m != "" && isFile(m) {
		return m
	}
	return ""
}

// Runs the launcher hook verb with the event, the materialized launcher through the Go engine resolver
func (h *designHook) runHook(ctx context.Context, launcher, cwd string, event *pyjson.Object, timeout time.Duration) (*pyjson.Object, error) {
	if launcher == "" || h.disabled() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if m := h.materialized(); m != "" && launcher == m {
		engine, err := EngineCommand(ctx, filepath.Dir(launcher), "hook")
		if err != nil {
			return nil, err
		}
		cmd = engine
	} else {
		cmd = exec.CommandContext(ctx, launcher, "hook")
		cmd.Env = os.Environ()
	}
	cmd.Dir = cwd
	cmd.Env = append(cmd.Env, "IMPECCABLE_HOOK_HARNESS=hermes")
	return runHookCommand(ctx, cmd, event)
}

// Adds the per edit findings to a successful write_file or patch result
func (h *designHook) transformToolResult(ctx context.Context, kw map[string]any) (any, error) {
	toolName, _ := kw["tool_name"].(string)
	result, isStr := kw["result"].(string)
	if hookToolNames[toolName] == "" || !isStr {
		return nil, nil
	}
	sessionID, _ := kw["session_id"].(string)
	cwd := h.sessionCwd(ctx)
	var texts []string
	for _, p := range editedPaths(result, kw["args"], cwd) {
		event := pyjson.ObjectOf("hook_event_name", "PostToolUse", "cwd", cwd, "tool_name", hookToolNames[toolName],
			"tool_input", pyjson.ObjectOf("file_path", p))
		if sessionID != "" {
			event.Set("session_id", sessionID)
		}
		launcher := h.findLauncher(ctx, filepath.Dir(p))
		if launcher != "" && sessionID != "" {
			h.mu.Lock()
			h.sessionLaunchers[sessionID] = launcher
			h.mu.Unlock()
		}
		out, err := h.runHook(ctx, launcher, cwd, event, impeccableEditTimeout)
		if err != nil {
			slog.Debug("impeccable-design-hook: edit hook skipped for " + p + ": " + err.Error())
			continue
		}
		if t := additionalContext(out); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return nil, nil
	}
	return withFindings(result, "impeccable", strings.Join(texts, "\n\n")), nil
}

// Runs the Stop deep pass, findings keeping the turn going and silence letting it finish
func (h *designHook) preVerify(ctx context.Context, kw map[string]any) (any, error) {
	sessionID, _ := kw["session_id"].(string)
	cwd := h.sessionCwd(ctx)
	event := pyjson.ObjectOf("hook_event_name", "Stop", "cwd", cwd, "stop_hook_active", false)
	if sessionID != "" {
		event.Set("session_id", sessionID)
	}
	h.mu.Lock()
	launcher := h.sessionLaunchers[sessionID]
	h.mu.Unlock()
	if launcher == "" {
		launcher = h.findLauncher(ctx, cwd)
	}
	out, err := h.runHook(ctx, launcher, cwd, event, impeccableStopTimeout)
	if err != nil {
		slog.Debug("impeccable-design-hook: stop hook skipped: " + err.Error())
		return nil, nil
	}
	if out == nil || out.Len() == 0 {
		return nil, nil
	}
	decision, _ := out.Get("decision")
	reason, _ := out.Get("reason")
	if r, ok := reason.(string); ok && decision == "block" && r != "" {
		return map[string]any{"decision": "block", "reason": r}, nil
	}
	if text := additionalContext(out); text != "" {
		return map[string]any{"action": "continue", "message": text}, nil
	}
	return nil, nil
}

// The hallmark edit time lint hook install-hook.mjs installs, on transform_tool_result
type hallmarkHook struct {
	sessionCwd func(context.Context) string
	command    func(ctx context.Context, script string, args ...string) (*exec.Cmd, error)
	installed  func(ctx context.Context, cwd string) bool
}

// Adds the Hallmark Floor failures lint-hook.mjs reports to a successful write_file or patch result
func (h *hallmarkHook) transformToolResult(ctx context.Context, kw map[string]any) (any, error) {
	toolName, _ := kw["tool_name"].(string)
	result, isStr := kw["result"].(string)
	if hookToolNames[toolName] == "" || !isStr {
		return nil, nil
	}
	sessionID, _ := kw["session_id"].(string)
	cwd := h.sessionCwd(ctx)
	if !h.installed(ctx, cwd) {
		return nil, nil
	}
	var texts []string
	for _, p := range editedPaths(result, kw["args"], cwd) {
		event := pyjson.ObjectOf("hook_event_name", "PostToolUse", "cwd", cwd, "tool_name", hookToolNames[toolName],
			"tool_input", pyjson.ObjectOf("file_path", p))
		if sessionID != "" {
			event.Set("session_id", sessionID)
		}
		out, err := func() (*pyjson.Object, error) {
			runCtx, cancel := context.WithTimeout(ctx, hallmarkLintTimeout)
			defer cancel()
			cmd, err := h.command(runCtx, "lint-hook")
			if err != nil {
				return nil, err
			}
			cmd.Dir = cwd
			return runHookCommand(runCtx, cmd, event)
		}()
		if err != nil {
			slog.Debug("hallmark: lint hook skipped for " + p + ": " + err.Error())
			continue
		}
		if t := additionalContext(out); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return nil, nil
	}
	return withFindings(result, "hallmark", strings.Join(texts, "\n\n")), nil
}
