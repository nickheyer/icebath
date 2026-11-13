package updater

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	externalUnsupportedMessage = "Updates are managed by the package owner outside this app."
	commitBuildUpdateMessage   = "This build doesn't get updates. Ask the developer who gave it to you for a new build."
	handoffClaimPoll           = 200 * time.Millisecond
)

// UpdateCommit is one commit an update would bring in
type UpdateCommit struct {
	SHA     string  `json:"sha"`
	Summary string  `json:"summary"`
	Author  string  `json:"author"`
	At      float64 `json:"at"`
}

// UpdaterStatus is the status updates.check answers
type UpdaterStatus struct {
	Supported         bool                     `json:"supported"`
	Mechanism         string                   `json:"mechanism,omitempty"`
	UpdateAvailable   *bool                    `json:"updateAvailable,omitempty"`
	Branch            string                   `json:"branch,omitempty"`
	CurrentBranch     string                   `json:"currentBranch,omitempty"`
	Reason            string                   `json:"reason,omitempty"`
	Message           string                   `json:"message,omitempty"`
	Advice            string                   `json:"advice,omitempty"`
	Error             string                   `json:"error,omitempty"`
	Behind            *float64                 `json:"behind"`
	CurrentSHA        string                   `json:"currentSha,omitempty"`
	CurrentVersion    string                   `json:"currentVersion,omitempty"`
	Channel           string                   `json:"channel,omitempty"`
	ChannelSelectable *bool                    `json:"channelSelectable,omitempty"`
	Retirement        *ChannelRetirementStatus `json:"retirement,omitempty"`
	LatestTag         *string                  `json:"latestTag,omitempty"`
	TargetSHA         string                   `json:"targetSha,omitempty"`
	Commits           []UpdateCommit           `json:"commits,omitempty"`
	Dirty             *bool                    `json:"dirty,omitempty"`
	HermesRoot        string                   `json:"hermesRoot,omitempty"`
	FetchedAt         int64                    `json:"fetchedAt,omitempty"`
}

// ApplyResult is the result updates.apply answers
type ApplyResult struct {
	OK              bool   `json:"ok"`
	UpdateAvailable *bool  `json:"updateAvailable,omitempty"`
	Mechanism       string `json:"mechanism,omitempty"`
	Error           string `json:"error,omitempty"`
	Message         string `json:"message,omitempty"`
	Manual          bool   `json:"manual,omitempty"`
	Bundled         bool   `json:"bundled,omitempty"`
	Command         string `json:"command,omitempty"`
	HermesRoot      string `json:"hermesRoot,omitempty"`
	HandedOff       bool   `json:"handedOff,omitempty"`
	Updater         string `json:"updater,omitempty"`
}

// UpdaterStrategy is one update owner's check and apply
type UpdaterStrategy interface {
	Mechanism() string
	Check(force bool) (UpdaterStatus, error)
	Apply() (ApplyResult, error)
}

// Update mechanism the stamp names, a hand-off for self updating installs
func resolveUpdaterMechanism(isWindows bool, stamp *InstallStamp) string {
	if stamp != nil && stringOrEmpty(stamp.Source) == "commit-build" {
		return "external"
	}
	if stamp != nil && stamp.UpdateMechanism != "" && stamp.UpdateMechanism != "self" {
		return stamp.UpdateMechanism
	}
	if isWindows {
		return "windows-handoff"
	}
	return "posix-handoff"
}

// One packaged update owner kept across check and apply, a single apply at a time
type updateOperation struct {
	initialize func() (UpdaterStrategy, error)

	mu       sync.Mutex
	strategy UpdaterStrategy
	resolved bool
	applying bool
}

// Packaged strategy, resolved once and resolved again after a failed resolution
func (o *updateOperation) resolve() (UpdaterStrategy, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.resolved {
		return o.strategy, nil
	}
	strategy, err := o.initialize()
	if err != nil {
		return nil, err
	}
	o.strategy, o.resolved = strategy, true
	return strategy, nil
}

// Runs one apply, refusing a second while one runs and staying busy after a hand-off
func (o *updateOperation) apply(run func() (ApplyResult, error)) (ApplyResult, error) {
	o.mu.Lock()
	if o.applying {
		o.mu.Unlock()
		return ApplyResult{}, errors.New("An update is already in progress.")
	}
	o.applying = true
	o.mu.Unlock()
	result, err := run()
	if err != nil || !result.HandedOff {
		o.mu.Lock()
		o.applying = false
		o.mu.Unlock()
	}
	return result, err
}

// Strategy of an install whose updates belong to an outside package owner or none at all
type externalStrategy struct {
	commitBuild bool
}

func newExternalStrategy(stamp *InstallStamp) externalStrategy {
	return externalStrategy{commitBuild: stamp != nil && stringOrEmpty(stamp.Source) == "commit-build"}
}

func (externalStrategy) Mechanism() string { return "external" }

func (e externalStrategy) Check(bool) (UpdaterStatus, error) {
	status := UpdaterStatus{Mechanism: "external", Reason: "bundled-not-appinstaller", Message: externalUnsupportedMessage, FetchedAt: time.Now().UnixMilli()}
	if e.commitBuild {
		status.Reason, status.Message = "commit-build", commitBuildUpdateMessage
	}
	return status, nil
}

func (e externalStrategy) Apply() (ApplyResult, error) {
	if e.commitBuild {
		return ApplyResult{Mechanism: "external", Error: "commit-build", Message: commitBuildUpdateMessage}, nil
	}
	return ApplyResult{OK: true, Manual: true, Bundled: true, Mechanism: "external"}, nil
}

// Manual update command for the branch a checkout follows
func buildManualUpdateCommand(currentBranch string) string {
	if currentBranch != "" && currentBranch != "HEAD" && currentBranch != "main" {
		return "icebath update --branch " + currentBranch
	}
	return "icebath update"
}

// Commit the checkout's install stamp names, empty when it names none
func readStampedCommit(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "install-stamp.json"))
	if err != nil {
		return ""
	}
	var stamp map[string]any
	if json.Unmarshal(raw, &stamp) != nil {
		return ""
	}
	commit, _ := stamp["commit"].(string)
	return commit
}

// BridgeClaimResult is the outcome of the desktop's bridge claim before a script hand-off
type BridgeClaimResult struct {
	OK       bool
	Body     string
	Conflict *HandoffConflict
	Existing *MarkerInspection
	Error    string
}

// Claims the marker for this desktop under runID so the hand-off script can take it over
func claimBridgeMarker(home string, startedAt int64, runID string) BridgeClaimResult {
	pid := currentPID()
	body := markerBody(pid, startedAt, processCreated(pid), runID)
	if err := os.MkdirAll(home, 0o777); err != nil {
		return BridgeClaimResult{Error: nodeErrorMessage(err)}
	}
	result := createMarkerExclusive(home, body)
	if result.OK || result.Owner == nil {
		return BridgeClaimResult{OK: result.OK, Body: result.Body, Existing: result.Existing, Error: result.Error}
	}
	return BridgeClaimResult{Conflict: &HandoffConflict{PID: result.Owner.PID, AgeMS: result.Owner.AgeMS, Message: conflictMessage(result.Owner.PID, result.Owner.AgeMS)}}
}

// PID of the hand-off script that took the marker from desktopPID, zero when none has
func handoffTakenBy(text string, desktopPID int, runID string, startedAt *float64, createTime func(int) *float64) int {
	env := hostJudgeEnv(createTime, nil)
	env.OurPID = desktopPID
	env.OurCT = func() *float64 { return createTime(desktopPID) }
	judgement := judgeMarkerText(text, env)
	m := judgement.Marker
	if m == nil || m.PID == desktopPID {
		return 0
	}
	if runID != "" {
		if judgement.OwnerState == identityMatch && m.Run != nil && *m.Run == runID {
			return m.PID
		}
		return 0
	}
	live := judgement.OwnerState == identityMatch || judgement.OwnerState == identityUnknown
	if live && (startedAt == nil || m.StartedAt == *startedAt) {
		return m.PID
	}
	return 0
}

// Polls until the hand-off script takes the marker or the timeout passes, answering its pid or zero
func waitForHandoffClaim(home string, desktopPID int, runID string, startedAt *float64, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	probe := cachedCreateTimeProbe(processCreated)
	for {
		if raw, err := os.ReadFile(markerPath(home)); err == nil && len(raw) > 0 {
			if pid := handoffTakenBy(pystr.DecodeUTF8Replace(raw), desktopPID, runID, startedAt, probe); pid != 0 {
				return pid
			}
		}
		if !time.Now().Before(deadline) {
			return 0
		}
		time.Sleep(handoffClaimPoll)
	}
}
