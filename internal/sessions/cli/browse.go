package cli

import (
	"bufio"
	"cmp"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Short tag of how a session's last turn went
var statusTags = map[string]string{"complete": "done", "interrupted": "intr", "error": "err", "empty": "empty"}

func addBrowse(parent *cobra.Command, at func(*cobra.Command) *run) {
	var sourceFlag string
	var limit int
	command := &cobra.Command{Use: "browse", Short: "Interactive session picker — browse, search, and resume sessions", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := at(cmd)
			if limit == 0 {
				limit = 500
			}
			filter := &sessionsv1.Filter{Limit: int32(limit), Source: sourceFlag, Status: true, ByStart: true}
			if sourceFlag == "" {
				filter.ExcludeSources = []string{"tool"}
			}
			response, err := r.service.List(r.ctx(), &sessionsv1.ListRequest{Filter: filter})
			if err != nil {
				return err
			}
			rows := response.GetSessions()
			if len(rows) == 0 {
				r.print("No sessions found.")
				return nil
			}
			var chosen string
			width, height, sizeErr := term.GetSize(os.Stdout.Fd())
			if term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd()) && sizeErr == nil {
				list := &picker{rows: rows, all: rows, width: width, height: height, delete: func(id string) error {
					_, err := r.service.Delete(r.ctx(), &sessionsv1.DeleteRequest{Id: id})
					return err
				}}
				result, err := tea.NewProgram(list, tea.WithContext(r.ctx())).Run()
				if err != nil {
					chosen = r.fallback(rows)
				} else {
					chosen = result.(*picker).chosen
				}
			} else {
				chosen = r.fallback(rows)
			}
			if chosen == "" {
				r.print("Cancelled.")
				return nil
			}
			r.print("Resuming session: " + chosen)
			return openApp(r.ctx(), r.conn, r.app, chosen, time.Now())
		}}
	command.Flags().StringVar(&sourceFlag, "source", "", "Filter by source (cli, telegram, discord, etc.)")
	command.Flags().IntVar(&limit, "limit", 500, "Max sessions to load (default: 500)")
	parent.AddCommand(command)
}

// Label a session is listed by: its title, else what it started with, else the start of its id
func label(s *sessionsv1.Session) string {
	return cmp.Or(s.GetTitle(), cmp.Or(s.GetPreview(), strs.Head(s.GetId(), 18)))
}

func messageCount(s *sessionsv1.Session) string {
	return fmt.Sprint(s.GetMessages())
}

// Numbered list read from the prompt, for a terminal that can't show the picker
func (r *run) fallback(rows []*sessionsv1.Session) string {
	r.print("\n  Browse sessions  (enter number to resume, q to cancel)\n")
	for i, s := range rows {
		label := label(s)
		if len([]rune(label)) > 50 {
			label = strs.Head(label, 47) + "..."
		}
		r.printf("  %3d. %s  %s  %5s  %s  %s", i+1, cli.Pad(label, 50), cli.Pad(cmp.Or(statusTags[s.GetStatus()], "-"), 5), messageCount(s), cli.Pad(ago(updated(s)), 10),
			strs.Head(s.GetSource(), 6))
	}
	reader := bufio.NewReader(r.cmd.InOrStdin())
	for {
		fmt.Fprintf(r.cmd.OutOrStdout(), "\n  Select [1-%d]: ", len(rows))
		line, err := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if err != nil || line == "" || line == "q" || line == "quit" || line == "exit" {
			return ""
		}
		var n int
		if _, err := fmt.Sscanf(line, "%d", &n); err != nil {
			r.print("  Invalid input. Enter a number or q to cancel.")
			continue
		}
		if n < 1 || n > len(rows) {
			r.printf("  Invalid selection. Enter 1-%d or q to cancel.", len(rows))
			continue
		}
		return rows[n-1].GetId()
	}
}

// Full screen list of the sessions: arrows move, typing filters, enter picks, d deletes after a yes
type picker struct {
	all, rows     []*sessionsv1.Session
	cursor, top   int
	filter        string
	filtering     bool
	confirming    bool
	flash         string
	width, height int
	chosen        string
	delete        func(id string) error
}

func (p *picker) Init() tea.Cmd { return nil }

func (p *picker) refilter() {
	p.rows = nil
	needle := strings.ToLower(p.filter)
	for _, s := range p.all {
		haystack := strings.ToLower(s.GetTitle() + " " + s.GetPreview() + " " + s.GetId() + " " + s.GetSource())
		if needle == "" || strings.Contains(haystack, needle) {
			p.rows = append(p.rows, s)
		}
	}
	p.cursor = min(p.cursor, max(len(p.rows)-1, 0))
}

