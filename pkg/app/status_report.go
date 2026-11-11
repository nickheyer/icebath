package app

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const statusStamp = "%Y-%m-%d %H:%M"

const unknownStatusModel = "(unknown)"

const unknownStatusProvider = "unknown"

var lastActivityFields = []string{"updated_at", "last_updated_at", "last_activity_at"}

var statusLabels = map[string]string{
	"session_id": "Session ID", "path": "Path", "title": "Title", "model": "Model", "created": "Created",
	"last_activity": "Last Activity", "tokens": "Tokens", "agent_running": "Agent Running",
}

// Local time of a unix stamp value, the default when it is missing or unparseable
func timestampOr(value any, def *time.Time) *time.Time {
	if !cliTruthy(value) {
		return def
	}
	var f float64
	switch v := value.(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case string:
		parsed, err := strconv.ParseFloat(pystr.Strip(v), 64)
		if err != nil {
			return def
		}
		f = parsed
	default:
		return def
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return def
	}
	sec, frac := math.Modf(f)
	stamp := time.Unix(int64(sec), int64(math.Round(frac*1e9))).Local()
	return &stamp
}

// Created and last activity times of a session row, last activity falling back to created
func sessionTimestamps(meta map[string]any, createdFallback *time.Time) (time.Time, time.Time) {
	created := time.Now()
	if c := timestampOr(meta["started_at"], createdFallback); c != nil {
		created = *c
	}
	for _, field := range lastActivityFields {
		if stamp := timestampOr(meta[field], nil); stamp != nil {
			return created, *stamp
		}
	}
	return created, created
}

// statusFieldsInput carries the surface fallbacks of the shared status field set
type statusFieldsInput struct {
	ctx             context.Context
	sessionID       string
	agentModel      string
	agentProvider   string
	agentTokens     int
	meta            map[string]any
	model           string
	provider        string
	createdFallback *time.Time
	agentRunning    bool
}

// The shared status facts preformatted for display, the agent winning for model, provider and tokens
func buildStatusFields(in statusFieldsInput) map[string]any {
	meta := in.meta
	if meta == nil {
		meta = map[string]any{}
	}
	created, lastActivity := sessionTimestamps(meta, in.createdFallback)
	title := ""
	if s, ok := meta["title"].(string); ok {
		title = pystr.Strip(s)
	}
	return map[string]any{
		"session_id":    in.sessionID,
		"path":          home.Display(home.Dir(in.ctx)),
		"title":         title,
		"model":         firstNonEmpty(in.agentModel, in.model),
		"provider":      firstNonEmpty(in.agentProvider, in.provider),
		"created":       pydatetime.Strftime(created, statusStamp),
		"last_activity": pydatetime.Strftime(lastActivity, statusStamp),
		"tokens":        pystr.GroupThousands(int64(in.agentTokens)),
		"agent_running": in.agentRunning,
	}
}

// Label value lines for the keys in order, an empty title skipped and an unresolved route shown as placeholders
func statusLines(fields map[string]any, keys ...string) []string {
	var lines []string
	for _, key := range keys {
		value := fields[key]
		var text string
		switch key {
		case "title":
			if value == "" {
				continue
			}
			text = cliStr(value, "")
		case "model":
			text = firstNonEmpty(cliStr(fields["model"], ""), unknownStatusModel) + " (" + firstNonEmpty(cliStr(fields["provider"], ""), unknownStatusProvider) + ")"
		case "agent_running":
			text = "No"
			if b, _ := value.(bool); b {
				text = "Yes"
			}
		default:
			text = cliStr(value, "")
		}
		lines = append(lines, statusLabels[key]+": "+text)
	}
	return lines
}
