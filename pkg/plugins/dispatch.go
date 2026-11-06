package plugins

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
)

const StreamHoldingHook = "transform_llm_output"

var validHooks = map[string]bool{
	"pre_tool_call": true, "post_tool_call": true, "transform_terminal_output": true, "transform_tool_result": true,
	"transform_llm_output": true, "pre_llm_call": true, "post_llm_call": true, "on_stream_start": true,
	"on_stream_delta": true, "on_stream_end": true, "on_interim_message": true, "pre_verify": true,
	"pre_api_request": true, "post_api_request": true, "api_request_error": true, "pre_auxiliary_call": true,
	"post_auxiliary_call": true, "transform_api_error_classification": true, "on_session_start": true,
	"on_session_end": true, "on_session_finalize": true, "on_session_reset": true, "on_session_delete": true,
	"on_skill_lifecycle": true, "subagent_start": true, "subagent_stop": true, "pre_gateway_dispatch": true,
	"post_gateway_admission": true, "agent_loop_stopped": true, "pre_approval_request": true,
	"post_approval_response": true, "on_human_input_request": true, "on_human_input_resolved": true,
	"on_room_member_activity": true, "pre_transcription": true, "kanban_task_claimed": true,
	"kanban_task_completed": true, "kanban_task_blocked": true, "on_kanban_worker_spawned": true,
	"on_kanban_worker_exited": true, "on_kanban_worker_stale_claim": true, "on_kanban_task_updated": true,
	"on_kanban_dispatch_tick": true, "gateway_platform_event": true, "pre_command": true, "post_compaction": true,
}

// Hooks whose directive a shell hook has no channel to return, refused for shell registration
var shellUnsupportedHooks = map[string]bool{"transform_api_error_classification": true}

// Every lifecycle hook name a plugin may register, sorted
func ValidHooks() []string { return sortedSet(validHooks) }

// Hook callback receiving the hook payload fields and returning an optional directive or value
type HookFunc func(ctx context.Context, kwargs map[string]any) (any, error)

// One registered hook callback with its identity and display name
type hookEntry struct {
	fn       HookFunc
	name     string
	module   string
	shellKey *shellHookKey
	hosted   bool
	params   []wireParam
}

// Whether the callback takes arbitrary keyword arguments, a Go callback and an unknown hosted signature always do
func (e *hookEntry) acceptsVarKwargs() bool {
	if !e.hosted || len(e.params) == 0 {
		return true
	}
	for _, p := range e.params {
		if p.Kind == paramVarKeyword {
			return true
		}
	}
	return false
}

// Timeout suppression slot of one callback on one hook
type hookSuppressionKey struct {
	hook string
	cb   *hookEntry
}

// Concurrency gate slot of one call of a callback, distinct per call identity
type hookGateKey struct {
	hookSuppressionKey
	identity    string
	hasIdentity bool
}

// Deduplication key of a reported callback failure
type hookFailureKey struct {
	hook, module, qualname, errType, message string
}

// Qualified and short names of a Go callback
func callbackNames(fn any) (string, string) {
	full := fmt.Sprintf("%T", fn)
	if v := reflect.ValueOf(fn); v.Kind() == reflect.Func && !v.IsNil() {
		if f := runtime.FuncForPC(v.Pointer()); f != nil {
			full = f.Name()
		}
	}
	short := full
	if i := strings.LastIndex(short, "/"); i >= 0 {
		short = short[i+1:]
	}
	if i := strings.Index(short, "."); i >= 0 {
		short = short[i+1:]
	}
	return full, short
}

// Logs one warning per distinct hook, callback and error, identical repeats at debug
func (m *Manager) reportHookFailure(hookName, callbackName string, kwargs map[string]any, err error, surface string) {
	module, qualname := callbackName, callbackName
	if i := strings.LastIndex(callbackName, "."); i >= 0 {
		module, qualname = callbackName[:i], callbackName[i+1:]
	}
	msg := err.Error()
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200])
	}
	key := hookFailureKey{hook: hookName, module: module, qualname: qualname, errType: fmt.Sprintf("%T", err), message: msg}
	m.hookTimeoutMu.Lock()
	seen := m.hookFailuresReported[key]
	m.hookFailuresReported[key] = true
	m.hookTimeoutMu.Unlock()
	if seen {
		logDebug(fmt.Sprintf("%s '%s' callback %s raised again: %v", surface, hookName, callbackName, err))
		return
	}
	fields := make([]string, 0, len(kwargs))
	for k := range kwargs {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	provided := strings.Join(fields, ", ")
	if provided == "" {
		provided = "no fields"
	}
	logWarn(fmt.Sprintf("%s '%s' callback %s raised: %v (%s provides: %s; identical failures are logged at DEBUG from now on)",
		surface, hookName, callbackName, err, strings.ToLower(surface), provided))
}

// Reports whether at least one callback is registered for a hook outside a safe worker
func (m *Manager) HasHook(hookName string) bool {
	if config.SafeWorkerEnabled() {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.hooks[hookName]) > 0
}

// Registers a hook callback for a context, hold_stream honoured only on the output transform hook
func (m *Manager) TrackHook(c *Context, hookName string, fn HookFunc, holdStream bool) *Registration {
	full, name := callbackNames(fn)
	module := full
	if i := strings.LastIndex(module, "."); i >= 0 {
		module = module[:i]
	}
	return m.trackHookAs(c, hookName, fn, holdStream, module, name)
}

// Registers a hook callback under the module and display name that attribute it
func (m *Manager) trackHookAs(c *Context, hookName string, fn HookFunc, holdStream bool, module, name string) *Registration {
	return m.trackHookEntry(c, hookName, &hookEntry{fn: fn, name: name, module: module}, holdStream)
}

// Registers a hosted hook callback keeping its signature for the Plugin Doctor keyword check
func (m *Manager) trackHostedHook(c *Context, hookName string, cb *HostedCallable, holdStream bool) *Registration {
	return m.trackHookEntry(c, hookName, &hookEntry{fn: hostedHook(cb), name: cb.Name, module: cb.Module, hosted: true, params: cb.Sig}, holdStream)
}

// Stores one hook entry, hold_stream honoured only on the output transform hook
func (m *Manager) trackHookEntry(c *Context, hookName string, entry *hookEntry, holdStream bool) *Registration {
	handle := trackCallback(c, "hook", hookName, entry, m.hooks, validHooks)
	if holdStream && hookName != StreamHoldingHook {
		logWarn(fmt.Sprintf("Plugin '%s' set hold_stream on '%s'; only %s rewrites the reply, ignored",
			c.manifest.Name, hookName, StreamHoldingHook))
	} else {
		m.mu.Lock()
		handle.HoldStream = holdStream
		m.mu.Unlock()
	}
	return handle
}

// Reports whether a loaded output transform registration declared hold_stream
func (m *Manager) HoldsStream() bool {
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.holdsStreamLocked()
}

// Reports whether any ledger registration holds the stream, caller holding the lock
func (m *Manager) holdsStreamLocked() bool {
	for _, owned := range m.ownershipLedger {
		for _, r := range owned {
			if r.HoldStream {
				return true
			}
		}
	}
	return false
}

const (
	DefaultSystemPromptSectionMaxChars = 4000
	MaxSystemPromptSectionChars        = 4000
	MaxSystemPromptSections            = 32
	MaxSystemPromptSectionsTotalChars  = 8000
	systemPromptSectionHeadingPrefix   = "## Plugin Context: "
	PluginSectionsStart                = "<!-- icebath-plugin-sections:start -->"
	PluginSectionsEnd                  = "<!-- icebath-plugin-sections:end -->"
)

var (
	SystemPromptSectionPositions = map[string]bool{"after_memory": true}
	systemPromptSectionIDRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

// Plugin owned system prompt section rendered once per new session, from text or a session callback
type SystemPromptSection struct {
	ID          string
	Content     string
	ContentFunc func(sessionInfo map[string]any) (any, error)
	Position    string
	MaxChars    int
	Plugin      string
}

// Validated prompt text frozen on the owning agent
type RenderedSystemPromptSection struct {
	ID       string
	Content  string
	Position string
	Plugin   string
}

// Reports whether a value is a stable heading safe section identifier
func IsValidSystemPromptSectionID(value any) bool {
	s, ok := value.(string)
	return ok && systemPromptSectionIDRe.MatchString(s)
}

// Auditable length framed block recoverable from the full prompt
func FormatSystemPromptSection(sectionID, content string) string {
	return fmt.Sprintf("%s%s\n<!-- icebath-plugin-section-chars:%d -->\n\n%s",
		systemPromptSectionHeadingPrefix, sectionID, utf8.RuneCountInString(content), content)
}

// Canonical container of rendered sections used for persistence recovery, empty for no sections
func FormatSystemPromptSections(sections []RenderedSystemPromptSection) string {
	if len(sections) == 0 {
		return ""
	}
	blocks := make([]string, len(sections))
	for i, s := range sections {
		blocks[i] = FormatSystemPromptSection(s.ID, s.Content)
	}
	return PluginSectionsStart + "\n" + strings.Join(blocks, "\n\n") + "\n" + PluginSectionsEnd
}
