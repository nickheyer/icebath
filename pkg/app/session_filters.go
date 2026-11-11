package app

import (
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	bareDaysRE, _ = pyre.CompileFullmatch(`\d+(?:\.\d+)?`)
	durationRE    = pyre.MustCompile(`^(\d+(?:\.\d+)?)\s*` +
		`(s|sec|secs|second|seconds|` +
		`m|min|mins|minute|minutes|` +
		`h|hr|hrs|hour|hours|` +
		`d|day|days|` +
		`w|wk|wks|week|weeks)$`)
	unitSeconds = map[byte]float64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 604800}
)

// Seconds of a duration like 5h, 2d, 1w or a bare number of days, false for anything else
func parseDurationSeconds(value string) (float64, bool) {
	s := pystr.Lower(pystr.Strip(value))
	if m, _ := bareDaysRE.FindStringMatch(s); m != nil {
		days, err := pyint.ParseFloat(s)
		return days * 86400, err == nil
	}
	m, _ := durationRE.FindStringMatch(s)
	if m == nil {
		return 0, false
	}
	amount, err := pyint.ParseFloat(m.GroupByNumber(1).String())
	if err != nil {
		return 0, false
	}
	return amount * unitSeconds[m.GroupByNumber(2).String()[0]], true
}

// Epoch of a CLI time value, a duration meaning that long ago and an ISO timestamp taken as written
func parsePointInTime(value any, flag string) (float64, error) {
	s := pystr.Strip(pyrepr.Str(value))
	if dur, ok := parseDurationSeconds(s); ok {
		return float64(time.Now().UnixNano())/1e9 - dur, nil
	}
	dt, aware, err := pydatetime.ParseISOFormat(s)
	if err != nil {
		return 0, &pyint.Error{Kind: "ValueError", Msg: "Invalid value for " + flag + ": '" + pyrepr.Str(value) + "'. Use a duration like '5h', " +
			"'30m', '2d', '1w', a bare number of days, or an ISO timestamp " +
			"like '2026-07-05' or '2026-07-05 14:30'."}
	}
	if aware {
		return pydatetime.Timestamp(dt.UTC()), nil
	}
	seconds, ok := pydatetime.LocalToSeconds(dt, time.Local, 0, pydatetime.LocaltimeAccepts)
	if !ok {
		return 0, &pyint.Error{Kind: "OverflowError", Msg: "timestamp out of range for platform time_t"}
	}
	return float64(seconds) + float64(dt.Nanosecond()/1000)/1e6, nil
}

// Short local time of an epoch timestamp, a dash when unset or corrupt
func formatEpoch(ts any) string {
	epoch, ok := pydatetime.CoerceEpoch(ts, "", "timestamp")
	if !ok {
		return "-"
	}
	return pydatetime.Strftime(pydatetime.FromTimestamp(epoch), "%Y-%m-%d %H:%M")
}

// The four epoch bounds as filter key, namespace dest, CLI flag and description template
var timeBounds = [][4]string{
	{"last_active_before", "older_than", "--older-than", "last active before {v}"},
	{"last_active_after", "newer_than", "--newer-than", "last active after {v}"},
	{"started_before", "before", "--before", "started before {v}"},
	{"started_after", "after", "--after", "started after {v}"},
}

// The bound pairs checked for an empty window as lower key, upper key, label, lower flag and upper flag
var filterWindows = [][5]string{
	{"started_after", "started_before", "start-time", "--after", "--before"},
	{"last_active_after", "last_active_before", "activity", "--newer-than", "--older-than"},
}

// The pass through filters as filter key, namespace dest and description template, in describe order
var argFilters = [][3]string{
	{"source", "source", "source '{v}'"},
	{"title_like", "title", "title contains '{v}'"},
	{"end_reason", "end_reason", "end reason '{v}'"},
	{"cwd_prefix", "cwd", "cwd under '{v}'"},
	{"min_messages", "min_messages", ">= {v} messages"},
	{"max_messages", "max_messages", "<= {v} messages"},
	{"model_like", "model", "model contains '{v}'"},
	{"provider", "provider", "provider '{v}'"},
	{"user_id", "user", "user '{v}'"},
	{"chat_id", "chat_id", "chat '{v}'"},
	{"chat_type", "chat_type", "chat type '{v}'"},
	{"branch_like", "branch", "git branch contains '{v}'"},
	{"min_tokens", "min_tokens", ">= {v} tokens"},
	{"max_tokens", "max_tokens", "<= {v} tokens"},
	{"min_cost", "min_cost", ">= ${v}"},
	{"max_cost", "max_cost", "<= ${v}"},
	{"min_tool_calls", "min_tool_calls", ">= {v} tool calls"},
	{"max_tool_calls", "max_tool_calls", "<= {v} tool calls"},
}

// Store filter arguments of the namespace flags, a ValueError for a bad time value or an empty window
func buildPruneFilters(ns map[string]any) (map[string]any, error) {
	filters := map[string]any{"older_than_days": nil}
	bounds := map[string]float64{}
	for _, b := range timeBounds {
		key, raw := b[0], ns[b[1]]
		filters[key] = nil
		if raw == nil {
			continue
		}
		ts, err := parsePointInTime(raw, b[2])
		if err != nil {
			return nil, err
		}
		filters[key], bounds[key] = ts, ts
	}
	for _, w := range filterWindows {
		lo, hasLo := bounds[w[0]]
		hi, hasHi := bounds[w[1]]
		if hasHi && hasLo && lo >= hi {
			return nil, &pyint.Error{Kind: "ValueError", Msg: "Empty " + w[2] + " window: the " + w[3] + " bound " +
				"(" + formatEpoch(lo) + ") is not earlier than the " +
				w[4] + " bound (" + formatEpoch(hi) + ")."}
		}
	}
	for _, f := range argFilters {
		filters[f[0]] = ns[f[1]]
	}
	return filters, nil
}

// Human readable summary of the active filters for previews and confirmation prompts
func describeFilters(filters map[string]any) string {
	var parts []string
	for _, b := range timeBounds {
		if v := filters[b[0]]; v != nil {
			parts = append(parts, strings.ReplaceAll(b[3], "{v}", formatEpoch(v)))
		}
	}
	for _, f := range argFilters {
		v := filters[f[0]]
		active := pyrepr.Truthy(v)
		if strings.HasPrefix(f[0], "min_") || strings.HasPrefix(f[0], "max_") {
			active = v != nil
		}
		if active {
			parts = append(parts, strings.ReplaceAll(f[2], "{v}", pyrepr.Str(v)))
		}
	}
	if len(parts) == 0 {
		return "no filters (all ended sessions)"
	}
	return strings.Join(parts, ", ")
}
