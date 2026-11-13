package home

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/proc"
)

// Hidden first argument that makes a binary importing this package run one detached scratch prune and exit
const pruneChildArg = "__icebath_scratch_prune__"

// Ancestry of this process as comma separated pid colon start pairs, a dash when it cannot be read
func encodeLineage() string {
	table, err := proc.ReadTable()
	if err != nil {
		return "-"
	}
	self, ok := table.Lookup(os.Getpid())
	if !ok {
		return "-"
	}
	ancestors, err := table.Ancestors(self.PID)
	if err != nil {
		return "-"
	}
	parts := []string{formatIdentity(self.Identity())}
	for _, a := range ancestors {
		parts = append(parts, formatIdentity(a))
	}
	return strings.Join(parts, ",")
}

// Pid colon start text of id, the start written so it parses back to the same value
func formatIdentity(id proc.Identity) string {
	return strconv.Itoa(id.PID) + ":" + strconv.FormatFloat(id.Start, 'g', -1, 64)
}

// Launcher ancestry from its encoded form, unknown for a dash
func decodeLineage(text string) (lineage, error) {
	if text == "-" {
		return lineage{}, nil
	}
	l := lineage{known: true}
	for _, item := range strings.Split(text, ",") {
		pidText, startText, ok := strings.Cut(item, ":")
		if !ok {
			return lineage{}, fmt.Errorf("lineage item %q has no start", item)
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			return lineage{}, fmt.Errorf("lineage item %q: %w", item, err)
		}
		start, err := strconv.ParseFloat(startText, 64)
		if err != nil {
			return lineage{}, fmt.Errorf("lineage item %q: %w", item, err)
		}
		l.chain = append(l.chain, proc.Identity{PID: pid, Start: start})
	}
	return l, nil
}

// Starts one detached prune of scratch by running this binary again, the ancestry captured before the launcher exits
func spawnPrune(scratch string) {
	exe, err := os.Executable()
	if err != nil {
		slog.Debug(fmt.Sprintf("scratch prune: could not start the detached pass for %s: %v", scratch, err))
		return
	}
	cmd := exec.Command(exe, pruneChildArg, scratch, encodeLineage())
	if err := proc.StartDetached(cmd); err != nil {
		slog.Debug(fmt.Sprintf("scratch prune: could not start the detached pass for %s: %v", scratch, err))
		return
	}
	go func() { _ = cmd.Wait() }()
}

// Runs the detached prune and exits before main ever starts when this process was launched as one
func init() {
	if len(os.Args) > 1 && os.Args[1] == pruneChildArg {
		os.Exit(runPruneChild(os.Args[2:]))
	}
}

// One prune of the scratch directory in args sparing the launcher lineage, returning the exit status
func runPruneChild(args []string) int {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "%s takes the scratch directory and the launcher lineage\n", pruneChildArg)
		return 2
	}
	scratch := filepath.Clean(args[0])
	from, err := decodeLineage(args[1])
	if err != nil {
		audit := openPruneLog(ScratchPruneLog.In(filepath.Dir(filepath.Dir(scratch))))
		audit.info(fmt.Sprintf("scratch prune: detached pass failed (%v)", err))
		return 0
	}
	pruneScratch(scratch, ScratchMaxIdle, from)
	return 0
}
