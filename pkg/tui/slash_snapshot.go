package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Drops the gateway socket and blocks every reconnect until the returned func starts the owner again
func (c *gatewayClient) releaseForMaintenance() func() {
	c.locked(func() {
		c.clearReconnectLocked()
		c.reconnectStop, c.reconnectToken = func() bool { return true }, new(int)
		c.localGeneration++
		c.lifecycleLocked("[lifecycle] gateway released for snapshot maintenance")
		c.resetStartupStateLocked()
		c.closeGatewaySocketLocked()
		c.closeSidecarLocked()
		c.attachURL = ""
	})
	return func() {
		c.locked(func() {
			c.reconnectStop, c.reconnectToken = nil, nil
			c.reconnects = 0
			c.localStarted = false
			c.lifecycleLocked("[lifecycle] snapshot maintenance done, starting the gateway again")
			c.startLocked()
			c.flushBufferedLocked()
		})
	}
}

// Whether the state database of the home holds the session id
func sessionInState(h, sid string) (bool, error) {
	db, err := state.OpenHome(h, state.Options{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer state.CloseAllUnder(h)
	id, err := db.ResolveSessionID(sid)
	return id == sid, err
}

// Home and catalog of a /snapshot run in this terminal UI
func (a *app) snapshotCommand() gatewaycli.SnapshotCommand {
	return gatewaycli.SnapshotCommand{Home: home.Dir(a.ctx), T: func(key string, kwargs map[string]any) string {
		return i18n.T(a.ctx, "cli.commands."+key, "", kwargs)
	}}
}

// Shows the lines of a snapshot subcommand, a table in the pager and short answers as a system line
func (a *app) snapshotOutput(lines []string, err error) tea.Cmd {
	if err != nil {
		a.sys("/snapshot: " + err.Error())
		return nil
	}
	return a.showOutput(strings.Join(lines, "\n"), "snapshot")
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

// Restores a quick snapshot with the profile handed off the gateway, then reopens the session on the restored state
func (a *app) restoreSnapshot(snap gatewaycli.SnapshotCommand, snapID string) tea.Cmd {
	ui := &a.store.ui
	local := attachURLEnv() == ""
	handoff := false
	var targetErr error
	if local {
		dir, err := backup.QuickSnapshotDir(snapID, snap.Home)
		handoff, targetErr = dir != "", err
	}
	if targetErr != nil || !handoff {
		return func() tea.Msg {
			restored, err := false, targetErr
			if err == nil {
				restored, err = backup.RestoreQuickSnapshot(a.ctx, snapID, snap.Home)
			}
			return asyncDoneMsg{run: func() tea.Cmd {
				return a.snapshotOutput(snap.RestoreReport(snapID, restored, err, local))
			}}
		}
	}
	stored := ui.StoredSid
	if stored == "" {
		stored = ui.Sid
	}
	connected, status := ui.GatewayConnected, ui.Status
	off := false
	ui.GatewayConnected, ui.Status = &off, "restoring snapshot"
	return func() tea.Msg {
		var reopen func()
		res, err := gatewaycli.RestoreSnapshotOffGateway(a.ctx, snap.Home, snapID, func(h string) (*pyjson.Object, error) {
			return gatewaycli.ManualGatewayIdentity(h, func(h string) *pyjson.Object {
				return gateway.IdentifyGateway(h, gateway.DefaultControlClientTimeout)
			}, fmt.Errorf)
		}, func(h string) error {
			reopen = a.gw.releaseForMaintenance()
			state.CloseAllUnder(h)
			return nil
		})
		kept := true
		var probeErr error
		if res.Restored && stored != "" {
			kept, probeErr = sessionInState(snap.Home, stored)
		}
		return asyncDoneMsg{run: func() tea.Cmd {
			report := func() tea.Cmd {
				shown := a.snapshotOutput(snap.RestoreReport(snapID, res.Restored, err, true))
				if line := res.ReserveLine(); line != "" {
					a.sys(line)
				}
				if probeErr != nil {
					a.sys("/snapshot: " + probeErr.Error())
				} else if !kept {
					a.sys("This session is not part of the restored state.")
				}
				return shown
			}
			if !res.Detached {
				ui.GatewayConnected, ui.Status = connected, status
				return report()
			}
			previous := ui.Sid
			a.turn.reset()
			ui.Busy, ui.Compacting, ui.Status = false, false, tr("session.status.reconnecting")
			a.recoverSid = stored
			if probeErr == nil && !kept {
				a.recoverSid, a.events.startupResumeID = "", ""
			}
			reopen()
			return a.afterReopen(previous, a.recoverSid != "", time.Now().Add(90*time.Second), report)
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
				parts := append([]string{"/snapshot"}, strings.Fields(r.arg)...)
				sub := "list"
				if len(parts) > 1 {
					sub = pystr.Lower(parts[1])
				}
				snap := a.snapshotCommand()
				switch sub {
				case "list", "ls":
					return tea.Batch(r.ran(), a.snapshotOutput(snap.List()))
				case "create":
					return tea.Batch(r.ran(), a.snapshotOutput(snap.Create(parts)))
				case "prune":
					return tea.Batch(r.ran(), a.snapshotOutput(snap.Prune(parts)))
				case "restore", "rewind":
					snapID, lines, err := snap.RestoreTarget(parts)
					if err != nil || lines != nil {
						return tea.Batch(r.ran(), a.snapshotOutput(lines, err))
					}
					return a.confirmDestructive("Restore snapshot "+snapID+"?",
						"State files of "+home.Display(snap.Home)+" go back to this snapshot. The gateway stops for the restore and the session reopens on the restored state.",
						"Restore", "Cancel", func() tea.Cmd { return tea.Batch(r.ran(), a.restoreSnapshot(snap, snapID)) })
				}
				return tea.Batch(r.ran(), a.snapshotOutput(snap.Unknown(sub), nil))
			}},
	}
}
