package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Owner rows of turns lost in a gateway restart
func (a *app) lostRows() []pendingRow {
	rows := []pendingRow{}
	for _, row := range a.queue.serverRows() {
		if row.Status == "unknown" {
			rows = append(rows, row)
		}
	}
	return rows
}

// Slash commands that settle turns the gateway lost in a restart
func (a *app) recoveryCommands() []slashCommand {
	return []slashCommand{
		{name: "discard", help: "drop a turn lost in a gateway restart without running it again", usage: "<admission_id>", suggest: func(string) []string {
			ids := []string{}
			for _, row := range a.lostRows() {
				ids = append(ids, row.AdmissionID)
			}
			return ids
		}, run: func(r *slashRun) tea.Cmd {
			id := strings.TrimSpace(r.arg)
			if id == "" {
				r.sys("Usage: /discard <admission_id>")
				return nil
			}
			if r.sid == "" {
				r.sys("/discard: no gateway session is open")
				return nil
			}
			var lost *pendingRow
			for _, row := range a.lostRows() {
				if row.AdmissionID == id {
					lost = &row
					break
				}
			}
			if lost == nil {
				r.sys("/discard: No unknown (lost) admission with that id; resume to refresh")
				return nil
			}
			return tea.Batch(r.ran(), a.queue.cancelServerRow(lost, func(err error) tea.Cmd {
				if r.stale() {
					return nil
				}
				if err != nil {
					r.sys("/discard: " + rpcErrorMessage(err))
					return nil
				}
				left := 0
				for _, row := range a.lostRows() {
					if row.AdmissionID != id {
						left++
					}
				}
				tail := "You can send messages again."
				if left > 0 {
					plural := ""
					if left > 1 {
						plural = "s"
					}
					tail = fmt.Sprintf("%d more paused turn%s to /discard first.", left, plural)
				}
				r.sys(fmt.Sprintf("Discarded %s; it was not re-run. %s", id, tail))
				return nil
			}))
		}},
	}
}
