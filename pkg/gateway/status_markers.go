package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	takeoverMarkerFilename    = ".gateway-takeover.json"
	takeoverMarkerTTL         = 60
	plannedStopMarkerFilename = ".gateway-planned-stop.json"
	plannedStopMarkerTTL      = 60
	plannedStopMutexWait      = 2 * time.Second
)

// Seams over the takeover steps hermes tests patch
var (
	statusSnapshotGatewayChildren = snapshotGatewayChildrenLive
	statusTerminatePid            = TerminatePid
	statusSleep                   = time.Sleep
	statusProcessCreateTime       = proc.Birth
)

func pyFloat(v any) (float64, error) { return pyint.Float(v) }

func pyRound(f float64) float64 { return math.RoundToEven(f) }

// Takeover marker path in the canonical target home, the process home when empty
func takeoverMarkerPath(hermesHome string) (string, error) {
	if hermesHome == "" {
		hermesHome = processHermesHome()
	}
	h, err := canonicalHermesHome(hermesHome)
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(h, takeoverMarkerFilename), nil
}

func plannedStopMarkerPath() string {
	return filepath.Join(processHermesHome(), plannedStopMarkerFilename)
}

// Whether a written_at stamp is older than ttl seconds, unparseable or naive stamps counting as stale
func markerIsStale(writtenAt any, ttl int) bool {
	s, ok := writtenAt.(string)
	if !ok {
		return true
	}
	stamp, aware, err := pydatetime.ParseISOFormat(s)
	if err != nil || !aware {
		return true
	}
	return time.Since(stamp).Seconds() > float64(ttl)
}

// Usable marker record with its target pid and start time, malformed or expired markers unlinked
func readLivePidMarker(path string, ttl int) (*pyjson.Object, int, any, bool, error) {
	record := readJSONFile(path, false)
	if record == nil || record.Len() == 0 {
		return nil, 0, nil, false, nil
	}
	targetPid, ok := pidFromRecord(record, "target_pid")
	if !ok || markerIsStale(recGet(record, "written_at"), ttl) {
		keep := false
		if accepted, isBool := recGet(record, "accepted").(bool); isBool && accepted {
			paused, err := updatePauseOnDisk()
			if err != nil {
				return nil, 0, nil, false, err
			}
			keep = paused
		}
		if !keep {
			unlinkQuietly(path)
		}
		return nil, 0, nil, false, nil
	}
	return record, targetPid, recGet(record, "target_start_time"), true, nil
}

// Whether a marker names this process, start times compared only when both are known
func pidMarkerNamesSelf(targetPid int, targetStartTime any) bool {
	if targetPid != statusGetpid() {
		return false
	}
	ours := statusProcessStartTime(targetPid)
	return targetStartTime == nil || ours == nil || pyEq(targetStartTime, ours)
}

// Consumes a pid marker naming this process home and pid, unlinking it unless kept
func consumePidMarkerForSelf(path string, ttl int, onConsume func(string, *pyjson.Object) error, keep bool) (bool, error) {
	record, targetPid, targetStart, ok, err := readLivePidMarker(path, ttl)
	if err != nil || !ok {
		return false, err
	}
	ourHome := processHermesHome()
	if targetHome, present := record.Get("target_hermes_home"); present && targetHome != nil {
		s, isStr := targetHome.(string)
		if !isStr {
			return false, nil
		}
		same, err := sameHermesHome(s, ourHome)
		if err != nil {
			return false, err
		}
		if !same {
			return false, nil
		}
	} else if replacer, present := record.Get("replacer_hermes_home"); present && replacer != nil {
		s, isStr := replacer.(string)
		if !isStr {
			return false, fmt.Errorf("TypeError: expected str, bytes or os.PathLike object")
		}
		same, err := sameHermesHome(s, ourHome)
		if err != nil {
			return false, err
		}
		if !same {
			return false, nil
		}
	}
	matches := pidMarkerNamesSelf(targetPid, targetStart)
	if matches && onConsume != nil {
		if err := onConsume(path, record); err != nil {
			return false, err
		}
	}
	if !keep {
		unlinkQuietly(path)
	}
	return matches, nil
}

