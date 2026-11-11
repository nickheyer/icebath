package acp

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/tools"
)

var sessionLog = logging.GetLogger("acp_adapter.session")

var mntDrive = regexp.MustCompile(`^/mnt/[A-Za-z]/`)

// Windows editor cwd translated to its WSL form when the agent runs in WSL, unchanged elsewhere
func translateACPCwd(cwd string) string { return home.TranslateCwdForWSLBackend(cwd) }

// Canonical cwd for comparisons, user expanded, WSL mapped, drive lowercased and symlinks resolved
func normalizeCwdForCompare(cwd string) string {
	raw := strings.TrimSpace(cwd)
	if raw == "" {
		raw = "."
	}
	expanded := pypath.Expanduser(raw)
	if translated, ok := home.WindowsPathToWSL(expanded); ok {
		expanded = translated
	} else if mntDrive.MatchString(expanded) {
		expanded = "/mnt/" + strings.ToLower(expanded[5:6]) + "/" + expanded[7:]
	}
	if real, err := pypath.Realpath(expanded, pypath.Lax); err == nil {
		return real
	}
	return filepath.Clean(expanded)
}

// Session title from the stored title, the preview, the cwd leaf or New thread
func buildSessionTitle(title, preview any, cwd string) string {
	leaf := filepath.Base(strings.TrimRight(cwd, `/\`))
	if strings.TrimRight(cwd, `/\`) == "" {
		leaf = ""
	}
	for _, s := range []string{strings.TrimSpace(pyStr(pyrepr.Or(title, ""))), strings.TrimSpace(pyStr(pyrepr.Or(preview, ""))), leaf} {
		if s != "" {
			return s
		}
	}
	return "New thread"
}

// Updated at as given for text and nil, an epoch rendered as a UTC isoformat, nil when unparseable
func formatUpdatedAt(value any) any {
	if value == nil {
		return nil
	}
	if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	f, ok := pyFloat(value)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return pydatetime.ISOFormatUTC(pydatetime.FromTimestamp(f))
}

func pyFloat(value any) (float64, bool) {
	switch x := value.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(x, "_", "")), 64)
		return f, err == nil
	}
	return 0, false
}

// Sort key of an updated at value, numbers as is, isoformat or numeric text parsed, minus infinity otherwise
func updatedAtSortKey(value any) float64 {
	switch value.(type) {
	case int64, int, float64, bool:
		f, _ := pyFloat(value)
		return f
	}
	raw := ""
	if value != nil {
		raw = strings.TrimSpace(pyStr(value))
	}
	if raw == "" {
		return math.Inf(-1)
	}
	if t, aware, err := pydatetime.ParseISOFormat(raw); err == nil {
		if !aware {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
		}
		return pydatetime.Timestamp(t)
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	return math.Inf(-1)
}

// Prints incidental agent output to stderr since stdout carries JSON-RPC
func acpStderrPrint(args ...any) {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = pyStr(a)
	}
	fmt.Fprintln(os.Stderr, strings.Join(parts, " "))
}

// Binds a task id to the editor cwd for tools, WSL translated
func registerTaskCwd(ctx context.Context, taskID, cwd string) {
	if taskID == "" {
		return
	}
	if err := tools.RegisterTaskEnvOverrides(nil, taskID, map[string]any{"cwd": translateACPCwd(cwd)}); err != nil {
		sessionLog.Debug("Failed to register ACP task cwd override")
	}
}

// ACP toolsets plus one mcp toolset per selected server, deduplicated in order
func expandACPEnabledToolsets(toolsets []string, useDefault bool, mcpServerNames []string) []string {
	names := toolsets
	if useDefault {
		names = []string{"hermes-acp"}
	}
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range names {
		add(n)
	}
	for _, s := range mcpServerNames {
		if s != "" {
			add("mcp-" + s)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// Decoded model_config JSON blob, empty when absent, invalid or not a dict
func parseModelConfig(mc any) *pyjson.Object {
	s, ok := mc.(string)
	if !ok || s == "" {
		return pyjson.NewObject()
	}
	v, err := pyjson.Loads(s)
	if obj, isObj := v.(*pyjson.Object); err == nil && isObj {
		return obj
	}
	return pyjson.NewObject()
}

// Session listing row of id, cwd, model, history length, title and updated at
func sessionInfo(sid, cwd string, model any, historyLen int, title, preview, updatedAt any) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("session_id", sid)
	o.Set("cwd", cwd)
	o.Set("model", model)
	o.Set("history_len", int64(historyLen))
	o.Set("title", buildSessionTitle(title, preview, cwd))
	o.Set("updated_at", formatUpdatedAt(updatedAt))
	return o
}

// First non blank user message content of a history, def when none
func firstUserPreview(history []map[string]any, def string) string {
	for _, m := range history {
		if m["role"] != "user" {
			continue
		}
		if s := strings.TrimSpace(pyStr(pyrepr.Or(m["content"], ""))); s != "" {
			return s
		}
	}
	return def
}
