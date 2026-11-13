package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Blocks the local backend start while an update owns the install, reporting the detached result once it clears
func (s *Service) WaitForUpdate() error {
	announced, longWaitAnnounced := false, false
	var parked ParkedRun
	ticker := newHoldTicker(func(state HeldState) { s.screen.show(state, primaryHoldOwner) }, func() { s.screen.clear(primaryHoldOwner) })
	overridden := false
	deps := s.updateGateDeps(markerGateCallbacks{
		onLiveMarker: func(run ParkedRun) {
			parked = run
			overridden = false
		},
		onHeld:     ticker.onHeld,
		onOverride: func(string) { overridden = true },
	})
	quit := s.backend.QuitContext()
	outcome, err := waitForUpdateClearance(deps, clearanceOptions{
		ctx:     quit,
		timeout: updateWaitTimeout,
		poll:    updateWaitPoll,
		onWaitTick: func(reason string, waited time.Duration) {
			if !announced {
				announced = true
				s.log(fmt.Sprintf("[updates] update in progress (%s); deferring backend start until it finishes", reason))
			}
			held, shown := ticker.tick(reason)
			if shown {
				return
			}
			if reason == gateReasonMarker && held != nil {
				s.backend.AdvanceBootProgress("backend.update-wait", heldWaitMessage(*held), 12)
				return
			}
			if reason == gateReasonMarker && waited >= updateWaitTimeout {
				if !longWaitAnnounced {
					longWaitAnnounced = true
					s.log("[updates] update still running past 20 minutes; its owner is alive, so the boot keeps waiting")
				}
				s.backend.AdvanceBootProgress("backend.update-wait", "An update is still running — Icebath will start automatically when it finishes. Its progress is in logs/update.log.", 12)
				return
			}
			s.backend.AdvanceBootProgress("backend.update-wait", "An update is finishing — Icebath will start automatically when it completes…", 12)
		},
	})
	if err != nil {
		return err
	}
	s.screen.clear(primaryHoldOwner)
	s.reportHandoffResult(parked, s.releaseHolderRetry())
	if outcome == clearanceCancelled {
		if err := s.backend.AssertCanStart(); err != nil {
			return err
		}
	}
	switch {
	case outcome == clearanceClear:
		return nil
	case outcome == clearanceTimeout:
		s.log("[updates] update still in progress after wait timeout; starting backend anyway")
	case overridden:
		s.log("[updates] proceeding with backend start over a held update (user chose Start anyway); no relaunch")
	case s.relaunchIntoSwappedBundle():
		s.backend.AdvanceBootProgress("backend.update-restart", "Restarting Icebath to load the updated app…", 14)
		time.Sleep(bundleSwapRelaunchFailsafe)
		s.log(fmt.Sprintf("[updates] relaunch did not land within %dms; continuing with the current build", bundleSwapRelaunchFailsafe.Milliseconds()))
	default:
		s.log("[updates] update finished; proceeding with backend start")
	}
	return nil
}

// The dialog's close and retry, nil off Windows where no host reports holders
func (s *Service) releaseHolderRetry() func([]ReleaseHolder) {
	if !s.isWindows() {
		return nil
	}
	return func(holders []ReleaseHolder) {
		closable := closableReleaseHolders(holders)
		names := holderNames(closable)
		updateRoot := s.resolveUpdateRoot()
		s.consent.mu.Lock()
		s.consent.pending = closable
		s.consent.mu.Unlock()
		forget := func(reason string) {
			s.consent.mu.Lock()
			s.consent.pending = nil
			s.consent.mu.Unlock()
			if err := os.Remove(releaseHolderConsentPath(updateRoot)); err != nil && !os.IsNotExist(err) {
				s.log(fmt.Sprintf("[updates] could not remove the consent to close %s: %s", names, nodeErrorMessage(err)))
			}
			s.log(fmt.Sprintf("[updates] retry after consenting to close %s %s", names, reason))
		}
		go func() {
			result, err := s.ApplyUpdates()
			if err != nil {
				forget("failed: " + err.Error())
				return
			}
			if !result.OK || !result.HandedOff {
				reason := "no hand-off"
				if result.Error != "" {
					reason = result.Error
				} else if result.Message != "" {
					reason = result.Message
				}
				forget("did not hand off: " + reason)
			}
		}()
	}
}

// Consent given for one apply, bound to its hand-off run when the script spawns
type releaseConsent struct {
	mu      sync.Mutex
	pending []ReleaseHolder
}

// Writes the pending consent bound to runID at most once, logging a write it cannot make
func (c *releaseConsent) bind(updateRoot string, runID *string, log func(string), now func() time.Time) {
	c.mu.Lock()
	holders := c.pending
	c.pending = nil
	c.mu.Unlock()
	if len(holders) == 0 || runID == nil || *runID == "" {
		return
	}
	type consentHolder struct {
		PID       int     `json:"pid"`
		StartTime float64 `json:"start_time"`
		Exe       string  `json:"exe"`
	}
	record := struct {
		WrittenAt float64         `json:"written_at"`
		RunID     string          `json:"run_id"`
		Holders   []consentHolder `json:"holders"`
	}{WrittenAt: float64(now().UnixMilli()) / 1000, RunID: *runID}
	for _, holder := range holders {
		record.Holders = append(record.Holders, consentHolder{PID: holder.PID, StartTime: holder.StartTime, Exe: holder.Exe})
	}
	body, _ := json.Marshal(record)
	if err := os.WriteFile(releaseHolderConsentPath(updateRoot), body, 0o666); err != nil {
		log("[updates] could not record the consent to close a Desktop folder holder: " + nodeErrorMessage(err))
	}
}
