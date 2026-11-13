package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	heldReprobe         = 5 * time.Second
	helperErrorAttempts = 3
	holdScreenGrace     = 5 * time.Second
	primaryHoldOwner    = "primary"
)

// HeldState is why a dead marker still keeps the gate closed as the script helper put it
type HeldState struct {
	Verdict   string
	OwnerPID  *int
	LivePID   *int
	HoldID    string
	Since     int64
	CheckedAt int64
	Blocking  bool
}

func stillRunning(kind string) bool {
	return kind == "held" || kind == "busy" || kind == "live" || kind == "error"
}

// Stable id of one marker body, the file identity mixed in so equal bodies of different updates differ
func markerHoldID(raw []byte, file *fileIdentity) string {
	hash := sha256.New()
	hash.Write(raw)
	if file != nil {
		fmt.Fprintf(hash, "\x00%s:%s", jsNumberText(file.ino), jsNumberText(file.mtimeMS))
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

var absentHoldID = markerHoldID([]byte("\x00no update marker\x00"), nil)

// Inode and modification time of a file as Node stat reports them
type fileIdentity struct {
	ino     float64
	mtimeMS float64
}

func statMarkerFile(home string) *fileIdentity {
	info, err := os.Stat(markerPath(home))
	if err != nil {
		return nil
	}
	mod := info.ModTime()
	return &fileIdentity{ino: fileIno(markerPath(home), info), mtimeMS: float64(mod.Unix())*1e3 + float64(mod.Nanosecond())/1e6}
}

// Process wide hold decisions every gate wait of this desktop shares
type holdRegistry struct {
	mu          sync.Mutex
	firstHeldAt map[string]int64
	order       []string
	startAnyway map[string]bool
	unanswered  map[string]bool
	generation  int
}

func newHoldRegistry() *holdRegistry {
	return &holdRegistry{firstHeldAt: map[string]int64{}, startAnyway: map[string]bool{}, unanswered: map[string]bool{}}
}

// First sighting of a held body, the table reset once it holds sixteen
func (r *holdRegistry) heldSince(key string, at int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.firstHeldAt[key]; !ok {
		if len(r.firstHeldAt) >= 16 {
			r.firstHeldAt = map[string]int64{}
		}
		r.firstHeldAt[key] = at
	}
	return r.firstHeldAt[key]
}

func (r *holdRegistry) wasHeld(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.firstHeldAt[key]
	return ok
}

// Records the confirmed Start anyway over this exact held body
func (r *holdRegistry) allowStartOverHold(holdID string) {
	r.mu.Lock()
	r.startAnyway[holdID] = true
	r.mu.Unlock()
}

func (r *holdRegistry) startAnywayFor(holdID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startAnyway[holdID]
}

// Asks the helper again on every probe's next poll
func (r *holdRegistry) requestHoldRecheck() {
	r.mu.Lock()
	r.generation++
	r.mu.Unlock()
}

func (r *holdRegistry) currentGeneration() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

func (r *holdRegistry) markUnanswerable(holdID string) {
	r.mu.Lock()
	r.unanswered[holdID] = true
	r.mu.Unlock()
}

func (r *holdRegistry) isUnanswerable(holdID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unanswered[holdID]
}

// UpdateHoldBoard holds one blocked hold per wait, the primary's shown first
type UpdateHoldBoard struct {
	owners []string
	holds  map[string]HeldState
}

func (b *UpdateHoldBoard) set(owner string, state HeldState) {
	if b.holds == nil {
		b.holds = map[string]HeldState{}
	}
	if _, ok := b.holds[owner]; !ok {
		b.owners = append(b.owners, owner)
	}
	b.holds[owner] = state
}

func (b *UpdateHoldBoard) clear(owner string) {
	if _, ok := b.holds[owner]; !ok {
		return
	}
	delete(b.holds, owner)
	for i, o := range b.owners {
		if o == owner {
			b.owners = append(b.owners[:i], b.owners[i+1:]...)
			break
		}
	}
}

func (b *UpdateHoldBoard) shown() *HeldState {
	if state, ok := b.holds[primaryHoldOwner]; ok {
		return &state
	}
	if len(b.owners) == 0 {
		return nil
	}
	state := b.holds[b.owners[0]]
	return &state
}

// One wait's hold state machine showing a blocking hold once it outlasts the grace
type holdTicker struct {
	show         func(HeldState)
	clear        func()
	now          func() time.Time
	held         *HeldState
	blockedSince *time.Time
}

func newHoldTicker(show func(HeldState), clear func()) *holdTicker {
	return &holdTicker{show: show, clear: clear, now: time.Now}
}

func (h *holdTicker) onHeld(state HeldState) {
	h.held = &state
}

func (h *holdTicker) tick(reason string) (*HeldState, bool) {
	seen := h.held
	h.held = nil
	if reason == gateReasonMarker && seen != nil && seen.Blocking {
		if h.blockedSince == nil {
			at := h.now()
			h.blockedSince = &at
		}
	} else {
		h.blockedSince = nil
	}
	shown := seen != nil && h.blockedSince != nil && h.now().Sub(*h.blockedSince) >= holdScreenGrace
	if shown {
		h.show(*seen)
	} else {
		h.clear()
	}
	return seen, shown
}

type askedEntry struct {
	verdict        MarkerHelperVerdict
	at             int64
	generation     int
	overrideLogged bool
	errors         int
	answered       bool
}

func verdictDue(previous *askedEntry, now func() int64, reprobe time.Duration, generation int) bool {
	return previous == nil || (stillRunning(previous.verdict.Kind) && (now()-previous.at >= reprobe.Milliseconds() || previous.generation != generation))
}

func (r *holdRegistry) refreshVerdict(previous *askedEntry, holdID string, reclaim func() MarkerHelperVerdict, now func() int64, log func(string), subject string) *askedEntry {
	generation := r.currentGeneration()
	verdict := reclaim()
	previousKind := ""
	if previous != nil {
		previousKind = previous.verdict.Kind
	}
	if verdict.Kind == "unsupported" && (stillRunning(previousKind) || r.wasHeld(holdID)) {
		verdict = MarkerHelperVerdict{Kind: "error"}
	}
	if previous == nil || stillRunning(previous.verdict.Kind) != stillRunning(verdict.Kind) {
		pid := ""
		if verdict.Kind == "live" || verdict.Kind == "taken" {
			pid = fmt.Sprintf(" %d", verdict.PID)
		}
		log(fmt.Sprintf("[updates] %s: script helper says %s%s", subject, verdict.Kind, pid))
	}
	entry := askedEntry{}
	if previous != nil {
		entry = *previous
	}
	errorsSeen := 0
	if verdict.Kind == "error" {
		errorsSeen = entry.errors + 1
	}
	entry.verdict, entry.at, entry.generation, entry.errors = verdict, now(), generation, errorsSeen
	entry.answered = entry.answered || verdict.Kind != "error"
	return &entry
}

func (r *holdRegistry) helperUnanswerable(entry *askedEntry, holdID string, log func(string)) bool {
	if entry.verdict.Kind != "error" || entry.answered || entry.errors < helperErrorAttempts {
		return false
	}
	r.markUnanswerable(holdID)
	log(fmt.Sprintf("[updates] dead update marker (hold %s): the script helper failed %d times and never answered; starting as for any dead marker. The marker is left in place.", holdID, entry.errors))
	return true
}

func logOverrideOnce(entry *askedEntry, state HeldState, log func(string)) {
	if entry.overrideLogged {
		return
	}
	entry.overrideLogged = true
	log(fmt.Sprintf("[updates] update marker still %s (hold %s); not blocking start-up because the user chose Start anyway. The marker is left in place.", state.Verdict, state.HoldID))
}

// ParkedRun is the run identity a live marker carried when the gate saw it
type ParkedRun struct {
	StartedAt *float64
	RunID     *string
}

type helperSubject struct {
	holdID   string
	absent   bool
	label    string
	ownerPID *int
	run      ParkedRun
}

func helperSubjectOf(inspection MarkerInspection, home string, checkoutLockMayBeHeld func() bool) *helperSubject {
	if inspection.State == inspectionAbsent {
		if checkoutLockMayBeHeld != nil && !checkoutLockMayBeHeld() {
			return nil
		}
		return &helperSubject{holdID: absentHoldID, absent: true, label: "no update marker"}
	}
	if inspection.State != inspectionDead {
		return nil
	}
	subject := &helperSubject{holdID: markerHoldID(inspection.Raw, statMarkerFile(home)), label: "dead update marker"}
	if m := inspection.Marker; m != nil {
		pid, started := m.PID, m.StartedAt
		subject.ownerPID, subject.run = &pid, ParkedRun{StartedAt: &started, RunID: m.Run}
	}
	return subject
}

// Without a marker only a positive held closes the gate
func keepsGateShut(verdict MarkerHelperVerdict, absent bool) bool {
	return stillRunning(verdict.Kind) && (!absent || verdict.Kind == "held")
}

// Whether a failed receipt finished after a v1 marker started, ending that update
func failedAfterV1Start(home string, m *UpdateMarker) bool {
	if m == nil || m.CT != nil || m.Delegate != nil {
		return false
	}
	text, err := os.ReadFile(filepath.Join(home, "logs", "update_receipts", "latest.json"))
	if err != nil {
		return false
	}
	var receipt map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(string(text), "\ufeff")), &receipt) != nil {
		return false
	}
	finished, _ := receipt["finished_at"].(string)
	finishedS := jsDateParse(finished) / 1000
	return receipt["outcome"] == "failed" && finishedS >= m.StartedAt+1
}

