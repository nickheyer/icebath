package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Commands of the spawn tree, its window with pause and status words, the replay of finished trees and their diff
func (a *app) agentsCommands() []slashCommand {
	return []slashCommand{
		{name: "agents", aliases: []string{"tasks"}, help: "open the spawn-tree dashboard (live audit + kill/pause controls)", usage: "[pause|resume|status]",
			suggest: func(string) []string { return []string{"pause", "resume", "status"} },
			run: func(r *slashRun) tea.Cmd {
				switch sub := strings.ToLower(trimJS(r.arg)); sub {
				case "pause", "resume", "unpause":
					return r.rpc("delegation.pause", map[string]any{"paused": sub == "pause"}, func(res map[string]any) tea.Cmd {
						a.store.applyDelegationStatus(map[string]any{"paused": res["paused"]})
						if truthyJS(res["paused"]) {
							r.sys(tr("slashCmd.ops.agents.paused"))
						} else {
							r.sys(tr("slashCmd.ops.agents.resumed"))
						}
						return nil
					})
				case "status":
					d := a.store.delegation
					state := tr("slashCmd.ops.agents.stateActive")
					if d.Paused {
						state = tr("slashCmd.ops.agents.statePaused")
					}
					limit := func(v *float64) string {
						if v == nil {
							return "?"
						}
						return jsString(*v)
					}
					r.sys(tr("slashCmd.ops.agents.status", state, limit(d.MaxSpawnDepth), limit(d.MaxConcurrentChildren)))
					return nil
				}
				return a.openSpawnTree(0, nil)
			}},
		{name: "replay", help: "replay a completed spawn tree", usage: "[N|last|list|load <path>]",
			suggest: func(string) []string { return []string{"last", "list", "load"} },
			run:     a.replayCommand},
		{name: "replay-diff", help: "diff two completed spawn trees (indexes from /replay list or history N)", usage: "<baseline> <candidate>",
			run: a.replayDiffCommand},
	}
}

// Saved trees of the session, one loaded by path, or a finished tree of this session by its history index
func (a *app) replayCommand(r *slashRun) tea.Cmd {
	raw := trimJS(r.arg)
	lower := strings.ToLower(raw)
	if lower == "list" || lower == "ls" {
		sid := r.sid
		if sid == "" {
			sid = "default"
		}
		return r.rpc("spawn_tree.list", map[string]any{"limit": 30, "session_id": sid}, func(res map[string]any) tea.Cmd {
			entries, _ := res["entries"].([]any)
			if len(entries) == 0 {
				r.sys(tr("slashCmd.ops.replay.noneOnDisk"))
				return nil
			}
			return a.openSavedSpawnTrees(entries)
		})
	}
	if strings.HasPrefix(lower, "load ") {
		path := trimJS(raw[len("load "):])
		if path == "" {
			r.sys(tr("slashCmd.ops.replay.usageLoad"))
			return nil
		}
		return a.loadSpawnTree(path, r.rpc)
	}
	history := a.store.spawnHistory
	if len(history) == 0 {
		r.sys(tr("slashCmd.ops.replay.noneThisSession"))
		return nil
	}
	index := 1
	if raw != "" && lower != "last" {
		parsed, ok := jsParseInt(raw)
		if !ok || parsed < 1 || parsed > len(history) {
			r.sys(tr("slashCmd.ops.replay.indexOutOfRange", len(history)))
			return nil
		}
		index = parsed
	}
	return a.openSpawnTree(index, nil)
}

// Two finished trees of this session side by side, picked by their history indexes
func (a *app) replayDiffCommand(r *slashRun) tea.Cmd {
	var parts []string
	for _, p := range splitArgs(r.arg) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) != 2 {
		r.sys(tr("slashCmd.ops.replayDiff.usage"))
		return nil
	}
	history := a.store.spawnHistory
	resolve := func(token string) *spawnSnapshot {
		if n, ok := jsParseInt(token); ok && n >= 1 && n <= len(history) {
			return &history[n-1]
		}
		return nil
	}
	baseline, candidate := resolve(parts[0]), resolve(parts[1])
	if baseline == nil || candidate == nil {
		r.sys(tr("slashCmd.ops.replayDiff.unresolved", len(history)))
		return nil
	}
	return a.openSpawnTree(0, &spawnDiffPair{baseline: *baseline, candidate: *candidate})
}
