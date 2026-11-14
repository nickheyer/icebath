package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/proc"
)

// How long a TERMed process gets before KILL
const reapGrace = 3 * time.Second

// Suffix the kernel appends to the cwd of a process whose directory was removed
const deletedSuffix = " (deleted)"

// Launcher ancestry a detached pass spares, known false when the launcher could not compute it and every process is spared
type lineage struct {
	known bool
	chain []proc.Identity
}

// One same-user process whose working directory lies in scratch
type reapCandidate struct {
	id  proc.Identity
	cwd string
}

// A registered live process, its identity kept when its start could be read
type registration struct {
	id    proc.Identity
	known bool
}

// Pids never signalled, this process with its ancestors and the launcher ancestors still running, all when the lineage is unknown
func ownLineage(table proc.Table, from lineage) (map[int]bool, bool) {
	if !from.known {
		return nil, true
	}
	self := os.Getpid()
	spare := map[int]bool{self: true}
	if ancestors, err := table.Ancestors(self); err == nil {
		for _, a := range ancestors {
			spare[a.PID] = true
		}
	}
	for _, id := range from.chain {
		start, err := proc.Birth(id.PID)
		switch {
		case errors.Is(err, proc.ErrNotFound):
		case err != nil, start == id.Start:
			spare[id.PID] = true
		}
	}
	return spare, false
}

// Doomed entries left after sparing owned work, reaping same-user processes rooted in them or in vanished scratch paths
func reapRootedIn(scratchRoot string, doomed []string, from lineage, audit *pruneAudit) ([]string, error) {
	root, err := resolvePath(scratchRoot)
	if err != nil {
		return nil, err
	}
	targets, err := resolveAll(doomed)
	if err != nil {
		return nil, err
	}
	table, err := proc.ReadTable()
	if err != nil {
		return nil, err
	}
	spare, spareAll := ownLineage(table, from)
	var victims, lineageHits []reapCandidate
	for _, e := range table {
		if !sameUser(e.PID) {
			continue
		}
		cwd, err := proc.Cwd(e.PID)
		if err != nil || cwd == "" {
			continue
		}
		deleted := strings.HasSuffix(cwd, deletedSuffix)
		cwdPath := strings.TrimSuffix(cwd, deletedSuffix)
		if !under(cwdPath, root) {
			continue
		}
		if !deleted && exists(cwdPath) && !slices.ContainsFunc(targets, func(t string) bool { return under(cwdPath, t) }) {
			continue
		}
		c := reapCandidate{id: e.Identity(), cwd: cwdPath}
		if spareAll || spare[e.PID] {
			lineageHits = append(lineageHits, c)
		} else {
			victims = append(victims, c)
		}
	}
	var registered map[int]registration
	if len(victims) > 0 {
		abs, err := filepath.Abs(scratchRoot)
		if err != nil {
			return nil, err
		}
		if registered, err = registeredLive(filepath.Dir(filepath.Dir(abs))); err != nil {
			return nil, err
		}
	}
	owned := slices.Clone(lineageHits)
	for _, c := range victims {
		if ownedByIcebath(table, c.id, registered) {
			owned = append(owned, c)
		}
	}
	for _, c := range owned {
		for _, entry := range slices.Clone(doomed) {
			real, err := resolvePath(entry)
			if err != nil {
				return nil, err
			}
			if !under(c.cwd, real) {
				continue
			}
			doomed = slices.DeleteFunc(doomed, func(d string) bool { return d == entry })
			name, err := proc.Name(c.id.PID)
			if err != nil {
				name = "?"
			}
			audit.info(fmt.Sprintf("scratch prune: kept %s — live icebath process pid=%d (%s) works inside", reprQuote(entry), c.id.PID, reprQuote(name)))
		}
	}
	kept, err := resolveAll(doomed)
	if err != nil {
		return nil, err
	}
	var remaining []reapCandidate
	for _, c := range victims {
		if slices.Contains(owned, c) {
			continue
		}
		if !exists(c.cwd) || slices.ContainsFunc(kept, func(d string) bool { return under(c.cwd, d) }) {
			remaining = append(remaining, c)
		}
	}
	if len(remaining) > 0 {
		signalVictims(remaining, audit)
	}
	return doomed, nil
}

