package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Stdio child ledgers guarded by mcpMu, live pids with their owner, orphans awaiting the sweep, groups and start times
var (
	stdioPIDs             = map[int]string{}
	orphanStdioPIDs       = map[int]bool{}
	orphanStdioPIDServers = map[int]string{}
	stdioPGIDs            = map[int]int{}
	stdioStartTimes       = map[int]int64{}
)

// Adopter profile home to server names whose shared connection an owner teardown removed, guarded by mcpMu
var orphanedAdopters = map[string]map[string]bool{}

// Pause between the SIGTERM and SIGKILL passes of the orphan sweep, swapped by tests
var orphanKillGrace = 2 * time.Second

// Process primitives the orphan sweep signals and probes through, swapped by tests
var reaper = struct {
	killpg    func(pgid, sig int) error
	kill      func(pid, sig int)
	getpgrp   func() (int, bool)
	pidExists func(pid int) bool
	startTime func(pid int) (int64, bool)
	sleep     func(time.Duration)
}{signalGroup, signalProcessTree, ownProcessGroup, pidExists, leaderStartTime, time.Sleep}

// Start time fingerprint of a process group leader, false when no reading is available
func leaderStartTime(pid int) (int64, bool) { return tools.ProcessStartTime(pid) }

// Drops connect retry cooldowns of keys, every key when all is set, caller holds mcpMu
func clearConnectCooldowns(keys map[ServerKey]bool, all bool) {
	if all {
		clear(serverConnectRetryAfter)
		clear(serverConnectFailures)
		return
	}
	for k := range keys {
		delete(serverConnectRetryAfter, k)
		delete(serverConnectFailures, k)
	}
}

// Re-registers profiles whose adopted shared connection an owner teardown removed, each under its own home and secrets
func reregisterOrphanedAdopters(ctx context.Context, env *tools.Env) {
	mcpMu.Lock()
	pending := orphanedAdopters
	orphanedAdopters = map[string]map[string]bool{}
	mcpMu.Unlock()
	for adopter, names := range pending {
		func() {
			defer func() {
				if r := recover(); r != nil {
					mcpLogger().Debug(fmt.Sprintf("MCP: re-registration for profile scope %s failed: %v", adopter, r))
				}
			}()
			actx := home.WithOverride(ctx, adopter)
			aenv := &tools.Env{}
			if env != nil {
				copied := *env
				aenv = &copied
			}
			aenv.Home = adopter
			aenv.Secrets = config.NewSecrets(adopter)
			servers := map[string]*Rendered{}
			for name, r := range LoadServerConfigs(actx, aenv) {
				if names[name] {
					servers[name] = r
				}
			}
			if len(servers) > 0 {
				registerMCPServers(actx, aenv, servers)
			}
		}()
	}
}

// Closes the selected connections in parallel and stops the loop, a nil scope with no names closes every server
func ShutdownMCPServers(ctx context.Context, scope *string, names []string, timeout time.Duration) {
	wildcard := scope == nil && names == nil
	want := ""
	if scope != nil {
		want = *scope
	}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	mcpMu.Lock()
	var selected []ServerKey
	for key := range mcpServers {
		if (wildcard || serverScopeKeys[key] == want) && (names == nil || nameSet[key.Name]) {
			selected = append(selected, key)
		}
	}
	snapshot := make([]*serverTask, 0, len(selected))
	for _, key := range selected {
		snapshot = append(snapshot, mcpServers[key])
	}
	status := map[ServerKey]bool{}
	switch {
	case names != nil:
		for _, k := range selected {
			status[k] = true
		}
	case wildcard:
		for k := range mcpServers {
			status[k] = true
		}
		for k := range serverScopeKeys {
			status[k] = true
		}
		for k := range serverToolScopes {
			status[k] = true
		}
		for k := range serverConnecting {
			status[k] = true
		}
		for k := range serverConnectErrors {
			status[k] = true
		}
	default:
		for k, owner := range serverScopeKeys {
			if owner == want {
				status[k] = true
			}
		}
	}
	if !wildcard {
		for _, key := range selected {
			for adopter := range serverToolScopes[key] {
				if adopter != want {
					if orphanedAdopters[adopter] == nil {
						orphanedAdopters[adopter] = map[string]bool{}
					}
					orphanedAdopters[adopter][key.Name] = true
				}
			}
		}
	}
	mcpMu.Unlock()
	clearSelectedStatus := func() {
		for k := range status {
			delete(serverConnecting, k)
			delete(serverConnectErrors, k)
			delete(serverScopeKeys, k)
			delete(serverToolScopes, k)
		}
	}
	if len(snapshot) > 0 && runningLoop() != nil {
		finished := make(chan struct{})
		err := spawnOnLoop(ctx, func(loopCtx context.Context) {
			defer close(finished)
			done := make(chan struct{}, len(snapshot))
			for _, srv := range snapshot {
				go func(srv *serverTask) {
					defer func() {
						if r := recover(); r != nil {
							mcpLogger().Debug(fmt.Sprintf("Error closing MCP server '%s': %v", srv.name, r))
						}
						done <- struct{}{}
					}()
					srv.shutdown(loopCtx)
				}(srv)
			}
			for range snapshot {
				<-done
			}
			mcpMu.Lock()
			for _, key := range selected {
				delete(mcpServers, key)
				delete(serverScopeKeys, key)
			}
			clearSelectedStatus()
			clearConnectCooldowns(status, wildcard)
			mcpMu.Unlock()
		})
		if err != nil {
			mcpLogger().Debug(fmt.Sprintf("MCP shutdown: failed to schedule: %v", err))
		} else {
			t := time.NewTimer(timeout)
			select {
			case <-finished:
			case <-t.C:
				mcpLogger().Debug(fmt.Sprintf("Error during MCP shutdown: timed out after %.1fs", timeout.Seconds()))
			}
			t.Stop()
		}
	}
	mcpMu.Lock()
	if len(snapshot) == 0 {
		clearSelectedStatus()
	}
	clearConnectCooldowns(status, wildcard)
	mcpMu.Unlock()
	stopMCPLoop(!wildcard)
	if names == nil {
		closeStderrLogs(want)
	}
}

