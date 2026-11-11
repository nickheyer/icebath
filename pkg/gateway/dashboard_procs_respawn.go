package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Age under which an unowned Desktop local serve is still waiting for its lock record
const reapMinAgeSeconds = 180.0

// Process scan, parent lookup and descendant snapshot of the orphan reap, swapped by tests
var (
	dashboardScan             = scanDashboardProcesses
	dashboardProcessPpid      = processPpid
	dashboardPosixDescendants = posixDescendants
)

// Index of the first serve or dashboard token of an argv, false when neither appears
func dashboardSubcommandIndex(argv []string) (int, bool) {
	for i, tok := range argv {
		if tok == "serve" || tok == "dashboard" {
			return i, true
		}
	}
	return 0, false
}

// True for Desktop style serve or dashboard backends started on port zero
func isEphemeralPortZeroBackend(argv []string) bool {
	if _, ok := dashboardSubcommandIndex(argv); !ok {
		return false
	}
	for i, tok := range argv {
		if tok == "--port" && i+1 < len(argv) && argv[i+1] == "0" {
			return true
		}
		if strings.HasPrefix(tok, "--port=") && pystr.Strip(strings.SplitN(tok, "=", 2)[1]) == "0" {
			return true
		}
	}
	return false
}

// Profile flags before the subcommand plus the serve or dashboard tail, the key that collapses duplicate launches
func normalizeDashboardCmdline(argv []string) []string {
	idx, ok := dashboardSubcommandIndex(argv)
	if !ok {
		return slices.Clone(argv)
	}
	prefix := []string{}
	for i := 0; i < idx; i++ {
		tok := argv[i]
		if (tok == "--profile" || tok == "-p") && i+1 < idx {
			prefix = append(prefix, tok, argv[i+1])
			i++
			continue
		}
		if strings.HasPrefix(tok, "--profile=") {
			prefix = append(prefix, tok)
		}
	}
	return append(prefix, argv[idx:]...)
}

// Symlink resolved absolute form of a home, the spelling given when it cannot be resolved
func resolvedDashboardHome(h string) string {
	if resolved, err := pypath.Resolve(h); err == nil {
		return resolved
	}
	return h
}

// Owner key of a respawn, the profile of a profiles home, the home itself, else the profile flag
func profileKeyForRespawn(argv []string, hermesHome string) string {
	if hermesHome != "" {
		resolved := resolvedDashboardHome(hermesHome)
		name, parent := filepath.Base(resolved), filepath.Base(filepath.Dir(resolved))
		if parent == "profiles" && name != "" && filepath.Dir(resolved) != resolved {
			return "profile:" + name
		}
		return "home:" + NormalizedHomeForCompare(hermesHome)
	}
	if profile, ok := argvProfileFlagValue(argv); ok && profile != "" {
		return "profile:" + profile
	}
	return "profile:default"
}

// Stopped manual backend with the argv and home captured before its kill
type respawnCandidate struct {
	pid  int
	argv []string
	home string
}

// Argvs to respawn after an update, no port zero or foreign home backends, one per command line and profile
func filterDashboardRespawnCandidates(ctx context.Context, candidates []respawnCandidate, ownHome *string) [][]string {
	own := ""
	if ownHome != nil {
		own = *ownHome
	} else {
		own = home.Dir(ctx)
	}
	ownKey := ""
	if own != "" {
		ownKey = NormalizedHomeForCompare(own)
	}
	selected := [][]string{}
	seenCmdlines := map[string]bool{}
	seenProfiles := map[string]bool{}
	for _, c := range candidates {
		if len(c.argv) == 0 || isEphemeralPortZeroBackend(c.argv) {
			continue
		}
		if ownKey != "" && c.home != "" && NormalizedHomeForCompare(c.home) != ownKey {
			continue
		}
		norm := strings.Join(normalizeDashboardCmdline(c.argv), "\x00")
		profileKey := profileKeyForRespawn(c.argv, c.home)
		if seenCmdlines[norm] || seenProfiles[profileKey] {
			continue
		}
		seenCmdlines[norm] = true
		seenProfiles[profileKey] = true
		selected = append(selected, slices.Clone(c.argv))
	}
	return selected
}

// Value of a flag given as two tokens or joined with an equals sign, false when absent
func dashboardFlagValue(tokens []string, flag string) (string, bool) {
	for i, tok := range tokens {
		if tok == flag && i+1 < len(tokens) {
			return tokens[i+1], true
		}
		if strings.HasPrefix(tok, flag+"=") {
			return tok[len(flag)+1:], true
		}
	}
	return "", false
}

