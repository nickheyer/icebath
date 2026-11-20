package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Commands of the spawn tree, its window with pause and status words, the replay of finished trees and their diff
func (a *app) agentsCommands() []slashCommand {
	return []slashCommand{
		{name: "agents", aliases: []string{"tasks"}, help: "open the spawn-tree dashboard (live audit + kill/pause controls)", usage: "[pause|resume|status]",
			suggest: func(string) []string { return []string{"pause", "resume", "status"} },
			run: func(r *slashRun) tea.Cmd {
				switch sub := strings.ToLower(trimJS(r.arg)); sub {
				case "pause", "resume", "unpause":
					params := tuigateway.DelegationPauseParams{Paused: ptr(sub == "pause")}
					return ask(r.rpc, r.sys, tuigateway.DelegationPause, params, func(res *tuigateway.DelegationPauseResult) tea.Cmd {
						if res == nil {
							return nil
						}
						a.store.applyDelegationPause(res.Paused)
						if res.Paused {
							r.sys(tr(txt.SlashCmdOpsAgentsPaused))
						} else {
							r.sys(tr(txt.SlashCmdOpsAgentsResumed))
						}
						return nil
					})
				case "status":
					d := a.store.delegation
					state := tr(txt.SlashCmdOpsAgentsStateActive)
					if d.Paused {
						state = tr(txt.SlashCmdOpsAgentsStatePaused)
					}
					limit := func(v *float64) string {
						if v == nil {
							return "?"
						}
						return strconv.FormatFloat(*v, 'f', -1, 64)
					}
					r.sys(tr(txt.SlashCmdOpsAgentsStatus, state, limit(d.MaxSpawnDepth), limit(d.MaxConcurrentChildren)))
					if failed := a.store.failedDelegations(); len(failed) > 0 {
						r.sys(tr(txt.SlashCmdOpsAgentsFailedHeader, len(failed)))
						for _, f := range failed {
							goal := f.Goal
							if goal == "" {
								goal = tr(txt.HubsAgentsSubagentFallback)
							}
							r.sys(tr(txt.SlashCmdOpsAgentsFailedLine, goal, agentStatusLabel(failedDelegationStatus(f)), firstText(val(f.Error), "-")))
						}
					}
					return nil
				}
				return a.openSpawnTree(0, nil)
			}},
		{name: "processes", aliases: []string{"ps"}, help: "list the background processes of the session, read their output or kill one",
			run: func(r *slashRun) tea.Cmd {
				if r.sid == "" {
					r.sys(tr(txt.ProcessesNone))
					return nil
				}
				return a.openProcesses(r.sid)
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
		return ask(r.rpc, r.sys, tuigateway.SpawnTreeList, tuigateway.SpawnTreeListParams{Limit: ptr(int64(30)), SessionID: &sid}, func(res *tuigateway.SpawnTreeListResult) tea.Cmd {
			if res == nil {
				return nil
			}
			if len(res.Entries) == 0 {
				r.sys(tr(txt.SlashCmdOpsReplayNoneOnDisk))
				return nil
			}
			return a.openSavedSpawnTrees(res.Entries)
		})
	}
	if strings.HasPrefix(lower, "load ") {
		path := trimJS(raw[len("load "):])
		if path == "" {
			r.sys(tr(txt.SlashCmdOpsReplayUsageLoad))
			return nil
		}
		return a.loadSpawnTree(path, r.rpc, r.sys)
	}
	history := a.store.spawnHistory
	if len(history) == 0 {
		r.sys(tr(txt.SlashCmdOpsReplayNoneThisSession))
		return nil
	}
	index := 1
	if raw != "" && lower != "last" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > len(history) {
			r.sys(tr(txt.SlashCmdOpsReplayIndexOutOfRange, len(history)))
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
		r.sys(tr(txt.SlashCmdOpsReplayDiffUsage))
		return nil
	}
	history := a.store.spawnHistory
	resolve := func(token string) *spawnSnapshot {
		if n, err := strconv.Atoi(token); err == nil && n >= 1 && n <= len(history) {
			return &history[n-1]
		}
		return nil
	}
	baseline, candidate := resolve(parts[0]), resolve(parts[1])
	if baseline == nil || candidate == nil {
		r.sys(tr(txt.SlashCmdOpsReplayDiffUnresolved, len(history)))
		return nil
	}
	return a.openSpawnTree(0, &spawnDiffPair{baseline: *baseline, candidate: *candidate})
}