// Records that targetPid is being replaced by this process, the target start time captured unless given
func WriteTakeoverMarker(ctx context.Context, targetPid int, targetHome string, targetStartTime any, startTimeGiven bool) bool {
	markerHome := targetHome
	if markerHome == "" {
		markerHome = processHermesHome()
	}
	markerHome, err := canonicalHermesHome(markerHome)
	if err != nil {
		return false
	}
	if !startTimeGiven {
		targetStartTime = statusProcessStartTime(targetPid)
	}
	replacerHome, err := canonicalHermesHome(processHermesHome())
	if err != nil {
		return false
	}
	path, err := takeoverMarkerPath(markerHome)
	if err != nil {
		return false
	}
	record := pyjson.NewObject()
	record.Set("target_pid", int64(targetPid))
	record.Set("target_start_time", targetStartTime)
	record.Set("target_hermes_home", markerHome)
	record.Set("replacer_pid", int64(statusGetpid()))
	record.Set("replacer_hermes_home", replacerHome)
	record.Set("written_at", utcNowISO())
	return writeMarker(ctx, path, record)
}

// Atomically writes a marker record, false on any OS failure
func writeMarker(ctx context.Context, path string, record *pyjson.Object) bool {
	return writeJSONFile(ctx, path, record) == nil
}

// Consumes the takeover marker, true for a planned takeover of this process
func ConsumeTakeoverMarkerForSelf() (bool, error) {
	path, err := takeoverMarkerPath("")
	if err != nil {
		return false, err
	}
	return consumePidMarkerForSelf(path, takeoverMarkerTTL, nil, false)
}

// Removes the takeover marker of targetHome, the process home when empty
func ClearTakeoverMarker(targetHome string) {
	if path, err := takeoverMarkerPath(targetHome); err == nil {
		unlinkQuietly(path)
	}
}

// Verified live scoped lock owner whose pid record in its own home corroborates pid, start time and home
func validatedScopedLockGatewayOwner(record *pyjson.Object) (int, any, string, bool) {
	if record == nil || !recordLooksLikeGateway(record) {
		return 0, nil, "", false
	}
	ownerPid, ok := pidFromRecord(record, "pid")
	ownerStart := recGet(record, "start_time")
	rawHome, isStr := recGet(record, "hermes_home").(string)
	if !ok || ownerPid <= 0 || ownerPid == statusGetpid() || !pyIsInt(ownerStart) || !isStr ||
		pystr.Strip(rawHome) == "" || !pypath.IsAbs(pypath.Expanduser(rawHome)) {
		return 0, nil, "", false
	}
	targetHome, err := canonicalHermesHome(rawHome)
	if err != nil {
		return 0, nil, "", false
	}
	if scopedLockOwnerState(ownerPid, ownerStart) != "same" {
		return 0, nil, "", false
	}
	if live, ok := statusReadProcessCmdline(ownerPid); ok && !LooksLikeGatewayRuntimeCommandLine(live) {
		return 0, nil, "", false
	}
	pidRecord := readJSONFile(pypath.PathJoin(targetHome, "gateway.pid"), false)
	if pidRecord == nil {
		pidRecord = pyjson.NewObject()
	}
	pidHome, homeIsStr := recGet(pidRecord, "hermes_home").(string)
	recordPid, recordOK := pidFromRecord(pidRecord, "pid")
	if !recordLooksLikeGateway(pidRecord) || !recordOK || recordPid != ownerPid ||
		!pyEq(recGet(pidRecord, "start_time"), ownerStart) || !homeIsStr {
		return 0, nil, "", false
	}
	if same, err := sameHermesHome(pidHome, targetHome); err != nil || !same {
		return 0, nil, "", false
	}
	return ownerPid, ownerStart, targetHome, true
}

// State of a validated owner, same, exited when gone or reused, unknown without a start time
func scopedLockOwnerState(ownerPid int, ownerStart any) string {
	if !statusPidExists(ownerPid) {
		return "exited"
	}
	live := statusProcessStartTime(ownerPid)
	if live == nil {
		return "unknown"
	}
	if pyEq(live, ownerStart) {
		return "same"
	}
	return "exited"
}

