package updater

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UpdateHoldWire is the hold the blocked boot screen shows
type UpdateHoldWire struct {
	HoldID    string `json:"holdId"`
	Verdict   string `json:"verdict"`
	OwnerPID  *int   `json:"ownerPid"`
	Since     int64  `json:"since"`
	CheckedAt int64  `json:"checkedAt"`
	LogPath   string `json:"logPath"`
}

// Callbacks one gate wait hands its marker probe
type markerGateCallbacks struct {
	onLiveMarker func(ParkedRun)
	onHeld       func(HeldState)
	onOverride   func(holdID string)
}

// Blocked boot screen state over every wait blocked past the grace, the primary's shown first
type holdScreen struct {
	home     string
	log      func(string)
	progress func(update map[string]any)

	mu       sync.Mutex
	current  *HeldState
	bootHold *UpdateHoldWire
	board    UpdateHoldBoard
}

func (h *holdScreen) wire(state HeldState) UpdateHoldWire {
	verdict := state.Verdict
	if verdict == "live" {
		verdict = "held"
	}
	return UpdateHoldWire{HoldID: state.HoldID, Verdict: verdict, OwnerPID: state.OwnerPID, Since: state.Since, CheckedAt: state.CheckedAt,
		LogPath: filepath.Join(h.home, "logs", "update.log")}
}

// Publishes state on the boot screen, as a boot phase only for the primary wait, under h.mu
func (h *holdScreen) renderLocked(state HeldState, bootPhase bool) map[string]any {
	previous := h.current
	sameHold := previous != nil && previous.HoldID == state.HoldID && previous.Verdict == state.Verdict
	if sameHold && previous.CheckedAt == state.CheckedAt && h.bootHold != nil {
		return nil
	}
	if previous == nil || previous.HoldID != state.HoldID {
		owner := ""
		if state.OwnerPID != nil && *state.OwnerPID != 0 {
			owner = fmt.Sprintf(" (update pid %d, exited)", *state.OwnerPID)
		}
		h.log(fmt.Sprintf("[updates] boot blocked: the update marker is %s%s, hold %s; the backend stays stopped until the hold ends, the user quits, or the user confirms Start anyway",
			state.Verdict, owner, state.HoldID))
	}
	current := state
	h.current = &current
	wire := h.wire(state)
	h.bootHold = &wire
	if !bootPhase {
		return map[string]any{"updateHold": wire}
	}
	update := map[string]any{"phase": "backend.update-held", "progress": 12, "running": true, "error": nil, "updateHold": wire}
	if !sameHold {
		update["message"] = heldWaitMessage(state)
	}
	return update
}

func (h *holdScreen) push(update map[string]any) {
	if update != nil {
		h.progress(update)
	}
}

// Clears one wait's hold, showing the next blocked wait's hold when one remains
func (h *holdScreen) clear(owner string) {
	h.mu.Lock()
	h.board.clear(owner)
	if shown := h.board.shown(); shown != nil {
		update := h.renderLocked(*shown, false)
		h.mu.Unlock()
		h.push(update)
		return
	}
	if h.current == nil && h.bootHold == nil {
		h.mu.Unlock()
		return
	}
	h.current, h.bootHold = nil, nil
	h.mu.Unlock()
	h.push(map[string]any{"updateHold": nil})
}

// Records one wait's hold and shows the board's chosen hold
func (h *holdScreen) show(state HeldState, owner string) {
	h.mu.Lock()
	h.board.set(owner, state)
	update := h.renderLocked(*h.board.shown(), owner == primaryHoldOwner)
	h.mu.Unlock()
	h.push(update)
}

// The hold the boot screen shows now, nil when none
func (h *holdScreen) shownHold() *HeldState {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil {
		return nil
	}
	current := *h.current
	return &current
}

// The hold the boot progress carries, nil when none
func (h *holdScreen) carried() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bootHold == nil {
		return nil
	}
	return *h.bootHold
}

// Marker probe shared by the boot and pool gates, one creation time probe per wait
func (s *Service) markerGateProbe(cb markerGateCallbacks) func() bool {
	createTime := cachedCreateTimeProbe(processCreated)
	return s.holds.liveMarkerProbe(liveMarkerProbeOptions{
		home:         s.opts.IcebathHome,
		createTime:   createTime,
		onLiveMarker: cb.onLiveMarker,
		onHeld:       cb.onHeld,
		onOverride:   cb.onOverride,
		log:          s.log,
		checkoutLockMayBeHeld: func() bool {
			return checkoutLockMayBeHeld(s.resolveUpdateRoot(), s.isWindows())
		},
		reclaim: func() MarkerHelperVerdict {
			return runMarkerHelper("reclaim", s.resolveUpdateRoot(), s.opts.IcebathHome, 0, "", s.isWindows())
		},
	})
}

