package app

import (
	"cmp"
	"context"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/ansi"

	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// How long a process that ended stays in the dock
const retain = 60 * time.Second

// Docks the session's background processes in its chats
type docks struct{}

func (docks) Views() []toolsapp.View { return nil }

func (docks) Docks(conn *connect.Client) []toolsapp.Dock {
	service := terminalv1connect.NewProcessServiceClient(conn)
	return []toolsapp.Dock{{Load: func(ctx context.Context, session string) (toolsapp.Docked, error) {
		response, err := service.List(ctx, &terminalv1.ListRequest{SessionId: session})
		if err != nil {
			return nil, err
		}
		return &processes{list: response.GetProcesses(), at: time.Now()}, nil
	}}}
}

// Background processes of a session as they were listed at a moment
type processes struct {
	list []*terminalv1.Process
	at   time.Time
}

// One process of the dock: its command, how it stands, how long it ran and since when it ended, and what it last did
type row struct {
	command string
	status  string
	detail  string
	elapsed time.Duration
	since   time.Duration
}

// How a listed process stands: running until it exited, then by how it ended or its exit code
func status(p *terminalv1.Process) string {
	if p.GetStatus() != "exited" {
		return "running"
	}
	switch p.GetCompletionReason() {
	case "failed_start":
		return "failed"
	case "killed", "lost":
		return p.GetCompletionReason()
	}
	if p.GetHasExitCode() && p.GetExitCode() != 0 {
		return "failed"
	}
	return "done"
}

// Text on one line, its runs of blanks made one space
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// Last line of the output that is not blank
func lastLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := oneLine(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// Rows of the processes running and of those that ended within the retain window at the moment, running ones first, the longest running
// and the latest to end first
func (p *processes) rows(now time.Time) []row {
	passed := max(now.Sub(p.at), 0)
	var rows []row
	for _, process := range p.list {
		r := row{command: cmp.Or(oneLine(process.GetCommand()), "background process"), status: status(process)}
		uptime := time.Duration(process.GetUptimeSeconds()) * time.Second
		if r.status == "running" {
			r.elapsed = uptime + passed
			r.detail = "starting"
			if line := lastLine(process.GetOutputTail()); line != "" {
				r.detail = "last: " + line
			}
		} else {
			r.since = time.Duration(process.GetSinceExitSeconds())*time.Second + passed
			if r.since > retain {
				continue
			}
			r.elapsed = uptime
			verdict := r.status
			if r.status != "killed" && r.status != "lost" {
				code := "?"
				if process.GetHasExitCode() {
					code = fmt.Sprint(process.GetExitCode())
				}
				verdict = "exit " + code
			}
			r.detail = fmt.Sprintf("%s · %ds ago", verdict, int(r.since.Seconds()))
		}
		rows = append(rows, r)
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if (a.status == "running") != (b.status == "running") {
			if a.status == "running" {
				return -1
			}
			return 1
		}
		if a.status == "running" {
			return cmp.Compare(b.elapsed, a.elapsed)
		}
		return cmp.Compare(a.since, b.since)
	})
	return rows
}

func (p *processes) Live(now time.Time) bool {
	return len(p.rows(now)) > 0
}

// Head of the processes and a row for each, or one line folded: how many run and what the first did last
func (p *processes) Rows(width, limit int, now time.Time, t theme.Theme, folded bool) []string {
	rows := p.rows(now)
	if len(rows) == 0 {
		return nil
	}
	running := 0
	for _, r := range rows {
		if r.status == "running" {
			running++
		}
	}
	cols := max(width-2, 8)
	head := func(text string) string {
		return "  " + lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render(ansi.Truncate(text, cols, t.Glyphs.Ellipsis))
	}
	if folded {
		summary := fmt.Sprintf("▸ %d done", len(rows))
		if running > 0 {
			summary = fmt.Sprintf("▸ %d procs", running)
		}
		if room := cols - ansi.StringWidth(summary) - 3; room >= 12 {
			summary += " · " + compact(rows[0].detail, room)
		}
		return []string{head(summary)}
	}
	shown := rows
	if limit > 0 && len(rows) > limit {
		shown = rows[:limit]
	}
	var counts []string
	if running > 0 {
		counts = append(counts, fmt.Sprintf("%d running", running))
	}
	if done := len(rows) - running; done > 0 {
		counts = append(counts, fmt.Sprintf("%d done", done))
	}
	text := "▾ Processes · " + strings.Join(counts, " · ")
	if hidden := len(rows) - len(shown); hidden > 0 {
		text += fmt.Sprintf(" · +%d more", hidden)
	}
	out := []string{head(text)}
	for _, r := range shown {
		out = append(out, "  "+line(r, cols, t))
	}
	return out
}

// One process as its glyph, command and activity, how it ended standing in for the activity once it did
func line(r row, width int, t theme.Theme) string {
	glyph, tone := glyph(r.status, t)
	activity := r.detail
	if r.status == "running" {
		activity = duration(r.elapsed) + " · " + r.detail
	}
	command := compact(r.command, max(8, width-ansi.StringWidth(activity)-6))
	ink := func(text string, c color.Color) string { return lipgloss.NewStyle().Foreground(c).Render(text) }
	return ansi.Truncate(ink(glyph+" ", tone)+ink(command, t.Text)+ink(" · "+activity, t.TextDim), width, t.Glyphs.Ellipsis)
}

// Glyph and color of a process's status
func glyph(status string, t theme.Theme) (string, color.Color) {
	switch status {
	case "running":
		return "⚙", t.Accent
	case "done":
		return "✔", t.Alive
	case "failed":
		return "✘", t.Alarm
	case "killed":
		return "✘", t.Pulse
	}
	return "?", t.TextDim
}

// Text on one line cut to the width with an ellipsis
func compact(text string, width int) string {
	text = oneLine(text)
	if ansi.StringWidth(text) > width {
		return ansi.Truncate(text, max(width-1, 0), "") + "…"
	}
	return text
}

// Time a process ran as 45s, 3m or 3m 2s
func duration(d time.Duration) string {
	seconds := max(int(d.Round(time.Second).Seconds()), 0)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds%60 == 0 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
}