// True for the Desktop local shape, a serve holder on a loopback host with port zero
func isDesktopLocalServeCmdline(command string) bool {
	if mode, _ := HolderSubcommand(command); mode != "serve" {
		return false
	}
	tokens := pystr.SplitWhitespace(strings.ToLower(command), -1)
	host, _ := dashboardFlagValue(tokens, "--host")
	port, _ := dashboardFlagValue(tokens, "--port")
	return (host == "127.0.0.1" || host == "localhost") && port == "0"
}

// Pids an orphan reap matched, killed and could not signal
type DashboardReapResult struct {
	Matched []int
	Killed  []int
	Failed  []int
}

// Seams of one orphan reap, each zero value standing for the host behaviour
type DashboardReapOptions struct {
	Reason            string
	Kill              func(pid int, force bool) error
	Sleep             func(time.Duration)
	LockOwnedPIDs     func() (map[int]bool, error)
	ProcessAgeSeconds func(pid int) (float64, error)
}

// Python list text of pids
func pidListText(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Kills parentless Desktop local serve backends past the minimum age that no lock record or Desktop claims
func ReapOrphanedDesktopLocalServes(ctx context.Context, w io.Writer, o DashboardReapOptions) (result DashboardReapResult) {
	empty := DashboardReapResult{Matched: []int{}, Killed: []int{}, Failed: []int{}}
	reason := o.Reason
	if reason == "" {
		reason = "orphaned desktop-local icebath serve"
	}
	kill, sleep, age := o.Kill, o.Sleep, o.ProcessAgeSeconds
	if kill == nil {
		kill = dashboardKillPid
	}
	if sleep == nil {
		sleep = dashboardSleep
	}
	if age == nil {
		age = processAgeSeconds
	}
	lockOwned := o.LockOwnedPIDs
	if lockOwned == nil {
		lockOwned = func() (map[int]bool, error) { return dashboardLockOwnedServePIDs(), nil }
	}
	if dashboardGOOS == "windows" {
		return empty
	}
	ownedPIDs := func() (owned map[int]bool) {
		defer func() {
			if recover() != nil {
				owned = map[int]bool{}
			}
		}()
		pids, err := lockOwned()
		if err != nil {
			return map[int]bool{}
		}
		return pids
	}
	isStaleOrphan := func(pid int) (stale bool) {
		defer func() {
			if recover() != nil {
				stale = false
			}
		}()
		seconds, err := age(pid)
		return err == nil && seconds >= reapMinAgeSeconds
	}
	exclude := excludePidsFromEnv()
	exclude[dashboardScanGetpid()] = true
	for pid := range ownedPIDs() {
		exclude[pid] = true
	}
	exclude[dashboardGetppid()] = true
	scanned := dashboardScan(ctx, exclude)
	ownedNow := ownedPIDs()
	matched := []int{}
	for _, row := range scanned {
		if !isDesktopLocalServeCmdline(row.command) || ownedNow[row.pid] {
			continue
		}
		if ppid, ok := dashboardProcessPpid(ctx, row.pid); !ok || (ppid != 0 && ppid != 1) {
			continue
		}
		if isStaleOrphan(row.pid) {
			matched = append(matched, row.pid)
		}
	}
	if len(matched) == 0 {
		return empty
	}
	descendants := dashboardPosixDescendants(ctx, matched)
	killed, failed := []int{}, []int{}
	for _, pid := range matched {
		if err := kill(pid, false); err != nil && !errors.Is(err, os.ErrProcessDone) {
			failed = append(failed, pid)
		}
	}
	sleep(1500 * time.Millisecond)
	for _, pid := range matched {
		if slices.Contains(failed, pid) {
			continue
		}
		if dashboardPidExists(pid) {
			if err := kill(pid, true); err != nil && !errors.Is(err, os.ErrProcessDone) {
				failed = append(failed, pid)
				continue
			}
		}
		killed = append(killed, pid)
	}
	for _, d := range descendants {
		if !slices.Contains(failed, d.root) && d.start != nil && dashboardStartTime(d.pid) == d.start {
			_ = kill(d.pid, true)
		}
	}
	shown := killed
	if len(shown) == 0 {
		shown = matched
	}
	fmt.Fprintf(w, "⟲ Reaped %d orphaned desktop-local serve backend(s) (%s): %s\n", len(killed), reason, pidListText(shown))
	return DashboardReapResult{Matched: matched, Killed: killed, Failed: failed}
}
