package updater

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

var (
	tmpSiblingRE = regexp.MustCompile(`^\.icebath-update-in-progress\.(\d+)(?:\.\d+)?\.tmp$`)
	tmpSequence  atomic.Int64
	// The marker already exists where the temp was to be linked
	errLinkExists = errors.New("marker exists")
)

// ClaimResult is the outcome of an exclusive marker claim
type ClaimResult struct {
	OK       bool
	Body     string
	Owner    *LiveUpdate
	Existing *MarkerInspection
	Error    string
}

func markerBody(pid int, startedAt int64, ct *float64, runID string) string {
	ctLine := ""
	if ct != nil {
		ctLine = "ct:" + strconv.FormatFloat(*ct, 'f', 3, 64) + "\n"
	}
	runLine := ""
	if runID != "" {
		runLine = "run:" + runID + "\n"
	}
	return fmt.Sprintf("%d\n%d\n%s%s", pid, startedAt, ctLine, runLine)
}

// Claims the marker for pid exclusively, never over an existing claim
func writeUpdateMarker(home string, pid int) ClaimResult {
	acquiredAt := int64(math.Floor(nowMillis() / 1000))
	return createMarkerExclusive(home, markerBody(pid, acquiredAt, processCreated(pid), ""))
}

// Why a marker pre-write was skipped
func describeSkippedPrewrite(result ClaimResult) string {
	if result.Owner != nil {
		owner := "(claim being written)"
		if result.Owner.PID != 0 {
			owner = strconv.Itoa(result.Owner.PID)
		}
		return fmt.Sprintf("an update owned by pid %s holds the marker", owner)
	}
	if result.Existing != nil {
		return fmt.Sprintf("a %s marker exists; the staged updater reclaims it under its own lock", result.Existing.State)
	}
	if result.Error != "" {
		return result.Error
	}
	return "unknown error"
}

// Deletes temp siblings whose writer is gone
func reclaimDeadTmpSiblings(home string) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return
	}
	for _, entry := range entries {
		match := tmpSiblingRE.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		if pid, err := strconv.Atoi(match[1]); err == nil && pid != 0 && !backend.IsPidAlive(pid) {
			_ = os.RemoveAll(filepath.Join(home, entry.Name()))
		}
	}
}

func writeSynced(f *os.File, body string) error {
	if _, err := f.WriteString(body); err != nil {
		return err
	}
	return f.Sync()
}

// Publishes body at file only when nothing is there, a hard link of a synced temp first
func publishExclusive(file, body string) (bool, error) {
	tmp := fmt.Sprintf("%s.%d.%d.tmp", file, os.Getpid(), tmpSequence.Add(1))
	linked := false
	err := func() error {
		defer os.RemoveAll(tmp)
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		werr := writeSynced(f, body)
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
		if err := os.Link(tmp, file); err != nil {
			if errors.Is(err, os.ErrExist) {
				return errLinkExists
			}
			return nil
		}
		linked = true
		return nil
	}()
	if err == errLinkExists {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if linked {
		return true, nil
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	werr := writeSynced(f, body)
	cerr := f.Close()
	if werr != nil {
		return false, werr
	}
	return true, cerr
}

// Exclusive claim of the marker, naming the live owner or the existing body when it is refused
func createMarkerExclusive(home, body string) ClaimResult {
	file := markerPath(home)
	reclaimDeadTmpSiblings(home)
	published, err := publishExclusive(file, body)
	if err != nil {
		return ClaimResult{Error: nodeErrorMessage(err)}
	}
	if published {
		return ClaimResult{OK: true, Body: body}
	}
	inspection := inspectUpdateMarker(home, nil, nil)
	switch inspection.State {
	case inspectionLive:
		return ClaimResult{Owner: &LiveUpdate{PID: inspection.LivePID, AgeMS: inspection.AgeMS}}
	case inspectionAbsent:
		return ClaimResult{Error: "the marker changed while it was being claimed"}
	}
	return ClaimResult{Existing: &inspection}
}

func conflictMessage(pid int, ageMS float64) string {
	if pid == 0 {
		return "Another update is starting right now. Wait for it to finish, then try again."
	}
	if !jsFinite(ageMS) {
		ageMS = 0
	}
	ageMS = math.Max(0, ageMS)
	mins := int64(math.Floor(ageMS / 60000))
	secs := int64(math.Floor(math.Mod(ageMS, 60000) / 1000))
	elapsed := fmt.Sprintf("%ds", secs)
	if mins > 0 {
		elapsed = fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("An update is already running (PID %d, started %s ago). Wait for it to finish, then try again.", pid, elapsed)
}

// HandoffConflict is a live foreign update that refuses a new hand-off
type HandoffConflict struct {
	PID     int
	AgeMS   float64
	Message string
}

// Live foreign update holding the marker, nil when none
func updateHandoffConflict(home string) *HandoffConflict {
	owner := readLiveUpdateMarker(home, nil)
	if owner == nil {
		return nil
	}
	return &HandoffConflict{PID: owner.PID, AgeMS: owner.AgeMS, Message: conflictMessage(owner.PID, owner.AgeMS)}
}

// Stable run id of one desktop hand-off
func makeHandoffRunID(pid int, nowMS int64) (string, error) {
	random := make([]byte, 2)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	id := fmt.Sprintf("desk-%d-%s-%s", pid, strconv.FormatInt(nowMS, 36), hex.EncodeToString(random))
	if !runIDRE.MatchString(id) {
		return "", fmt.Errorf("invalid hand-off run id %s", id)
	}
	return id, nil
}