// Gate signals of the marker, the in process apply and the accepted hand-off
func (s *Service) updateGateDeps(cb markerGateCallbacks) updateGateDeps {
	return updateGateDeps{
		hasLiveMarker:    s.markerGateProbe(cb),
		isUpdateInFlight: s.updateInFlight.Load,
		isHandoffActive:  s.quittingForHandoff.Load,
	}
}

// Parks a pool backend start while an update holds the install, its hold published on the blocked screen
func (s *Service) WaitForPoolUpdate(profile string, cancelled func() bool) error {
	announced := false
	holdLogged := ""
	owner := "pool:" + profile
	ticker := newHoldTicker(func(state HeldState) { s.screen.show(state, owner) }, func() { s.screen.clear(owner) })
	defer s.screen.clear(owner)
	deps := s.updateGateDeps(markerGateCallbacks{onHeld: func(state HeldState) {
		ticker.onHeld(state)
		if state.Blocking && holdLogged != state.HoldID {
			holdLogged = state.HoldID
			s.log(fmt.Sprintf("[updates] pool backend start for profile \"%s\" blocked: update marker %s, hold %s; waiting for the hold to end or a Start anyway on the blocked screen",
				profile, state.Verdict, state.HoldID))
		}
	}})
	_, err := waitForUpdateClearance(deps, clearanceOptions{
		ctx:         s.backend.QuitContext(),
		isCancelled: cancelled,
		timeout:     updateWaitTimeout,
		poll:        updateWaitPoll,
		onWaitTick: func(reason string, _ time.Duration) {
			if !announced {
				announced = true
				s.log(fmt.Sprintf("[updates] update in progress (%s); deferring pool backend start for profile \"%s\"", reason, profile))
			}
			ticker.tick(reason)
		},
	})
	return err
}

// Answers the blocked screen's Check again for the hold the primary window shows
func (s *Service) UpdateHoldRecheck(windowID string) map[string]bool {
	hold := s.screen.shownHold()
	if !s.isPrimaryBootSender(windowID) || hold == nil {
		return map[string]bool{"ok": false}
	}
	s.log(fmt.Sprintf("[updates] boot blocked (hold %s): user asked to check again", hold.HoldID))
	s.holds.requestHoldRecheck()
	return map[string]bool{"ok": true}
}

// Quits the desktop from the blocked screen of the primary window
func (s *Service) UpdateHoldQuit(windowID string) map[string]bool {
	if !s.isPrimaryBootSender(windowID) {
		return map[string]bool{"ok": false}
	}
	hold := s.screen.shownHold()
	suffix := ""
	if hold != nil {
		suffix = fmt.Sprintf(" (hold %s)", hold.HoldID)
	}
	s.log("[updates] user quit Icebath from the update-hold screen" + suffix)
	s.host.Quit()
	return map[string]bool{"ok": true}
}

// StartAnywayRequest is the confirmed override the blocked screen sends
type StartAnywayRequest struct {
	HoldID    any `json:"holdId"`
	Confirmed any `json:"confirmed"`
}

// Lets the boot through the confirmed hold the primary window shows, logged and flushed first
func (s *Service) UpdateHoldStartAnyway(windowID string, request *StartAnywayRequest) map[string]bool {
	hold := s.screen.shownHold()
	var holdID, confirmed any
	if request != nil {
		holdID, confirmed = request.HoldID, request.Confirmed
	}
	if !s.isPrimaryBootSender(windowID) || hold == nil || confirmed != true || holdID != any(hold.HoldID) {
		which := "no hold"
		if hold != nil {
			which = "hold " + hold.HoldID
		}
		requested := "none"
		if text, ok := holdID.(string); ok {
			requested = jsSlice(text, 32)
		}
		s.log(fmt.Sprintf("[updates] Start anyway refused: %s is not the confirmed hold (%s)", which, requested))
		return map[string]bool{"ok": false}
	}
	s.log(startAnywayLogLine(*hold))
	s.opts.DesktopLog.FlushSync()
	s.holds.allowStartOverHold(hold.HoldID)
	return map[string]bool{"ok": true}
}

// First n UTF-16 code units of text as String.prototype.slice keeps them
func jsSlice(text string, n int) string {
	units := 0
	var b strings.Builder
	for _, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > n {
			if units < n && width == 2 {
				b.WriteRune(0xfffd)
			}
			break
		}
		units += width
		b.WriteRune(r)
	}
	return b.String()
}