// Pops the pids to reap with their spawn time groups and start times out of the ledgers
func takeReapablePIDs(includeActive bool, serverName string) (map[int]string, map[int]int, map[int]int64) {
	owned := func(pid int, owner string) bool { return serverName == "" || owner == serverName }
	mcpMu.Lock()
	defer mcpMu.Unlock()
	pids := map[int]string{}
	for pid := range orphanStdioPIDs {
		owner, ok := orphanStdioPIDServers[pid]
		if !ok {
			owner = "orphan"
		}
		if owned(pid, owner) {
			pids[pid] = owner
		}
	}
	for pid := range pids {
		delete(orphanStdioPIDs, pid)
		delete(orphanStdioPIDServers, pid)
	}
	if includeActive {
		for pid, owner := range stdioPIDs {
			if owned(pid, owner) {
				pids[pid] = owner
				delete(stdioPIDs, pid)
			}
		}
	}
	pgids := map[int]int{}
	starts := map[int]int64{}
	for pid := range pids {
		if pg, ok := stdioPGIDs[pid]; ok {
			pgids[pid] = pg
			delete(stdioPGIDs, pid)
		}
		if st, ok := stdioStartTimes[pid]; ok {
			starts[pid] = st
			delete(stdioStartTimes, pid)
		}
	}
	return pids, pgids, starts
}

// Signals a stdio child through its spawn time group, refusing a recycled pid and never our own group
func signalMCPProcess(pid int, force bool, serverName string, pgid int, hasPGID bool, myPGID int, hasMy bool, expectedStart int64, hasStart bool) {
	if hasStart {
		if current, ok := reaper.startTime(pid); ok && !tools.StartTimeFingerprintsMatch(expectedStart, current) {
			mcpLogger().Debug(fmt.Sprintf("Skip signalling MCP pid %d (%s): start-time mismatch — PID was recycled; refusing to kill an unrelated process group.", pid, serverName))
			return
		}
	}
	if hasPGID && groupSignalSupported {
		if hasMy && pgid == myPGID {
			mcpLogger().Warn(fmt.Sprintf("MCP server '%s' pgid %d matches gateway pgid; skipping killpg to avoid self-kill and using per-pid kill — any grandchildren in this group may not be reaped", serverName, pgid))
		} else if err := reaper.killpg(pgid, signalNumber(force)); err == nil {
			return
		} else {
			mcpLogger().Debug(fmt.Sprintf("killpg(%d, %d) failed for MCP server '%s': %v; falling back to kill(pid)", pgid, signalNumber(force), serverName, err))
		}
	}
	reaper.kill(pid, signalNumber(force))
}

// Whether any member of a recorded group other than ours still runs
func recordedGroupAlive(pgid int, hasPGID bool, myPGID int, hasMy bool) bool {
	if !hasPGID || !groupSignalSupported || (hasMy && pgid == myPGID) {
		return false
	}
	return reaper.killpg(pgid, 0) == nil
}

// Reaps stdio MCP children with SIGTERM then SIGKILL after a grace period, orphans only unless includeActive
func killOrphanedMCPChildren(includeActive bool, serverName string) {
	pids, pgids, starts := takeReapablePIDs(includeActive, serverName)
	if len(pids) == 0 {
		return
	}
	myPGID, hasMy := reaper.getpgrp()
	for pid, owner := range pids {
		pg, hasPG := pgids[pid]
		st, hasSt := starts[pid]
		signalMCPProcess(pid, false, owner, pg, hasPG, myPGID, hasMy, st, hasSt)
		mcpLogger().Debug(fmt.Sprintf("Sent SIGTERM to orphaned MCP process %d (%s)", pid, owner))
	}
	reaper.sleep(orphanKillGrace)
	for pid, owner := range pids {
		pg, hasPG := pgids[pid]
		st, hasSt := starts[pid]
		if reaper.pidExists(pid) || recordedGroupAlive(pg, hasPG, myPGID, hasMy) {
			signalMCPProcess(pid, true, owner, pg, hasPG, myPGID, hasMy, st, hasSt)
			mcpLogger().Warn(fmt.Sprintf("Force-killed MCP process %d (%s) after SIGTERM timeout", pid, owner))
		}
	}
	groups := make([]int, 0, len(pgids))
	for _, pg := range pgids {
		groups = append(groups, pg)
	}
	tools.ReleaseProcessGroups(groups)
}

// Stops the MCP loop only when no registered server still owns it
func stopMCPLoopIfIdle() bool { return stopMCPLoop(true) }
