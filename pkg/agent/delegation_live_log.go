package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	liveRetentionDays          = 7
	liveAssistantMax           = 600
	liveThinkingMax            = 300
	liveArgsMax                = 220
	liveResultMax              = 400
	liveKickoffMax             = 500
	liveStreamBufferFlushChars = 4000
	liveTimeFormat             = "2006-01-02 15:04:05"
)

func liveLogger() *slog.Logger { return slog.Default().With("logger", "tools.delegation_live_log") }

// Live transcript root under the given profile home, the ambient home when empty
func liveTranscriptRoot(ctx context.Context, h string) string {
	if h == "" {
		h = home.Dir(ctx)
	}
	return filepath.Join(home.HermesDir(h, "cache/delegation", "delegation_cache"), "live")
}

// Runs one transcript step and debug logs any failure so nothing reaches the agent loop
func liveBestEffort(what string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			liveLogger().Debug(fmt.Sprintf("Live transcript %s failed: %v", what, r))
		}
	}()
	if err := fn(); err != nil {
		liveLogger().Debug(fmt.Sprintf("Live transcript %s failed: %s", what, err))
	}
}

// Python str of a value with falsy values reading as empty
func liveText(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Single line of the text truncated to the limit with an elided character count
func oneLine(text any, limit int) string {
	s := strings.Join(pystr.SplitWhitespace(liveText(text), -1), " ")
	if n := pystr.Len(s); n > limit {
		s = pystr.Slice(s, 0, limit) + fmt.Sprintf(" …(+%d chars)", n-limit)
	}
	return s
}

// Credential masked text, forced on whatever the global redaction toggle says
func liveRedact(text string) string {
	if text == "" {
		return text
	}
	return tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true})
}

