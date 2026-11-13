package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
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

// Tolerance an epoch start comparison allows when no boot relative start was recorded
const epochSlack = 2.0

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

// Answer of a probe for a recorded process that may be unable to decide
type liveness int8

// The recorded incarnation runs, is gone, or the probe could not tell
const (
	alive liveness = iota
	dead
	undecided
)

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

// One registry icebath writes, its path and whether it is the spawn ledger or a background process checkpoint
type registrySource struct {
	path   string
	ledger bool
}

// Live processes icebath registered for home in the spawn ledger and the process checkpoints, reused pids excluded
func registeredLive(home string) (map[int]registration, error) {
	root, err := RootOf(home)
	if err != nil {
		return nil, err
	}
	home = filepath.Clean(home)
	sources := []registrySource{{SpawnLedger.In(root), true}, {ProcessesFile.In(home), false}}
	if !samePath(home, root) {
		sources = append(sources, registrySource{ProcessesFile.In(root), false})
	}
	out := map[int]registration{}
	for _, src := range sources {
		var doc any
		if fileutil.ReadJSON(src.path, &doc) != nil {
			continue
		}
		list, _ := doc.([]any)
		for _, raw := range list {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if pid, ok := registeredPID(entry, src); ok {
				reg := registration{id: proc.Identity{PID: pid}}
				if id, err := proc.Identify(pid); err == nil {
					reg = registration{id: id, known: true}
				}
				out[pid] = reg
			}
		}
	}
	return out, nil
}

// Pid of a registry entry whose host process still runs as recorded, false for sandbox pids and orphaned helpers
func registeredPID(entry map[string]any, src registrySource) (int, bool) {
	pid, ok := jsonInt(entry["pid"])
	if !ok || pid <= 0 {
		return 0, false
	}
	if scope, has := entry["pid_scope"]; has && scope != "host" {
		return 0, false
	}
	if entry["purpose"] == "mcp-helper" && spawnerDead(entry) == dead {
		return 0, false
	}
	stamp := entry["host_start_time"]
	switch {
	case src.ledger:
		return pid, incarnation(pid, entry["create_time"], entryBoot(entry, "boot_start")) != dead
	case stamp == nil:
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

// Recorded start as a number, set false when absent and ok false when present but not a number
func recordedNumber(v any) (value float64, set, ok bool) {
	switch n := v.(type) {
	case nil:
		return 0, false, true
	case float64:
		return n, true, true
	case json.Number:
		f, err := n.Float64()
		return f, true, err == nil
	}
	return 0, true, false
}

// Boot relative start under key for an entry of the running boot, minus one for an earlier boot, nil unknown
func entryBoot(entry map[string]any, key string) any {
	recorded, ok := entry["boot_id"].(string)
	live, err := proc.BootID()
	if !ok || err != nil {
		return nil
	}
	if recorded == live {
		return entry[key]
	}
	return float64(-1)
}

// Maps a probe failure to dead for a vanished process and undecided for anything else
func probeFailure(err error) liveness {
	if errors.Is(err, proc.ErrNotFound) {
		return dead
	}
	return undecided
}

// Whether pid still runs as the process a recorded epoch and boot relative start describe, zombies counting as gone
func incarnation(pid int, createTime, boot any) liveness {
	if !proc.Exists(pid) {
		return dead
	}
	same, decided := true, false
	if b, set, ok := recordedNumber(boot); set {
		if !ok {
			return undecided
		}
		live, err := proc.SinceBoot(pid)
		switch {
		case err == nil:
			same, decided = live == b, true
		case !errors.Is(err, errors.ErrUnsupported):
			return probeFailure(err)
		}
	}
	if !decided {
		if c, set, ok := recordedNumber(createTime); set {
			if !ok {
				return undecided
			}
			started, err := proc.Started(pid)
			if err != nil {
				return probeFailure(err)
			}
			same = math.Abs(started-c) < epochSlack
		}
	}
	if !same {
		return dead
	}
	if zombiesExist {
		st, err := proc.StatusOf(pid)
		if err != nil {
			return probeFailure(err)
		}
		if st == proc.Zombie {
			return dead
		}
	}
	return alive
}

// Whether the spawner an entry records is provably gone, undecided when unrecorded or unprovable
func spawnerDead(entry map[string]any) liveness {
	pid, ok := jsonInt(entry["spawner_pid"])
	if !ok || pid <= 0 {
		return undecided
	}
	switch incarnation(pid, entry["spawner_create"], entryBoot(entry, "spawner_boot")) {
	case alive:
		return alive
	case dead:
		return dead
	}
	return undecided
}
