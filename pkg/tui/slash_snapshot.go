package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Drops the gateway socket and blocks every reconnect until the returned func, which the update loop calls, starts the owner again
func (c *gatewayClient) releaseForMaintenance() func() {
	c.locked(func() {
		c.clearReconnectLocked()
		c.reconnectStop, c.reconnectToken = func() bool { return true }, new(int)
		c.localGeneration++
		c.lifecycleLocked("[lifecycle] gateway released for snapshot maintenance")
		c.resetStartupStateLocked()
		c.closeGatewaySocketLocked()
		c.attachURL = ""
	})
	return func() {
		c.lockedOnLoop(func() {
			c.reconnectStop, c.reconnectToken = nil, nil
			c.reconnects = 0
			c.localStarted = false
			c.lifecycleLocked("[lifecycle] snapshot maintenance done, starting the gateway again")
			c.startLocked()
			c.flushBufferedLocked()
		})
	}
}

// Shows the lines of a snapshot answer, a table in the pager and a failure as a system line
func (a *app) snapshotOutput(lines []string, err error) tea.Cmd {
	if err != nil {
		a.sys("/snapshot: " + rpcErrorMessage(err))
		return nil
	}
	return a.showOutput(strings.Join(lines, "\n"), "snapshot")
}

// Shows the lines a snapshot method answered or its failure
func (a *app) snapshotAnswer(res *tuigateway.SnapshotLines, err error) tea.Cmd {
	if err != nil {
		return a.snapshotOutput(nil, err)
	}
	return a.snapshotOutput(res.Lines, nil)
}

// Subcommands the argument completes to
func snapshotSuggestions(prefix string) []string {
	kept := []string{}
	for _, value := range []string{"list", "create", "restore", "prune"} {
		if strings.HasPrefix(value, strings.ToLower(prefix)) {
			kept = append(kept, value)
		}
	}
	return kept
}

// Restores a quick snapshot through the launcher, the socket released for the handoff, then reopens the session
func (a *app) restoreSnapshot(snapID string) tea.Cmd {
	ui := &a.store.ui
	handoff := attachURLEnv() == ""
	stored := ui.StoredSid
	if stored == "" {
		stored = ui.Sid
	}
	connected, status := ui.GatewayConnected, ui.Status
	if handoff {
		off := false
		ui.GatewayConnected, ui.Status = &off, "restoring snapshot"
	}
	return func() tea.Msg {
		var reopen func()
		res := a.host.RestoreSnapshot(a.ctx, snapID, stored, handoff, func() { reopen = a.gw.releaseForMaintenance() })
		return asyncDoneMsg{run: func() tea.Cmd {
			if !res.Released {
				if handoff {
					ui.GatewayConnected, ui.Status = connected, status
				}
				return a.snapshotOutput(res.Lines, nil)
			}
			previous := ui.Sid
			a.turn.reset()
			ui.Busy, ui.Compacting, ui.Status = false, false, tr("session.status.reconnecting")
			a.recoverSid = stored
			if !res.SessionKept {
				a.recoverSid, a.events.startupResumeID = "", ""
			}
			reopen()
			return a.afterReopen(previous, a.recoverSid != "", time.Now().Add(90*time.Second), func() tea.Cmd { return a.snapshotOutput(res.Lines, nil) })
		}}
	}
}

// Runs then once the session reopened after a restore, at the latest when the deadline passes
func (a *app) afterReopen(previous string, resumed bool, deadline time.Time, then func() tea.Cmd) tea.Cmd {
	return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			sid := a.store.ui.Sid
			if reopened := sid != "" && (resumed && a.recoverSid == "" || !resumed && sid != previous); reopened || time.Now().After(deadline) {
				return then()
			}
			return a.afterReopen(previous, resumed, deadline, then)
		}}
	})
}

// Slash command for quick state snapshots, list, create, prune and restore behind the confirm dialog
func (a *app) snapshotCommands() []slashCommand {
	return []slashCommand{
		{name: "snapshot", aliases: []string{"snap"}, help: "create, list, restore or prune state snapshots",
			usage: "[list|create [label]|restore <id>|prune [N]]", suggest: snapshotSuggestions, run: func(r *slashRun) tea.Cmd {
				words := strings.Fields(r.arg)
				sub := "list"
				if len(words) > 0 {
					sub, words = strings.ToLower(words[0]), words[1:]
				}
				switch sub {
				case "list", "ls":
					return tea.Batch(r.ran(), call(a.gw, tuigateway.SnapshotList, tuigateway.SnapshotListParams{}, a.snapshotAnswer))
				case "create":
					return tea.Batch(r.ran(), call(a.gw, tuigateway.SnapshotCreate, tuigateway.SnapshotArgsParams{Args: words}, a.snapshotAnswer))
				case "prune":
					return tea.Batch(r.ran(), call(a.gw, tuigateway.SnapshotPrune, tuigateway.SnapshotArgsParams{Args: words}, a.snapshotAnswer))
				case "restore", "rewind":
					return tea.Batch(r.ran(), call(a.gw, tuigateway.SnapshotTarget, tuigateway.SnapshotArgsParams{Args: words}, func(answer *tuigateway.SnapshotTargetResult, err error) tea.Cmd {
						if err != nil {
							return a.snapshotOutput(nil, err)
						}
						snapID := val(answer.ID)
						if answer.ID == nil {
							return a.snapshotOutput(answer.Lines, nil)
						}
						return a.confirmDestructive("Restore snapshot "+snapID+"?",
							"State files of "+home.Display(home.Dir(a.ctx))+" go back to this snapshot. The gateway stops for the restore and the session reopens on the restored state.",
							"Restore", "Cancel", func() tea.Cmd { return a.restoreSnapshot(snapID) })
					}))
				}
				return tea.Batch(r.ran(), a.snapshotOutput([]string{commandText(a.ctx, "snapshot.unknown_subcommand", map[string]any{"subcommand": sub}),
					commandText(a.ctx, "snapshot.usage", nil)}, nil))
			}},
	}
}
