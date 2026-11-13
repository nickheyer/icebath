package tui

import (
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

const processRetainSeconds = 60

var processWhitespaceRe = regexp.MustCompile(`[` + jsWS + `]+`)

// Background processes of the session they belong to, as process.list reports them
type processSnapshot struct {
	Sid       string
	Processes []any
}

// Dock row of one background process, its command, verdict and timings
type processRow struct {
	Command          string
	Detail           string
	ElapsedSeconds   float64
	ID               string
	SinceExitSeconds float64
	Status           string
}

// Replaces the process snapshot only when its session or entries changed
func (s *tuiStore) applyProcessSnapshot(sid string, processes []any) {
	if processes == nil {
		processes = []any{}
	}
	if s.processSnapshot.Sid != sid || !reflect.DeepEqual(s.processSnapshot.Processes, processes) {
		s.processSnapshot = processSnapshot{Sid: sid, Processes: processes}
	}
}

var reasonStatus = map[string]string{"failed_start": "failed", "killed": "killed", "lost": "lost"}

// Status of a process entry, running until it exited and then by its completion reason or exit code
func processStatus(entry map[string]any) string {
	if entry["status"] != "exited" {
		return "running"
	}
	reason, _ := entry["completion_reason"].(string)
	if status, known := reasonStatus[reason]; known {
		return status
	}
	if truthyJS(entry["exit_code"]) {
		return "failed"
	}
	return "done"
}

func collapseProcessText(s string) string {
	return strings.TrimFunc(processWhitespaceRe.ReplaceAllString(s, " "), isJSSpace)
}

// Last non blank line of an output preview with its whitespace collapsed
func lastOutputLine(preview string) string {
	lines := strings.Split(preview, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if text := collapseProcessText(lines[i]); text != "" {
			return text
		}
	}
	return ""
}

// Detail of a row, the last output while running, otherwise how it ended and how long ago
func processVerdict(row processRow, exitCode any) string {
	if row.Status == "running" {
		if row.Detail != "" {
			return "last: " + row.Detail
		}
		return "starting"
	}
	verdict := row.Status
	if row.Status != "killed" && row.Status != "lost" {
		code := "?"
		if exitCode != nil {
			code = jsValueString(exitCode)
		}
		verdict = "exit " + code
	}
	return verdict + " · " + jsString(row.SinceExitSeconds) + "s ago"
}

func entryNumber(entry map[string]any, k string) float64 {
	if v, isNum := entry[k].(float64); isNum {
		return v
	}
	return 0
}

// Rows of running processes and those that exited within the retain window, running ones first
func buildProcessRows(processes []any, nowMs float64) []processRow {
	nowS := nowMs / 1000
	rows := []processRow{}
	for _, raw := range processes {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		status := processStatus(entry)
		exitedAt := 0.0
		if status != "running" {
			exitedAt = entryNumber(entry, "exited_at")
		}
		sinceExit := 0.0
		if exitedAt != 0 && !math.IsNaN(exitedAt) {
			sinceExit = math.Max(0, math.Floor(nowS-exitedAt))
		}
		if status != "running" && (exitedAt == 0 || math.IsNaN(exitedAt) || sinceExit > processRetainSeconds) {
			continue
		}
		command, _ := entry["command"].(string)
		command = collapseProcessText(command)
		if command == "" {
			command = "background process"
		}
		preview, _ := entry["output_preview"].(string)
		id, _ := entry["session_id"].(string)
		row := processRow{
			Command:          command,
			Detail:           lastOutputLine(preview),
			ElapsedSeconds:   math.Max(0, entryNumber(entry, "uptime_seconds")) - sinceExit,
			ID:               id,
			SinceExitSeconds: sinceExit,
			Status:           status,
		}
		row.Detail = processVerdict(row, entry["exit_code"])
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.Status == "running") != (b.Status == "running") {
			return a.Status == "running"
		}
		if a.Status == "running" {
			return b.ElapsedSeconds-a.ElapsedSeconds < 0
		}
		return a.SinceExitSeconds-b.SinceExitSeconds < 0
	})
	return rows
}

// Process rows of the current session at a moment
func (s *tuiStore) currentProcessRows(nowMs float64) []processRow {
	var processes []any
	if s.processSnapshot.Sid == s.ui.Sid {
		processes = s.processSnapshot.Processes
	}
	return buildProcessRows(processes, nowMs)
}

// Glyph and theme tone of a process status in the dock
func processGlyph(status string, t theme) (glyph, color string) {
	switch status {
	case "running":
		return "⚙", t.Color.Accent
	case "done":
		return "✔", t.Color.StatusGood
	case "failed":
		return "✘", t.Color.Error
	case "killed":
		return "✘", t.Color.Warn
	}
	return "?", t.Color.Muted
}