// Bounded identity aware wait for an owner to exit, reporting whether a force kill is still safe
func waitForScopedLockOwnerExit(ownerPid int, ownerStart any, attempts int, delay time.Duration) (bool, bool) {
	for range max(0, attempts) {
		switch scopedLockOwnerState(ownerPid, ownerStart) {
		case "exited":
			return true, false
		case "unknown":
			return false, false
		}
		statusSleep(max(0, delay))
	}
	return false, scopedLockOwnerState(ownerPid, ownerStart) == "same"
}

// One descendant of a gateway with its creation time so a recycled pid is never signalled
type gatewayChild struct {
	PID   int
	Birth float64
}

// Live descendants of pid snapshotted while the parent lives, empty on Windows where taskkill tree kills
func snapshotGatewayChildrenLive(pid int) []gatewayChild {
	if statusIsWindows {
		return nil
	}
	table, err := proc.Snapshot()
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not snapshot children of gateway PID %d", pid))
		return nil
	}
	var out []gatewayChild
	for _, child := range table.Descendants(pid) {
		if birth, err := proc.Birth(child); err == nil {
			out = append(out, gatewayChild{child, birth})
		}
	}
	return out
}

// Terminates the orphaned descendants of a dead gateway, killing survivors after the timeout, returning the count
func ReapGatewayChildren(children []gatewayChild, parentPid int, timeout time.Duration) int {
	if statusIsWindows || len(children) == 0 {
		return 0
	}
	running := func(c gatewayChild) bool {
		birth, err := proc.Birth(c.PID)
		return err == nil && birth == c.Birth
	}
	var live []gatewayChild
	for _, child := range children {
		if !running(child) || proc.Zombie(child.PID) {
			continue
		}
		if ppid, ok := proc.PPID(child.PID); ok && ppid == parentPid {
			slog.Debug(fmt.Sprintf("Skipping child PID %d of old gateway %d: parent still appears alive", child.PID, parentPid))
			continue
		}
		if err := killPid(child.PID, false); err != nil {
			if !errors.Is(err, os.ErrProcessDone) {
				slog.Debug(fmt.Sprintf("Could not terminate child PID %d of old gateway %d", child.PID, parentPid))
			}
			continue
		}
		live = append(live, child)
	}
	if len(live) == 0 {
		return 0
	}
	deadline := time.Now().Add(max(0, timeout))
	reaped := 0
	for {
		var still []gatewayChild
		for _, c := range live {
			if running(c) && !proc.Zombie(c.PID) {
				still = append(still, c)
			} else {
				reaped++
			}
		}
		live = still
		if len(live) == 0 || !time.Now().Before(deadline) {
			break
		}
		statusSleep(min(40*time.Millisecond, time.Until(deadline)))
	}
	for _, child := range live {
		if err := killPid(child.PID, true); err != nil {
			slog.Debug(fmt.Sprintf("Could not force-kill child PID %d of old gateway %d", child.PID, parentPid))
			continue
		}
		reaped++
	}
	if reaped > 0 {
		slog.Info(fmt.Sprintf("Reaped %d orphaned child process(es) of replaced gateway PID %d.", reaped, parentPid))
	}
	return reaped
}

// Terminates one verified scoped lock holder for replace, the owner pid once that exact identity exited
func TakeOverScopedLockHolder(ctx context.Context, record *pyjson.Object, gracefulAttempts, forceAttempts int) (int, bool) {
	ownerPid, ownerStart, targetHome, ok := validatedScopedLockGatewayOwner(record)
	if !ok {
		return 0, false
	}
	children := statusSnapshotGatewayChildren(ownerPid)
	if !WriteTakeoverMarker(ctx, ownerPid, targetHome, ownerStart, true) {
		return 0, false
	}
	replaced, ok := terminateVerifiedOwner(ownerPid, ownerStart, gracefulAttempts, forceAttempts)
	ClearTakeoverMarker(targetHome)
	if ok {
		ReapGatewayChildren(children, ownerPid, 5*time.Second)
	}
	return replaced, ok
}

