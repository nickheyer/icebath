package tui

import (
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const processRetainSeconds = 60

var processWhitespaceRe = regexp.MustCompile(`[` + jsWS + `]+`)

// Background processes of the session they belong to, as process.list reports them
type processSnapshot struct {
	Sid       string
	Processes []tuigateway.ProcessEntry
}

// Dock row of one background process, its command, verdict and timings
type processRow struct {
	Command          string
	Detail           string
	ElapsedSeconds   float64
	ID               string
	SinceExitSeconds float64
	Status           string
	Tail             string
}

// Replaces the process snapshot only when its session or entries changed
func (s *tuiStore) applyProcessSnapshot(sid string, processes []tuigateway.ProcessEntry) {
	if processes == nil {
		processes = []tuigateway.ProcessEntry{}
	}
	if s.processSnapshot.Sid != sid || !reflect.DeepEqual(s.processSnapshot.Processes, processes) {
		s.processSnapshot = processSnapshot{Sid: sid, Processes: processes}
	}
}

var reasonStatus = map[string]string{"failed_start": "failed", "killed": "killed", "lost": "lost"}

// Status of a process entry, running until it exited and then by its completion reason or exit code
func processStatus(entry tuigateway.ProcessEntry) string {
	if entry.Status != "exited" {
		return "running"
	}
	if status, known := reasonStatus[val(entry.CompletionReason)]; known {
		return status
	}
	if val(entry.ExitCode) != 0 {
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
func processVerdict(row processRow, exitCode *int64) string {
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
			code = strconv.FormatInt(*exitCode, 10)
		}
		verdict = "exit " + code
	}
	return verdict + " · " + strconv.FormatFloat(row.SinceExitSeconds, 'f', -1, 64) + "s ago"
}

// Rows of running processes and those that exited within the retain window, running ones first
func buildProcessRows(processes []tuigateway.ProcessEntry, nowMs float64) []processRow {
	nowS := nowMs / 1000
	rows := []processRow{}
	for _, entry := range processes {
		status := processStatus(entry)
		exitedAt := 0.0
		if status != "running" {
			exitedAt = val(entry.ExitedAt)
		}
		sinceExit := 0.0
		if exitedAt != 0 && !math.IsNaN(exitedAt) {
			sinceExit = math.Max(0, math.Floor(nowS-exitedAt))
		}
		if status != "running" && (exitedAt == 0 || math.IsNaN(exitedAt) || sinceExit > processRetainSeconds) {
			continue
		}
		command := collapseProcessText(entry.Command)
		if command == "" {
			command = "background process"
		}
		preview := val(entry.OutputPreview)
		row := processRow{
			Command:          command,
			Detail:           lastOutputLine(preview),
			ElapsedSeconds:   math.Max(0, float64(val(entry.UptimeSeconds))) - sinceExit,
			ID:               entry.SessionID,
			SinceExitSeconds: sinceExit,
			Status:           status,
			Tail:             val(entry.OutputTail),
		}
		row.Detail = processVerdict(row, entry.ExitCode)
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
	var processes []tuigateway.ProcessEntry
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

// Keymap context of the background process list
const processesContext = "tui.processes"

// Read and kill keys of the background process list, registered with the keymap of the app
func registerProcessKeys(km *keys.Map) {
	if keys.ContextDocs[processesContext] == "" {
		if err := keys.AddContext(processesContext, "background processes"); err != nil {
			panic(err)
		}
	}
	km.Register(keys.Action{ID: "tui.processes.read", Context: processesContext, Label: "read", Doc: "page the output of the background process", Default: []string{"enter"}})
	km.Register(keys.Action{ID: "tui.processes.kill", Context: processesContext, Label: "kill", Doc: "kill the background process", Default: []string{"x"}})
}

// State text of a listed process, how long it ran or how it ended
func processStateText(entry tuigateway.ProcessEntry) string {
	status := processStatus(entry)
	if status == "running" {
		return "running · " + strconv.FormatInt(val(entry.UptimeSeconds), 10) + "s"
	}
	if status == "killed" || status == "lost" {
		return status
	}
	if entry.ExitCode != nil {
		return status + " · exit " + strconv.FormatInt(*entry.ExitCode, 10)
	}
	return status
}

// Background processes of the session in a list, enter paging the output tail and x killing the one under the cursor
func (a *app) openProcesses(sid string) tea.Cmd {
	params := tuigateway.SessionIDParams{SessionID: sid}
	return call(a.gw, tuigateway.ProcessList, params, func(r *tuigateway.ProcessListResult, err error) tea.Cmd {
		if err != nil {
			a.sys("error: " + rpcErrorMessage(err))
			return nil
		}
		entries := r.Processes
		refresh := func(l *cards.List) tea.Cmd {
			return call(a.gw, tuigateway.ProcessList, params, func(r *tuigateway.ProcessListResult, err error) tea.Cmd {
				if err == nil {
					entries = r.Processes
					a.store.applyProcessSnapshot(sid, entries)
					l.Refresh()
				}
				return nil
			})
		}
		a.store.applyProcessSnapshot(sid, entries)
		list := cards.NewList(a.cardEnv(), cards.ListSpec{
			Title: tr("hubs.agents.processes"), Glyph: a.toolkit.Glyphs.Jobs, Key: "tui:processes", Context: processesContext, Primary: "tui.processes.read",
			Columns: []render.Column{{Title: tr("exitSummary.id")}, {Title: tr("processes.column.state")}, {Title: tr("processes.column.command"), Flex: 3}},
			Rows: func() [][]string {
				out := make([][]string, len(entries))
				for i, e := range entries {
					out[i] = []string{e.SessionID, processStateText(e), collapseProcessText(e.Command)}
				}
				return out
			},
			Detail: func(i int) []string {
				if i < 0 || i >= len(entries) {
					return nil
				}
				return []string{lastOutputLine(val(entries[i].OutputTail))}
			},
			Empty: func() string { return tr("processes.none") },
			Actions: map[string]func(l *cards.List, i int) tea.Cmd{
				"tui.processes.read": func(l *cards.List, i int) tea.Cmd {
					e := entries[i]
					if strings.TrimSpace(val(e.OutputTail)) == "" {
						return wm.Notify(tr("processes.noOutput", e.SessionID), wm.Info)
					}
					return a.page(val(e.OutputTail), e.SessionID+" · "+truncateEnd(collapseProcessText(e.Command), 40))
				},
				"tui.processes.kill": func(l *cards.List, i int) tea.Cmd {
					id := entries[i].SessionID
					kill := tuigateway.ProcessKillParams{SessionID: sid, ProcessID: id}
					return call(a.gw, tuigateway.ProcessKill, kill, func(k *tuigateway.ProcessKillResult, err error) tea.Cmd {
						switch {
						case err != nil:
							return wm.Notify(tr("processes.killFailed", id, rpcErrorMessage(err)), wm.Error)
						case k.Status == "killed":
							return tea.Batch(refresh(l), wm.Notify(tr("processes.killed", id), wm.Success))
						case k.Status == "already_exited":
							return tea.Batch(refresh(l), wm.Notify(tr("processes.alreadyExited", id), wm.Info))
						case k.Status == "not_found":
							return tea.Batch(refresh(l), wm.Notify(tr("processes.notFound", id), wm.Warn))
						}
						return wm.Notify(tr("processes.killFailed", id, val(k.Error)), wm.Error)
					})
				},
			},
		})
		return a.wm.ShowOverlay(list)
	})
}