// Epoch milliseconds of an ISO 8601 date time as Date.parse reads it, NaN when it does not parse
func jsDateParse(text string) float64 {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04Z07:00", "2006-01-02"} {
		if t, err := time.Parse(layout, text); err == nil {
			return float64(t.UnixNano()) / 1e6
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return float64(t.UnixNano()) / 1e6
		}
	}
	return math.NaN()
}

type liveMarkerProbeOptions struct {
	home                  string
	reclaim               func() MarkerHelperVerdict
	checkoutLockMayBeHeld func() bool
	createTime            func(pid int) *float64
	onLiveMarker          func(ParkedRun)
	onHeld                func(HeldState)
	onOverride            func(holdID string)
	log                   func(string)
	now                   func() int64
	reprobe               time.Duration
}

// The live marker signal of one gate wait, asking the script helper about dead bodies
func (r *holdRegistry) liveMarkerProbe(o liveMarkerProbeOptions) func() bool {
	if o.now == nil {
		o.now = func() int64 { return time.Now().UnixMilli() }
	}
	if o.reprobe == 0 {
		o.reprobe = heldReprobe
	}
	if o.log == nil {
		o.log = func(string) {}
	}
	asked := map[string]*askedEntry{}
	finishedLogged := false
	return func() bool {
		inspection := inspectUpdateMarker(o.home, o.createTime, func() float64 { return float64(o.now()) })
		if inspection.State == inspectionLive {
			if failedAfterV1Start(o.home, inspection.Marker) {
				if !finishedLogged {
					finishedLogged = true
					o.log("[updates] latest update receipt records a failure after this v1 marker started; not parking the boot on it")
				}
				return false
			}
			if o.onLiveMarker != nil {
				run := ParkedRun{}
				if m := inspection.Marker; m != nil {
					started := m.StartedAt
					run = ParkedRun{StartedAt: &started, RunID: m.Run}
				}
				o.onLiveMarker(run)
			}
			return true
		}
		if o.reclaim == nil {
			return false
		}
		subject := helperSubjectOf(inspection, o.home, o.checkoutLockMayBeHeld)
		if subject == nil {
			return false
		}
		holdID := subject.holdID
		if r.isUnanswerable(holdID) {
			return false
		}
		entry := asked[holdID]
		if verdictDue(entry, o.now, o.reprobe, r.currentGeneration()) {
			entry = r.refreshVerdict(entry, holdID, o.reclaim, o.now, o.log, subject.label)
			asked[holdID] = entry
		}
		verdict := entry.verdict
		if !keepsGateShut(verdict, subject.absent) || r.helperUnanswerable(entry, holdID, o.log) {
			return false
		}
		state := HeldState{Verdict: verdict.Kind, OwnerPID: subject.ownerPID, HoldID: holdID, Since: r.heldSince(holdID, o.now()), CheckedAt: entry.at,
			Blocking: verdict.Kind != "live" && (verdict.Kind != "error" || entry.answered)}
		if verdict.Kind == "live" || verdict.Kind == "taken" {
			pid := verdict.PID
			state.LivePID = &pid
		}
		if state.Blocking && r.startAnywayFor(holdID) {
			logOverrideOnce(entry, state, o.log)
			if o.onOverride != nil {
				o.onOverride(holdID)
			}
			return false
		}
		if o.onHeld != nil {
			o.onHeld(state)
		}
		if o.onLiveMarker != nil {
			o.onLiveMarker(subject.run)
		}
		return true
	}
}