func (p *picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		key := msg.String()
		p.flash = ""
		if p.confirming {
			p.confirming = false
			if key == "y" || key == "Y" {
				id := p.rows[p.cursor].GetId()
				if err := p.delete(id); err != nil {
					p.flash = "Delete failed."
					if strings.Contains(err.Error(), "active turn lease") {
						p.flash = "Session is active (a live turn owns it); try again when it finishes."
					}
					break
				}
				p.flash = "Deleted."
				for i, s := range p.all {
					if s.GetId() == id {
						p.all = append(p.all[:i], p.all[i+1:]...)
						break
					}
				}
				p.refilter()
				if len(p.all) == 0 {
					return p, tea.Quit
				}
			}
			break
		}
		switch {
		case key == "up":
			if len(p.rows) > 0 {
				p.cursor = (p.cursor - 1 + len(p.rows)) % len(p.rows)
			}
		case key == "down":
			if len(p.rows) > 0 {
				p.cursor = (p.cursor + 1) % len(p.rows)
			}
		case key == "enter":
			if len(p.rows) > 0 {
				p.chosen = p.rows[p.cursor].GetId()
			}
			return p, tea.Quit
		case key == "esc":
			if p.filter != "" {
				p.filter, p.filtering = "", false
				p.refilter()
				break
			}
			return p, tea.Quit
		case key == "backspace":
			if p.filter != "" {
				p.filter = string([]rune(p.filter)[:len([]rune(p.filter))-1])
				p.filtering = p.filter != ""
				p.refilter()
			}
		case key == "q" && p.filter == "":
			return p, tea.Quit
		case key == "d" && p.filter == "" && len(p.rows) > 0:
			p.confirming = true
		case key == "ctrl+c":
			return p, tea.Quit
		case len(msg.Text) == 1 && msg.Text[0] >= 32 && msg.Text[0] <= 126:
			p.filter += msg.Text
			p.filtering = true
			p.refilter()
		}
	}
	return p, nil
}

func (p *picker) View() tea.View {
	if p.width < 40 || p.height < 5 {
		view := tea.NewView("Terminal too small")
		view.AltScreen = true
		return view
	}

	var lines []string
	if p.filtering {
		lines = append(lines, cli.Painted("  Browse sessions — filter: "+p.filter+"█", cli.Bold, cli.Cyan))
	} else {
		lines = append(lines, cli.Painted("  Browse sessions — ↑↓ navigate  Enter select  Type to filter  Esc quit", cli.Bold, cli.Yellow))
	}
	name := max(20, p.width-62)
	lines = append(lines, cli.Painted("   "+cli.Pad("Title / Preview", name)+"  "+cli.Pad("Stat", 5)+"  "+padLeft("Msgs", 5)+"  "+cli.Pad("Active", 10)+"  "+cli.Pad("Src", 5)+" ID", cli.Dim), "")
	visible := p.height - 5
	if p.cursor < p.top {
		p.top = p.cursor
	}
	if p.cursor >= p.top+visible {
		p.top = p.cursor - visible + 1
	}
	if len(p.rows) == 0 {
		lines = append(lines, "  No sessions match the filter.")
	}
	colors := map[string]string{"done": cli.Green, "intr": cli.Yellow, "err": cli.Red, "empty": cli.Gray}
	for i := p.top; i < min(len(p.rows), p.top+visible); i++ {
		s := p.rows[i]
		tag := cmp.Or(statusTags[s.GetStatus()], "-")
		cells := cli.Pad(strs.Head(label(s), name), name) + "  "
		tail := "  " + padLeft(messageCount(s), 5) + "  " + cli.Pad(ago(updated(s)), 10) + "  " + cli.Pad(strs.Head(s.GetSource(), 6), 5) + " " + strs.Head(s.GetId(), 18)
		if i == p.cursor {
			lines = append(lines, cli.Painted(" → "+cells+cli.Pad(tag, 5)+tail, cli.Bold, cli.Green))
		} else {
			lines = append(lines, "   "+cells+cli.Painted(cli.Pad(tag, 5), cmp.Or(colors[tag], cli.Reset))+tail)
		}
	}
	for len(lines) < p.height-1 {
		lines = append(lines, "")
	}
	footer := fmt.Sprintf("  %d/%d sessions", min(p.cursor+1, len(p.rows)), len(p.rows))
	if len(p.rows) != len(p.all) {
		footer += fmt.Sprintf(" (filtered from %d)", len(p.all))
	}
	if !p.filtering {
		footer += "   d delete"
	}
	switch {
	case p.confirming:
		target := label(p.rows[p.cursor])
		if len([]rune(target)) > 40 {
			target = strs.Head(target, 37) + "..."
		}
		footer = cli.Painted(fmt.Sprintf("  Delete session '%s'? [y/N]", target), cli.Bold, cli.Red)
	case p.flash != "":
		footer = "  " + p.flash
	}
	lines = append(lines, footer)
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, p.width, "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}
