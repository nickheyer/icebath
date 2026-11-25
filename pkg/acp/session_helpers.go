package acp

import (
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

var mntDrive = regexp.MustCompile(`^/mnt/[A-Za-z]/`)

// Windows editor cwd translated to its WSL form when the agent runs in WSL, unchanged elsewhere
func translateACPCwd(cwd string) string { return home.TranslateCwdForWSLBackend(cwd) }

// Canonical cwd for comparisons, user expanded, WSL mapped, drive lowercased and symlinks resolved
func normalizeCwdForCompare(cwd string) string {
	raw := strings.TrimSpace(cwd)
	if raw == "" {
		raw = "."
	}
	expanded := pathutil.ExpandUser(raw)
	if translated, ok := home.WindowsPathToWSL(expanded); ok {
		expanded = translated
	} else if mntDrive.MatchString(expanded) {
		expanded = "/mnt/" + strings.ToLower(expanded[5:6]) + "/" + expanded[7:]
	}
	if real, err := pathutil.Resolve(expanded); err == nil {
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
	for _, s := range []string{strings.TrimSpace(anyutil.String(anyutil.Or(title, ""))), strings.TrimSpace(anyutil.String(anyutil.Or(preview, ""))), leaf} {
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
	f, ok := floatNumber(value)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return timeutil.FromEpoch(f).UTC().Format(timeutil.RFC3339Micro)
}

func floatNumber(value any) (float64, bool) {
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
		f, _ := floatNumber(value)
		return f
	}
	raw := ""
	if value != nil {
		raw = strings.TrimSpace(anyutil.String(value))
	}
	if raw == "" {
		return math.Inf(-1)
	}
	if t, aware, err := timeutil.ParseISOZone(raw, time.UTC); err == nil {
		if !aware {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
		}
		return timeutil.Epoch(t)
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	return math.Inf(-1)
}

// Decoded model_config JSON blob, empty when absent, invalid or not a dict
func parseModelConfig(mc any) *jsonutil.Object {
	s, ok := mc.(string)
	if !ok || s == "" {
		return jsonutil.NewObject()
	}
	v, err := jsonutil.Decode(s)
	if obj, isObj := v.(*jsonutil.Object); err == nil && isObj {
		return obj
	}
	return jsonutil.NewObject()
}

// Session listing row of id, cwd, model, history length, title and updated at
func sessionInfo(sid, cwd string, model any, historyLen int, title, preview, updatedAt any) *jsonutil.Object {
	o := jsonutil.NewObject()
	o.Set("session_id", sid)
	o.Set("cwd", cwd)
	o.Set("model", model)
	o.Set("history_len", int64(historyLen))
	o.Set("title", buildSessionTitle(title, preview, cwd))
	o.Set("updated_at", formatUpdatedAt(updatedAt))
	return o
}