// Bounded SIGTERM then guarded SIGKILL of a verified owner, the pid once it exited
func terminateVerifiedOwner(ownerPid int, ownerStart any, gracefulAttempts, forceAttempts int) (int, bool) {
	switch scopedLockOwnerState(ownerPid, ownerStart) {
	case "exited":
		return ownerPid, true
	case "same":
	default:
		return 0, false
	}
	steps := []struct {
		attempts int
		delay    time.Duration
		force    bool
	}{{gracefulAttempts, 500 * time.Millisecond, false}, {forceAttempts, 250 * time.Millisecond, true}}
	for _, step := range steps {
		var expected any
		if step.force {
			expected = ownerStart
		}
		if err := statusTerminatePid(ownerPid, step.force, expected); err != nil {
			if errors.Is(err, os.ErrProcessDone) {
				return ownerPid, true
			}
			return 0, false
		}
		exited, safeToForce := waitForScopedLockOwnerExit(ownerPid, ownerStart, step.attempts, step.delay)
		if exited {
			return ownerPid, true
		}
		if !safeToForce {
			return 0, false
		}
	}
	return 0, false
}

// Records that targetPid is being stopped intentionally so its exit is clean
func WritePlannedStopMarker(ctx context.Context, targetPid int) bool {
	record := pyjson.NewObject()
	record.Set("target_pid", int64(targetPid))
	record.Set("target_start_time", statusProcessStartTime(targetPid))
	record.Set("stopper_pid", int64(statusGetpid()))
	record.Set("written_at", utcNowISO())
	return writeMarker(ctx, plannedStopMarkerPath(), record)
}

// Whether an update pause record or a recovery claim is on disk, true when the directory cannot be listed
func updatePauseOnDisk() (bool, error) {
	record := updaterecord.PauseRecordPath()
	entries, err := os.ReadDir(pypath.PathParent(record))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, nil
	}
	name := pypath.PathName(record)
	for _, e := range entries {
		if e.Name() == name || (strings.HasPrefix(e.Name(), name+".") && strings.HasSuffix(e.Name(), ".claim")) {
			return true, nil
		}
	}
	return false, nil
}

// Checkpoints a consumed planned stop into a pause that appeared since the consumer first looked
func checkpointPlannedStop(ctx context.Context, path string, record *pyjson.Object) error {
	paused, err := updatePauseOnDisk()
	if err != nil || !paused {
		return err
	}
	_, release, err := updaterecord.PauseMutex(ctx, plannedStopMutexWait)
	if err != nil {
		return err
	}
	defer release()
	return updaterecord.MarkStopConsumed(path, record, sameHermesHome)
}

// Whether this process is being intentionally stopped, the request kept as an accepted receipt when the checkpoint fails
func ConsumePlannedStopMarkerForSelf() (bool, error) {
	path := plannedStopMarkerPath()
	matched, err := func() (bool, error) {
		paused, err := updatePauseOnDisk()
		if err != nil {
			return false, err
		}
		mctx := context.Background()
		if !paused {
			unlinkQuietly(updaterecord.PauseAcceptedPath(path))
		} else {
			held, release, err := updaterecord.PauseMutex(mctx, plannedStopMutexWait)
			if err != nil {
				return false, err
			}
			defer release()
			mctx = held
		}
		checkpoint := func(p string, r *pyjson.Object) error { return checkpointPlannedStop(mctx, p, r) }
		return consumePidMarkerForSelf(path, plannedStopMarkerTTL, checkpoint, false)
	}()
	if err == nil {
		return matched, nil
	}
	if !isOSError(err) {
		return false, err
	}
	slog.Warn(fmt.Sprintf("Planned-stop checkpoint skipped (%v); the stop request stays on disk", err))
	return consumePidMarkerForSelf(plannedStopMarkerPath(), plannedStopMarkerTTL, updaterecord.MarkStopAccepted, true)
}

// Whether a live planned stop marker names this process, never unlinking a matching marker
func PlannedStopMarkerTargetsSelf() (bool, error) {
	_, targetPid, targetStart, ok, err := readLivePidMarker(plannedStopMarkerPath(), plannedStopMarkerTTL)
	if err != nil || !ok {
		return false, err
	}
	return pidMarkerNamesSelf(targetPid, targetStart), nil
}
