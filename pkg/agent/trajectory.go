package agent

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func trajectoryLog() *slog.Logger { return slog.Default().With("logger", "agent.trajectory") }

// Wall clock the trajectory timestamp reads
var trajectoryNow = time.Now

// REASONING_SCRATCHPAD tags rewritten as think tags
func ConvertScratchpadToThink(content string) string {
	if content == "" || !strings.Contains(content, "<REASONING_SCRATCHPAD>") {
		return content
	}
	return strings.ReplaceAll(strings.ReplaceAll(content, "<REASONING_SCRATCHPAD>", "<think>"), "</REASONING_SCRATCHPAD>", "</think>")
}

// True when an opening REASONING_SCRATCHPAD tag has no closing tag
func HasIncompleteScratchpad(content string) bool {
	return content != "" && strings.Contains(content, "<REASONING_SCRATCHPAD>") && !strings.Contains(content, "</REASONING_SCRATCHPAD>")
}

// Naive local isoformat with microseconds only when nonzero
func naiveISOFormat(t time.Time) string {
	base := t.Format("2006-01-02T15:04:05")
	if micro := t.Nanosecond() / 1000; micro != 0 {
		base += fmt.Sprintf(".%06d", micro)
	}
	return base
}

// Appends one ShareGPT entry to the JSONL file under an exclusive file lock, logging instead of failing
func SaveTrajectory(trajectory []any, model string, completed bool, filename string) {
	if filename == "" {
		filename = "failed_trajectories.jsonl"
		if completed {
			filename = "trajectory_samples.jsonl"
		}
	}
	entry := pyjson.NewObject()
	entry.Set("conversations", trajectory)
	entry.Set("timestamp", naiveISOFormat(trajectoryNow()))
	entry.Set("model", model)
	entry.Set("completed", completed)
	if err := appendTrajectoryLine(entry, filename); err != nil {
		trajectoryLog().Warn(fmt.Sprintf("Failed to save trajectory: %s", err))
		return
	}
	trajectoryLog().Info(fmt.Sprintf("Trajectory saved to %s", filename))
}

func appendTrajectoryLine(entry *pyjson.Object, filename string) (err error) {
	line, err := pyjson.Dumps(entry, pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if err := lockAppendHandle(f, true); err != nil {
		return err
	}
	defer func() {
		if uerr := lockAppendHandle(f, false); err == nil {
			err = uerr
		}
	}()
	_, err = f.WriteString(line + "\n")
	return err
}