func liveJoined(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

// Writes or appends UTF-8 text, refusing lone surrogates the way the Python codec does
func writeLiveText(path, text string, flag int) error {
	if err := pystr.StrictUTF8EncodeError(text); err != nil {
		return err
	}
	f, err := os.OpenFile(path, flag, 0o666)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(text)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func dumpLiveJSON(path string, payload any) error {
	text, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	return writeLiveText(path, text, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
}

// Append only event log of one subagent task whose first write failure silences later writes
type liveTranscriptWriter struct {
	stateMu   sync.Mutex
	fileMu    sync.Mutex
	ok        bool
	streamBuf []string
	streamLen int
	path      string
}

// Pre-creates the task log with its header and kickoff line, a pathless writer when creation fails
func newLiveTranscriptWriter(ctx context.Context, delegationID string, taskIndex int, goal string, taskContext any, root string) *liveTranscriptWriter {
	w := &liveTranscriptWriter{}
	liveBestEffort(fmt.Sprintf("init (%s task %d)", delegationID, taskIndex), func() error {
		goalLine := oneLine(goal, liveKickoffMax)
		if root == "" {
			root = liveTranscriptRoot(ctx, "")
		}
		d := filepath.Join(root, delegationID)
		if err := os.MkdirAll(d, 0o777); err != nil {
			return err
		}
		path := filepath.Join(d, fmt.Sprintf("task-%d.log", taskIndex))
		header := "=== Icebath subagent live transcript ===\n" +
			fmt.Sprintf("delegation: %s   task: %d\n", delegationID, taskIndex) +
			fmt.Sprintf("goal: %s\n", liveRedact(goalLine)) +
			fmt.Sprintf("started: %s\n", time.Now().Format(liveTimeFormat)) +
			"(append-only; streams while the subagent runs — tail -f me)\n" +
			strings.Repeat("=", 40) + "\n"
		if err := writeLiveText(path, header, os.O_WRONLY|os.O_CREATE|os.O_TRUNC); err != nil {
			return err
		}
		w.stateMu.Lock()
		w.path, w.ok = path, true
		w.stateMu.Unlock()
		kickoff := "kickoff: " + goalLine
		if pyTruthy(taskContext) {
			kickoff += " | context: " + oneLine(taskContext, liveKickoffMax)
		}
		w.event("user", kickoff)
		return nil
	})
	return w
}

// Path of the task log, empty when the writer could not create it
func (w *liveTranscriptWriter) Path() string {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	return w.path
}

// Appends one timestamped role line through the single redaction choke point
func (w *liveTranscriptWriter) event(role, text string) {
	w.stateMu.Lock()
	ok, path := w.ok, w.path
	w.stateMu.Unlock()
	if !ok || path == "" {
		return
	}
	line := fmt.Sprintf("%s %s| %s\n", time.Now().Format("15:04:05"), pystr.Ljust(role, 9), liveRedact(text))
	w.fileMu.Lock()
	err := writeLiveText(path, line, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	w.fileMu.Unlock()
	if err != nil {
		w.stateMu.Lock()
		w.ok = false
		w.stateMu.Unlock()
		liveLogger().Debug(fmt.Sprintf("Live transcript write failed (%s): %s", path, err))
	}
}

func (w *liveTranscriptWriter) line(role, text string, limit int) {
	if t := oneLine(text, limit); t != "" {
		w.event(role, t)
	}
}

func (w *liveTranscriptWriter) assistantText(text string) {
	w.line("assistant", text, liveAssistantMax)
}

func (w *liveTranscriptWriter) thinking(text string) { w.line("think", text, liveThinkingMax) }

// Logs a tool call start after flushing any buffered streamed reply
func (w *liveTranscriptWriter) toolStart(name string, argsPreview any) {
	w.flushStream()
	if name == "" {
		name = "?"
	}
	w.event("tool", fmt.Sprintf("-> %s(%s)", name, oneLine(argsPreview, liveArgsMax)))
}

// Seconds suffix of a duration value, empty when it is absent or not a float
func liveDuration(duration any) string {
	if duration == nil {
		return ""
	}
	f, err := pyint.Float(duration)
	if err != nil {
		return ""
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return " " + pyrepr.Float(f) + "s"
	}
	text, err := pyrepr.Format(f, ".1f")
	if err != nil {
		return ""
	}
	return " " + text + "s"
}

// Logs a tool result with its status and duration
func (w *liveTranscriptWriter) toolResult(name string, result, duration any, isError bool) {
	status := "ok"
	if isError {
		status = "ERROR"
	}
	if name == "" {
		name = "?"
	}
	w.event("result", fmt.Sprintf("%s %s%s: %s", name, status, liveDuration(duration), oneLine(result, liveResultMax)))
}

// Lifecycle marker line after flushing any buffered streamed reply
func (w *liveTranscriptWriter) marker(text string) {
	w.flushStream()
	w.event("final", oneLine(text, liveAssistantMax))
}

// Buffers streamed reply text, flushing once the buffer reaches its cap
func (w *liveTranscriptWriter) addStreamDelta(delta string) {
	w.stateMu.Lock()
	if delta == "" || !w.ok {
		w.stateMu.Unlock()
		return
	}
	w.streamBuf = append(w.streamBuf, delta)
	w.streamLen += pystr.Len(delta)
	full := w.streamLen >= liveStreamBufferFlushChars
	w.stateMu.Unlock()
	if full {
		w.flushStream()
	}
}

// Writes the buffered streamed reply as one assistant line
func (w *liveTranscriptWriter) flushStream() {
	w.stateMu.Lock()
	if len(w.streamBuf) == 0 {
		w.stateMu.Unlock()
		return
	}
	text := strings.Join(w.streamBuf, "")
	w.streamBuf, w.streamLen = nil, 0
	w.stateMu.Unlock()
	w.assistantText(text)
}

// Completion marker with status, duration and summary
func (w *liveTranscriptWriter) onComplete(preview any, kwargs map[string]any) {
	dur := kwargs["duration_seconds"]
	summary := kwargs["summary"]
	if !pyTruthy(summary) {
		summary = preview
	}
	status, ok := kwargs["status"]
	if !ok {
		status = "?"
	}
	durPart, summaryPart := "", ""
	if dur != nil {
		durPart = "duration=" + pyrepr.Str(dur) + "s"
	}
	if pyTruthy(summary) {
		summaryPart = "summary: " + oneLine(summary, liveResultMax)
	}
	w.marker(liveJoined("status="+pyrepr.Str(status), durPart, summaryPart))
}

// Maps one child progress event onto transcript lines, ignoring unknown events
func (w *liveTranscriptWriter) observe(eventType any, toolName, preview, args any, kwargs map[string]any) {
	switch liveText(eventType) {
	case "tool.started":
		argsPreview := preview
		if !pyTruthy(preview) {
			argsPreview = args
		}
		w.toolStart(liveText(toolName), argsPreview)
	case "tool.completed":
		w.toolResult(liveText(toolName), kwargs["result"], kwargs["duration"], pyTruthy(kwargs["is_error"]))
	case "_thinking":
		text := toolName
		if !pyTruthy(text) {
			text = preview
		}
		w.thinking(liveText(text))
	case "reasoning.available":
		w.thinking(liveText(preview))
	case "subagent.text":
		w.addStreamDelta(liveText(preview))
	case "subagent.start":
		w.event("start", oneLine(preview, liveKickoffMax))
	case "subagent.complete":
		w.onComplete(preview, kwargs)
	}
}

// Terminal marker carrying the exit reason detail the completion event lacks
func (w *liveTranscriptWriter) Finalize(entry *pyjson.Object) error {
	get := func(key string) any {
		v, _ := entry.Get(key)
		return v
	}
	status, ok := entry.Get("status")
	if !ok {
		status = "?"
	}
	exitReason := get("exit_reason")
	exitPart, budgetPart, errorPart := "", "", ""
	if pyTruthy(exitReason) {
		exitPart = "exit_reason=" + pyrepr.Str(exitReason)
	}
	if exitReason == "max_iterations" {
		budgetPart = "(iteration budget exhausted)"
	}
	if errVal := get("error"); pyTruthy(errVal) {
		errorPart = "error: " + oneLine(errVal, liveResultMax)
	}
	w.marker(liveJoined("end status="+pyrepr.Str(status), exitPart, budgetPart, errorPart))
	return nil
}

// Progress callback carrying a flush hook, the callable with a _flush attribute
type flushingProgressCallback struct {
	call  any
	flush func()
}

func (c *flushingProgressCallback) progressFunc() any { return c.call }

func (c *flushingProgressCallback) Flush() { c.flush() }

// Callable surface of a progress callback, unwrapping one that carries a flush hook
func progressFn(cb any) any {
	if w, ok := cb.(interface{ progressFunc() any }); ok {
		return w.progressFunc()
	}
	return cb
}

// Runs the flush hook of a progress callback when it carries one
func flushProgressCallback(cb any) {
	if f, ok := cb.(interface{ Flush() }); ok {
		f.Flush()
	}
}

// Wraps a child progress callback so every event also lands in the live log and flush reaches both
func wrapProgressCallback(inner any, writer delegationLiveWriter) any {
	observe := func(event, name string, preview, args any, extra map[string]any) {
		liveBestEffort("observe", func() error {
			writer.observe(event, name, preview, args, extra)
			return nil
		})
	}
	flush := func() {
		liveBestEffort("flush", func() error {
			writer.flushStream()
			return nil
		})
		flushProgressCallback(inner)
	}
	var call any
	switch cb := progressFn(inner).(type) {
	case codexProgressFn:
		call = func(event, name string, preview any, args any, extra *provider.CodexToolProgressExtra) {
			observe(event, name, preview, args, toolProgressExtra(extra))
			cb(event, name, preview, args, extra)
		}
	case func(event, name string, preview any, args any, extra map[string]any):
		call = func(event, name string, preview any, args any, extra map[string]any) {
			observe(event, name, preview, args, extra)
			cb(event, name, preview, args, extra)
		}
	case toolProgressFn:
		call = func(event, name, preview string, args map[string]any, extra map[string]any) {
			observe(event, name, preview, args, extra)
			cb(event, name, preview, args, extra)
		}
	default:
		call = func(event, name, preview string, args map[string]any, extra map[string]any) {
			observe(event, name, preview, args, extra)
		}
	}
	return &flushingProgressCallback{call: call, flush: flush}
}

// Eight hex characters of a random UUID4
func liveDelegationSuffix() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)[:8]
}

// Pre-headered writer per task plus a manifest, an empty id and nil writers when nothing could be created
func createLiveTranscripts(ctx context.Context, taskList []map[string]any, taskContext, model, providerName any, liveHome string) (string, []delegationLiveWriter, []string) {
	n := len(taskList)
	root := liveTranscriptRoot(ctx, liveHome)
	pruneStaleLiveDirs(liveRetentionDays, root)
	delegID := "deleg_" + liveDelegationSuffix()
	writers := make([]delegationLiveWriter, n)
	var paths []string
	for i, t := range taskList {
		goal := ""
		if g, ok := t["goal"]; ok {
			goal = pyrepr.Str(g)
		}
		ctxVal := t["context"]
		if !pyTruthy(ctxVal) {
			ctxVal = taskContext
		}
		w := newLiveTranscriptWriter(ctx, delegID, i, goal, ctxVal, root)
		if p := w.Path(); p != "" {
			writers[i] = w
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return "", make([]delegationLiveWriter, n), nil
	}
	writeLiveManifest(ctx, delegID, taskList, paths, model, providerName, liveHome)
	return delegID, writers, paths
}

func liveManifestPath(ctx context.Context, delegationID, h string) string {
	return filepath.Join(liveTranscriptRoot(ctx, h), delegationID, "manifest.json")
}

// Writes the dispatch manifest naming every task's redacted goal, log and running status
func writeLiveManifest(ctx context.Context, delegationID string, taskList []map[string]any, paths []string, model, providerName any, h string) {
	liveBestEffort("manifest write", func() error {
		tasks := make([]any, 0, len(taskList))
		for i, t := range taskList {
			goal := ""
			if g, ok := t["goal"]; ok {
				goal = pyrepr.Str(g)
			}
			task := pyjson.NewObject()
			task.Set("index", i)
			task.Set("goal", liveRedact(pystr.Slice(goal, 0, 500)))
			if i < len(paths) {
				task.Set("log", paths[i])
			} else {
				task.Set("log", nil)
			}
			task.Set("status", "running")
			tasks = append(tasks, task)
		}
		manifest := pyjson.NewObject()
		manifest.Set("delegation_id", delegationID)
		manifest.Set("started", time.Now().Format(liveTimeFormat))
		manifest.Set("task_count", len(taskList))
		manifest.Set("model", model)
		manifest.Set("provider", providerName)
		manifest.Set("tasks", tasks)
		return dumpLiveJSON(liveManifestPath(ctx, delegationID, h), manifest)
	})
}

// Python dict get on a decoded JSON object, the default when absent or not an object
func liveObjectGet(v any, key string, def any) any {
	if o, ok := v.(*pyjson.Object); ok {
		if got, ok := o.Get(key); ok {
			return got
		}
	}
	return def
}

// Updates each task's manifest status from the aggregated results, stamping completion unless settling early
func updateManifestStatuses(delegationID string, results []*pyjson.Object, liveHome string, completed bool) {
	if delegationID == "" {
		return
	}
	liveBestEffort("manifest update", func() error {
		mp := liveManifestPath(context.Background(), delegationID, liveHome)
		raw, err := os.ReadFile(mp)
		if err != nil {
			return err
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			return err
		}
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return err
		}
		manifest, ok := decoded.(*pyjson.Object)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
		}
		type indexed struct {
			key any
			r   *pyjson.Object
		}
		var byIndex []indexed
		for _, r := range results {
			if r == nil {
				continue
			}
			key, _ := r.Get("task_index")
			replaced := false
			for i := range byIndex {
				if pyrepr.Equal(byIndex[i].key, key) {
					byIndex[i].r, replaced = r, true
				}
			}
			if !replaced {
				byIndex = append(byIndex, indexed{key, r})
			}
		}
		taskList, _ := liveObjectGet(manifest, "tasks", []any{}).([]any)
		for _, rawTask := range taskList {
			task, ok := rawTask.(*pyjson.Object)
			if !ok {
				return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(rawTask))
			}
			idx, _ := task.Get("index")
			var r *pyjson.Object
			for _, e := range byIndex {
				if pyrepr.Equal(e.key, idx) {
					r = e.r
				}
			}
			if r == nil {
				continue
			}
			current, _ := task.Get("status")
			task.Set("status", liveObjectGet(r, "status", current))
			if v, _ := r.Get("exit_reason"); pyTruthy(v) {
				task.Set("exit_reason", v)
			}
			if v, _ := r.Get("model"); pyTruthy(v) {
				task.Set("model", v)
			}
		}
		if !pyTruthy(liveObjectGet(manifest, "model", nil)) {
			var inherited any
			for _, rawTask := range taskList {
				if v := liveObjectGet(rawTask, "model", nil); pyTruthy(v) {
					inherited = v
					break
				}
			}
			manifest.Set("model", inherited)
		}
		if completed {
			manifest.Set("completed", time.Now().Format(liveTimeFormat))
		}
		return dumpLiveJSON(mp, manifest)
	})
}

// Removes delegation dirs older than the retention window under the root and counts them
func pruneStaleLiveDirs(maxAgeDays int, root string) int {
	removed := 0
	liveBestEffort("pruning", func() error {
		if root == "" {
			root = liveTranscriptRoot(context.Background(), "")
		}
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			return nil
		}
		cutoff := time.Now().Add(-time.Duration(maxAgeDays) * 24 * time.Hour)
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, e := range entries {
			child := filepath.Join(root, e.Name())
			st, err := os.Stat(child)
			if err != nil || !st.IsDir() || !st.ModTime().Before(cutoff) {
				continue
			}
			if lst, err := os.Lstat(child); err == nil && lst.Mode()&os.ModeSymlink == 0 {
				_ = os.RemoveAll(child)
			}
			removed++
		}
		return nil
	})
	return removed
}