// Boot progress text while a dead marker's checkout is still held
func heldWaitMessage(state HeldState) string {
	if state.Verdict == "busy" || state.Verdict == "error" {
		return "Icebath could not verify update ownership yet — startup is paused while it retries. Details are in logs/update.log."
	}
	if state.LivePID != nil {
		return fmt.Sprintf("An update is still finishing (process %d) — Icebath will start automatically when it completes…", *state.LivePID)
	}
	who := "a process"
	if state.OwnerPID != nil && *state.OwnerPID != 0 {
		who = fmt.Sprintf("the update (process %d) exited, but a process", *state.OwnerPID)
	}
	return fmt.Sprintf("An update is still finishing: %s it started still holds the Icebath install. Icebath will start when it lets go.", who)
}

func exitedOwner(state HeldState) string {
	if state.OwnerPID != nil && *state.OwnerPID != 0 {
		return fmt.Sprintf(" (update pid %d, exited)", *state.OwnerPID)
	}
	return ""
}

// Log line of a confirmed Start anyway
func startAnywayLogLine(state HeldState) string {
	since := time.UnixMilli(state.Since).UTC().Format("2006-01-02T15:04:05.000Z")
	return fmt.Sprintf("[updates] USER OVERRIDE: Start anyway over an update marker the helper reports %s%s, hold %s, blocking since %s; starting the local backend without waiting. The marker is left in place.",
		state.Verdict, exitedOwner(state), state.HoldID, since)
}
