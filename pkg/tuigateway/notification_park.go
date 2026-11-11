package tuigateway

import (
	"log/slog"
	"reflect"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	parkRetentionSeconds = 600.0
	parkMaxParked        = 16
	parkMaxParkedPerKey  = 4
)

type parkedEvent struct {
	deadline float64
	evt      map[string]any
}

var (
	parkMu     sync.Mutex
	parkedList []parkedEvent
)

func parkLogger() *slog.Logger { return slog.Default().With("logger", "tui_gateway.notification_park") }

// Whether an event may wait in the park, startup replayed delegations never
func parkable(evt map[string]any) bool {
	return !(evt["type"] == "async_delegation" && pyrepr.Truthy(evt["restored"]))
}

// Drops expired entries under the park lock and returns them for release outside it
func parkExpireLocked(now float64) []map[string]any {
	var expired []map[string]any
	var kept []parkedEvent
	for _, p := range parkedList {
		if p.deadline <= now {
			expired = append(expired, p.evt)
		} else {
			kept = append(kept, p)
		}
	}
	if len(expired) > 0 {
		parkedList = kept
	}
	return expired
}

// Whether an event is a progress notice that yields to results
func parkIsProgress(evt map[string]any) bool {
	evtType := ""
	if pyrepr.Truthy(evt["type"]) {
		evtType = pyrepr.Str(evt["type"])
	}
	return evtType == "heartbeat" || strings.HasPrefix(evtType, "watch_")
}

// Lets go of events the park held, a delegation logged as a lost result with its offer cleared
func parkRelease(events []map[string]any) {
	for _, evt := range events {
		isDelegation := evt["type"] == "async_delegation"
		if isDelegation {
			tools.ReturnCompletionOffer(evt)
		}
		UnholdParked([]map[string]any{evt})
		evtType := "completion"
		if t, ok := evt["type"]; ok {
			evtType = pyrepr.Str(t)
		}
		msg := "Dropping parked " + evtType + " notification past its retention window (key=" + pyrepr.Repr(parkKey(evt)) + ")"
		if isDelegation {
			parkLogger().Warn(msg)
		} else {
			parkLogger().Debug(msg)
		}
	}
}

// Owner session key of an event
func parkKey(evt map[string]any) string {
	if pyrepr.Truthy(evt["session_key"]) {
		return pyrepr.Str(evt["session_key"])
	}
	return ""
}

// Room for an event under the park lock, a result displacing the oldest progress notice of the same owner first
func parkAdmitLocked(evt map[string]any, key string) (bool, []map[string]any) {
	held := 0
	for _, p := range parkedList {
		if parkKey(p.evt) == key {
			held++
		}
	}
	keyFull := key != "" && held >= parkMaxParkedPerKey
	if !keyFull && len(parkedList) < parkMaxParked {
		return true, nil
	}
	if parkIsProgress(evt) {
		return false, nil
	}
	var progress, sameOwner []int
	for i, p := range parkedList {
		if parkIsProgress(p.evt) {
			progress = append(progress, i)
			if parkKey(p.evt) == key {
				sameOwner = append(sameOwner, i)
			}
		}
	}
	victims := sameOwner
	if len(victims) == 0 && !keyFull {
		victims = progress
	}
	if len(victims) == 0 {
		return false, nil
	}
	victim := parkedList[victims[0]].evt
	parkedList = append(parkedList[:victims[0]:victims[0]], parkedList[victims[0]+1:]...)
	return true, []map[string]any{victim}
}

// Retains an addressed event whose owner session is not live, false when the park cannot take it
func ParkNotification(evt map[string]any) bool {
	if !parkable(evt) {
		return false
	}
	now := pythread.Monotonic()
	parkMu.Lock()
	expired := parkExpireLocked(now)
	admitted, displaced := parkAdmitLocked(evt, parkKey(evt))
	if admitted {
		tools.MarkCopyHeld(tools.CompletionCopyID(evt))
		parkedList = append(parkedList, parkedEvent{deadline: now + parkRetentionSeconds, evt: evt})
	}
	parkMu.Unlock()
	parkRelease(expired)
	for _, held := range displaced {
		evtType := "completion"
		if t, ok := evt["type"]; ok {
			evtType = pyrepr.Str(t)
		}
		parkLogger().Debug("Dropping parked " + pyrepr.Str(held["type"]) + " notification to make room for a " + evtType + " (key=" + pyrepr.Repr(parkKey(held)) + ")")
	}
	return admitted
}

// Identity of an event dict
func parkIdentity(evt map[string]any) uintptr { return uintptr(reflect.ValueOf(evt).UnsafePointer()) }

// Parked events the owner predicate accepts in arrival order, each handed out to exactly one poller
func ClaimParked(owns func(map[string]any) bool) []map[string]any {
	parkMu.Lock()
	if len(parkedList) == 0 {
		parkMu.Unlock()
		return nil
	}
	expired := parkExpireLocked(pythread.Monotonic())
	snapshot := make([]map[string]any, len(parkedList))
	for i, p := range parkedList {
		snapshot[i] = p.evt
	}
	parkMu.Unlock()
	parkRelease(expired)
	mine := map[uintptr]bool{}
	for _, evt := range snapshot {
		if owns(evt) {
			mine[parkIdentity(evt)] = true
		}
	}
	if len(mine) == 0 {
		return nil
	}
	parkMu.Lock()
	defer parkMu.Unlock()
	var claimed []map[string]any
	var kept []parkedEvent
	for _, p := range parkedList {
		if mine[parkIdentity(p.evt)] {
			claimed = append(claimed, p.evt)
		} else {
			kept = append(kept, p)
		}
	}
	parkedList = kept
	return claimed
}

// Ends the held copy mark the park took for each event
func UnholdParked(events []map[string]any) {
	for _, evt := range events {
		tools.UnmarkCopyHeld(tools.CompletionCopyID(evt))
	}
}

// Events currently parked in arrival order
func ParkedNotifications() []map[string]any {
	parkMu.Lock()
	defer parkMu.Unlock()
	out := make([]map[string]any, len(parkedList))
	for i, p := range parkedList {
		out[i] = p.evt
	}
	return out
}

// Empties the park and ends every held copy mark
func ClearParked() {
	parkMu.Lock()
	dropped := make([]map[string]any, len(parkedList))
	for i, p := range parkedList {
		dropped[i] = p.evt
	}
	parkedList = nil
	parkMu.Unlock()
	UnholdParked(dropped)
}