// Resolved form of each path, the first resolution failure ending the list
func resolveAll(paths []string) ([]string, error) {
	out := make([]string, len(paths))
	for i, p := range paths {
		real, err := resolvePath(p)
		if err != nil {
			return nil, err
		}
		out[i] = real
	}
	return out, nil
}

// TERMs each victim, then KILLs those still running after the grace, recording every signal sent
func signalVictims(victims []reapCandidate, audit *pruneAudit) {
	ids := make([]proc.Identity, 0, len(victims))
	for _, c := range victims {
		ids = append(ids, c.id)
		name, err := proc.Name(c.id.PID)
		if err != nil || c.id.Signal(syscall.SIGTERM) != nil {
			continue
		}
		audit.info(fmt.Sprintf("scratch prune: sent TERM pid=%d name=%s cwd=%s", c.id.PID, reprQuote(name), reprQuote(c.cwd)))
	}
	wait, cancel := context.WithTimeout(context.Background(), reapGrace)
	survivors := proc.Await(wait, ids)
	cancel()
	for _, id := range survivors {
		if st, err := proc.StatusOf(id.PID); err == nil && st == proc.Zombie {
			continue
		}
		if id.Signal(syscall.SIGKILL) != nil {
			continue
		}
		audit.info(fmt.Sprintf("scratch prune: sent KILL pid=%d (still running after TERM)", id.PID))
	}
	slog.Info(fmt.Sprintf("scratch prune: reaped %d process(es) rooted in pruned entries", len(victims)))
}

// Reports whether id is registered itself or descends from a live registered process other than pid 1
func ownedByIcebath(table proc.Table, id proc.Identity, registered map[int]registration) bool {
	if registered[id.PID].matches(id) {
		return true
	}
	ancestors, err := table.Ancestors(id.PID)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(ancestors, func(a proc.Identity) bool { return a.PID != 1 && registered[a.PID].matches(a) })
}

// Reports whether a registered process is the incarnation id, a registration without a readable start matching its pid
func (r registration) matches(id proc.Identity) bool {
	return r.id.PID == id.PID && r.id.PID > 0 && (!r.known || r.id.Start == id.Start)
}

func registeredLive(home string) (map[int]registration, error) {
	root, err := RootOf(home)
	if err != nil {
		return nil, err
	}
	home = filepath.Clean(home)
	out := map[int]registration{}
	if entries, err := proc.ReadLedger(SpawnLedger.In(root)); err == nil {
		for _, e := range entries {
			if e.PID <= 0 {
				continue
			}
			if e.Purpose == "mcp-helper" {
				if dead, known := e.SpawnerDead(); known && dead {
					continue
				}
			}
			if e.StillRunning() {
				out[e.PID] = registered(e.PID)
			}
		}
	}
	checkpoints := []string{ProcessesFile.In(home)}
	if !samePath(home, root) {
		checkpoints = append(checkpoints, ProcessesFile.In(root))
	}
	for _, path := range checkpoints {
		var doc any
		if fileutil.ReadJSON(path, &doc) != nil {
			continue
		}
		list, _ := doc.([]any)
		for _, raw := range list {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if pid, ok := checkpointPID(entry); ok {
				out[pid] = registered(pid)
			}
		}
	}
	return out, nil
}

func registered(pid int) registration {
	if id, err := proc.Identify(pid); err == nil {
		return registration{id: id, known: true}
	}
	return registration{id: proc.Identity{PID: pid}}
}

func checkpointPID(entry map[string]any) (int, bool) {
	pid, ok := jsonInt(entry["pid"])
	if !ok || pid <= 0 {
		return 0, false
	}
	if scope, has := entry["pid_scope"]; has && scope != "host" {
		return 0, false
	}
	stamp := entry["host_start_time"]
	if stamp == nil {
		return pid, proc.Exists(pid)
	}
	want, ok := stamp.(json.Number)
	if !ok {
		return 0, false
	}
	fp, err := proc.Fingerprint(pid)
	if err != nil {
		return 0, false
	}
	f, err := want.Float64()
	return pid, err == nil && f == float64(fp)
}

// Whole JSON number as an int, false for any other value
func jsonInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	i, err := strconv.Atoi(string(n))
	return i, err == nil
}
