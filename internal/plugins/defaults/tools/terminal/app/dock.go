package app

import (
	"cmp"
	"context"
	"image/color"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/ansi"

	terminalapp "github.com/nickheyer/icebath/internal/terminal/app"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/ui/render"
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

// One process of the dock: its command, how it stands, how long it ran and since when it ended, and what it said last
type row struct {
	process *terminalv1.Process
	command string
	status  string
	last    string
	elapsed time.Duration
	since   time.Duration
}

// Rows of the processes running and of those that ended within the retain window at the moment, running ones first, the longest running
// and the latest to end first
func (p *processes) rows(now time.Time) []row {
	passed := max(now.Sub(p.at), 0)
	var rows []row
	for _, process := range p.list {
		r := row{process: process, command: cmp.Or(terminalapp.OneLine(process.GetCommand()), "Background process"), status: terminalapp.Status(process),
			last: terminalapp.LastLine(process.GetOutputTail())}
		uptime := time.Duration(process.GetUptimeSeconds()) * time.Second
		if r.status == terminalapp.Running {
			r.elapsed = uptime + passed
		} else {
			r.since = time.Duration(process.GetSinceExitSeconds())*time.Second + passed
			if r.since > retain {
				continue
			}
			r.elapsed = uptime
		}
		rows = append(rows, r)
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if (a.status == terminalapp.Running) != (b.status == terminalapp.Running) {
			if a.status == terminalapp.Running {
				return -1
			}
			return 1
		}
		if a.status == terminalapp.Running {
			return cmp.Compare(b.elapsed, a.elapsed)
		}
		return cmp.Compare(a.since, b.since)
	})
	return rows
}

func (p *processes) Live(now time.Time) bool {
	return len(p.rows(now)) > 0
}

// What a process is doing, or how it ended: "running for 3 minutes, last said: ready on :3000", "ended 12 seconds ago with exit code 0"
func (r row) activity() string {
	if r.status == terminalapp.Running {
		said := "running for " + terminalapp.Span(r.elapsed)
		if r.last != "" {
			said += ", last said: " + r.last
		}
		return said
	}
	ended := terminalapp.Ended(r.process, r.since)
	return strings.ToLower(ended[:1]) + ended[1:]
}

// Head of the processes and a row for each, or one line folded: how many run or ended and what the first is doing
func (p *processes) Rows(width, limit int, now time.Time, t theme.Theme, folded bool) []string {
	rows := p.rows(now)
	if len(rows) == 0 {
		return nil
	}
	running := 0
	for _, r := range rows {
		if r.status == terminalapp.Running {
			running++
		}
	}
	var counts []string
	if running > 0 {
		counts = append(counts, strconv.Itoa(running)+" running")
	}
	if ended := len(rows) - running; ended > 0 {
		counts = append(counts, strconv.Itoa(ended)+" ended")
	}
	cols := max(width-2, 8)
	head := func(text string) string {
		return "  " + lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render(ansi.Truncate(text, cols, t.Glyphs.Ellipsis))
	}
	if folded {
		summary := "▸ Background processes: " + strings.Join(counts, ", ")
		if room := cols - ansi.StringWidth(summary) - 3; room >= 12 {
			summary += " · " + compact(rows[0].command, room)
		}
		return []string{head(summary)}
	}
	shown := rows
	if limit > 0 && len(rows) > limit {
		shown = rows[:limit]
	}
	text := "▾ Background processes: " + strings.Join(counts, ", ")
	if hidden := len(rows) - len(shown); hidden > 0 {
		text += ", " + strconv.Itoa(hidden) + " more"
	}
	out := []string{head(text)}
	for _, r := range shown {
		out = append(out, "  "+line(r, cols, t))
	}
	return out
}

// One process as its glyph, command and activity
func line(r row, width int, t theme.Theme) string {
	glyph, tone := glyph(r.status, t)
	activity := r.activity()
	command := compact(r.command, max(8, width-ansi.StringWidth(activity)-6))
	ink := func(text string, c color.Color) string { return lipgloss.NewStyle().Foreground(c).Render(text) }
	return render.Truncate(ink(glyph+" ", tone)+ink(command, t.Text)+ink(" · "+activity, t.TextDim), width, t)
}

// Glyph and color of a process's status
func glyph(status string, t theme.Theme) (string, color.Color) {
	switch status {
	case terminalapp.Running:
		return "⚙", t.Accent
	case terminalapp.Done:
		return "✔", t.Alive
	case terminalapp.Failed:
		return "✘", t.Alarm
	case terminalapp.Killed:
		return "✘", t.Pulse
	}
	return "?", t.TextDim
}

// Command on one line cut to the width with an ellipsis, the dock row naming the whole of it in /processes
func compact(text string, width int) string {
	text = terminalapp.OneLine(text)
	if ansi.StringWidth(text) > width {
		return ansi.Truncate(text, max(width-1, 0), "") + "…"
	}
	return text
}
