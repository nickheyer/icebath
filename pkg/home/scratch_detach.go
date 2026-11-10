package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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
		slog.Debug(fmt.Sprintf("scratch prune: could not start the detached pass for %s: %s", scratch, err))
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
		slog.Debug(fmt.Sprintf("scratch prune: could not start the detached pass for %s: %s", scratch, err))
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
		parts[i] = strconv.Itoa(l.pid) + ":" + pyrepr.Float(l.started)
	}
	return strings.Join(parts, ",")
}

// Launcher ancestry parsed from its encoded form with the errors int and float raise
func decodeLineage(text string) ([]lineageEntry, error) {
	var chain []lineageEntry
	for _, item := range strings.Split(text, ",") {
		parts := strings.Split(item, ":")
		if len(parts) != 2 {
			if len(parts) < 2 {
				return nil, &pyint.Error{Kind: "ValueError", Msg: fmt.Sprintf("not enough values to unpack (expected 2, got %d)", len(parts))}
			}
			return nil, &pyint.Error{Kind: "ValueError", Msg: "too many values to unpack (expected 2)"}
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, &pyint.Error{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.StrRepr(parts[0])}
		}
		started, err := pyint.Float(parts[1])
		if err != nil {
			return nil, err
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
		audit := openPruneLog(filepath.Join(filepath.Dir(filepath.Dir(filepath.Clean(scratch))), "logs", "scratch-prune.log"))
		class, msg := excInfo(err)
		audit.info(fmt.Sprintf("scratch prune: detached pass failed (%s: %s)", class, pyrepr.StrRepr(msg)))
	}
	return 0
}
