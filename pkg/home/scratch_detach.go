package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Hidden first argument a main or TestMain routes to RunScratchPrune through DispatchScratchPruneEntry before any other parsing
const ScratchPruneEntry = "__icebath_scratch_prune__"

// Latch proving this binary dispatches the entry, the only state in which a prune may re-execute it
var pruneEntryDispatched atomic.Bool

// Runs the prune and exits when argv carries the entry, otherwise records that this binary handles it
func DispatchScratchPruneEntry(ctx context.Context) {
	pruneEntryDispatched.Store(true)
	if len(os.Args) > 1 && os.Args[1] == ScratchPruneEntry {
		os.Exit(RunScratchPrune(ctx, os.Args[2:]))
	}
}

// Starts one detached prune by re-executing this binary, panicking when nothing in it dispatches the entry
func spawnPruneChild(scratch string) {
	if !pruneEntryDispatched.Load() {
		panic("scratch prune would re-execute a binary that never called home.DispatchScratchPruneEntry, call it first in main or TestMain")
	}
	exe, err := os.Executable()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("scratch prune: could not start the detached pass for %s: %s", scratch, err))
		return
	}
	chain, ok := LineageWithStartTimes()
	lineage := encodeLineage(chain, ok)
	start := func(breakaway bool) (*exec.Cmd, error) {
		cmd := exec.Command(exe, ScratchPruneEntry, scratch, lineage)
		cmd.SysProcAttr = detachAttr(breakaway)
		return cmd, cmd.Start()
	}
	cmd, err := start(true)
	if err != nil && runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission) {
		cmd, err = start(false)
	}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("scratch prune: could not start the detached pass for %s: %s", scratch, err))
		return
	}
	go func() { _ = cmd.Wait() }()
}

// Launcher ancestry as comma separated pid colon repr pairs, a dash when it could not be computed
func encodeLineage(chain []lineageEntry, ok bool) string {
	if !ok {
		return "-"
	}
	parts := make([]string, len(chain))
	for i, l := range chain {
		parts[i] = strconv.Itoa(l.pid) + ":" + strconv.FormatFloat(l.started, 'f', -1, 64)
	}
	return strings.Join(parts, ",")
}

// Launcher ancestry parsed from its encoded form, failing on an entry that is not an integer pid and a numeric start joined by one colon
func decodeLineage(text string) ([]lineageEntry, error) {
	var chain []lineageEntry
	for _, item := range strings.Split(text, ",") {
		pidText, startText, ok := strings.Cut(item, ":")
		if !ok || strings.Contains(startText, ":") {
			return nil, fmt.Errorf("launcher lineage entry %q is not pid:start", item)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil {
			return nil, fmt.Errorf("launcher lineage entry %q: pid %q is not an integer", item, pidText)
		}
		started, err := anyutil.ToFloat(startText)
		if err != nil {
			return nil, fmt.Errorf("launcher lineage entry %q: start: %w", item, err)
		}
		chain = append(chain, lineageEntry{pid, started})
	}
	return chain, nil
}

// Detached child entry running one prune of the scratch dir while sparing the launcher ancestry
func RunScratchPrune(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "%s takes the scratch dir and the launcher lineage\n", ScratchPruneEntry)
		return 1
	}
	scratch, lineage := args[0], args[1]
	err := func() error {
		if lineage == "-" {
			launcherLineage, launcherLineageKnown = nil, false
		} else {
			chain, err := decodeLineage(lineage)
			if err != nil {
				return err
			}
			launcherLineage, launcherLineageKnown = chain, true
		}
		_, err := PruneScratchDir(ctx, scratch, ScratchMaxIdleHours)
		return err
	}()
	if err != nil {
		audit := openPruneLog(pruneLogPath(scratch))
		audit.info(fmt.Sprintf("scratch prune: detached pass failed: %s", logSafe(errchain.OSText(err))))
	}
	return 0
}
