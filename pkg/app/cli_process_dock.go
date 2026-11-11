package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds a finished process stays on the dock before it leaves
const processDockRetainSeconds = 60

// Dock status of a finished process by its completion reason
var processStatusByReason = map[string]string{"killed": "killed", "lost": "lost", "failed_start": "failed"}

// Glyph of each process dock status
var processDockGlyph = map[string]string{"running": "⚙", "done": "✔", "failed": "✘", "killed": "✘", "lost": "?"}

// Catalog key of the exit verdict of each finished status
var processVerdictKeys = map[string]string{"done": "cli.dock.exit_code", "failed": "cli.dock.exit_code",
	"killed": "cli.dock.verdict_killed", "lost": "cli.dock.verdict_lost"}

// Last non blank line of an output buffer with its whitespace collapsed
func lastOutputLine(buffer string) string {
	lines := pystr.Splitlines(buffer, false)
	for i := len(lines) - 1; i >= 0; i-- {
		if text := strings.Join(pystr.SplitWhitespace(lines[i], -1), " "); text != "" {
			return text
		}
	}
	return ""
}

// Running, killed, lost, failed or done from a process registry entry
func processStatus(entry map[string]any) string {
	if entry["status"] != "exited" {
		return "running"
	}
	reason, _ := entry["completion_reason"].(string)
	if status, ok := processStatusByReason[reason]; ok {
		return status
	}
	if !pyrepr.Truthy(entry["exit_code"]) {
		return "done"
	}
	return "failed"
}

// Running processes plus those exited within the retain window, running ones longest first
func processRows(ctx context.Context, env *tools.Env, now float64) []map[string]any {
	registry := tools.DefaultProcessRegistry()
	var rows []map[string]any
	for _, entry := range registry.ListSessions(ctx, env, "", "", false) {
		id, _ := entry["session_id"].(string)
		proc := registry.Get(env, id)
		if proc == nil {
			continue
		}
		f := proc.DockFields()
		status := processStatus(entry)
		exitedAt := 0.0
		if status != "running" {
			exitedAt = f.ExitedAt
		}
		if status != "running" && now-exitedAt > processDockRetainSeconds {
			continue
		}
		end := exitedAt
		if end == 0 {
			end = now
		}
		command := strings.Join(pystr.SplitWhitespace(f.Command, -1), " ")
		if command == "" {
			command = i18n.T(ctx, "cli.dock.process_goal_label", "", nil)
		}
		sinceExit := 0
		if exitedAt != 0 {
			sinceExit = max(0, int(now-exitedAt))
		}
		var exitCode any
		if f.ExitCode != nil {
			exitCode = *f.ExitCode
		}
		rows = append(rows, map[string]any{"kind": "process", "key": f.ID, "id": f.ID, "command": command, "status": status,
			"exit_code": exitCode, "elapsed": max(0, int(end-f.StartedAt)), "since_exit": sinceExit, "detail": lastOutputLine(f.OutputBuffer)})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := rows[i]["status"] != "running", rows[j]["status"] != "running"
		if ri != rj {
			return !ri
		}
		if !ri {
			return rows[i]["elapsed"].(int) > rows[j]["elapsed"].(int)
		}
		return rows[i]["since_exit"].(int) < rows[j]["since_exit"].(int)
	})
	return rows
}

// Elapsed time and latest output while running, the exit verdict and its age once finished
func processActivity(ctx context.Context, row map[string]any) string {
	if row["status"] == "running" {
		if detail, _ := row["detail"].(string); detail != "" {
			return fmt.Sprintf("%vs · ", row["elapsed"]) + i18n.T(ctx, "cli.dock.last_output", "", map[string]any{"detail": detail})
		}
		return fmt.Sprintf("%vs · ", row["elapsed"]) + i18n.T(ctx, "cli.dock.starting", "", nil)
	}
	status, _ := row["status"].(string)
	verdict := i18n.T(ctx, processVerdictKeys[status], "", map[string]any{"code": row["exit_code"]})
	return i18n.T(ctx, "cli.dock.verdict_ago", "", map[string]any{"verdict": verdict, "seconds": row["since_exit"]})
}

// Status glyph of a process row
func processGlyph(row map[string]any) string {
	status, _ := row["status"].(string)
	if g, ok := processDockGlyph[status]; ok {
		return g
	}
	return "?"
}

// Printable last 32KiB of a process's output
func processTail(ctx context.Context, env *tools.Env, processID string) string {
	proc := tools.DefaultProcessRegistry().Get(env, processID)
	if proc == nil {
		return i18n.T(ctx, "cli.dock.process_gone", "", nil)
	}
	text := []rune(proc.DockFields().OutputBuffer)
	if len(text) > 32768 {
		text = text[len(text)-32768:]
	}
	var b strings.Builder
	for _, r := range text {
		if pystr.IsPrintable(r) || r == '\n' || r == '\t' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return i18n.T(ctx, "cli.dock.no_output", "", nil)
	}
	return b.String()
}

// Kills a dock process through the registry and returns its result
func killDockProcess(ctx context.Context, env *tools.Env, processID string) map[string]any {
	return tools.DefaultProcessRegistry().KillProcess(ctx, env, processID, "process.kill", true)
}

// Wall clock seconds as a float
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }
