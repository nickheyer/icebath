package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	PrepareStallAttempts       = 3
	PrepareRetryTaskPrefix     = "prepare:"
	prepareStallEventPrefix    = "prepare-stalled:"
	prepareStallMaxErrorChars  = 300
	prepareStallEllipsisSuffix = "…"
)

func prepareStallLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.hosted_room_prepare_stall")
}

// Contention or an ownership move, never counted toward the stall
func prepareStallTransient(err error) bool {
	if class, ok := state.SQLiteClassOf(err); ok && class == state.SQLiteOperationalError {
		return true
	}
	return errAs[*AuthorityConflictError](err) || errAs[*EventCursorConflictError](err) || errAs[*RoomNotFoundError](err) ||
		errAs[*LeaseHeldError](err) || errAs[*StaleLeaseError](err) || errAs[*RoomUnavailableError](err)
}

type prepareStreak struct {
	failures int
	err      *string
	stalled  bool
}

// Bounded one line text of a preparation failure, its type name when the message is blank
func prepareStallDescribe(err error) string {
	text := strings.TrimSpace(err.Error())
	if text == "" {
		text = pyrepr.TypeName(err)
	}
	runes := []rune(text)
	if len(runes) <= prepareStallMaxErrorChars {
		return text
	}
	return string(runes[:prepareStallMaxErrorChars-1]) + prepareStallEllipsisSuffix
}

// Per room consecutive preparation failures shared by driver room workers and RPC callers
type PrepareStallTracker struct {
	mu    sync.Mutex
	rooms map[string]*prepareStreak
}

func NewPrepareStallTracker() *PrepareStallTracker {
	return &PrepareStallTracker{rooms: map[string]*prepareStreak{}}
}

// Whether the room's preparation stalled
func (t *PrepareStallTracker) Stalled(roomID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	streak := t.rooms[roomID]
	return streak != nil && streak.stalled
}

// Last recorded failure text of the room, nil when none
func (t *PrepareStallTracker) LastError(roomID string) *string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if streak := t.rooms[roomID]; streak != nil {
		return streak.err
	}
	return nil
}

func (t *PrepareStallTracker) Clear(roomID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.rooms, roomID)
}

// Records one failure, true exactly once when this failure stalls the room
func (t *PrepareStallTracker) Failed(roomID string, err error) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	streak := t.rooms[roomID]
	if streak == nil {
		streak = &prepareStreak{}
		t.rooms[roomID] = streak
	}
	text := prepareStallDescribe(err)
	streak.err = &text
	if prepareStallTransient(err) || streak.stalled {
		return false
	}
	streak.failures++
	streak.stalled = streak.failures >= PrepareStallAttempts
	return streak.stalled
}

// Marks a failed explicit Retry stalled at once, without a second room event
func (t *PrepareStallTracker) Stall(roomID string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := prepareStallDescribe(err)
	t.rooms[roomID] = &prepareStreak{failures: PrepareStallAttempts, err: &text, stalled: true}
}

// Retry row of a stalled room, its member id naming who was not woken
func PrepareRetryAction(roomID string, room map[string]any) map[string]any {
	names := []string{}
	members, _ := room["members"].([]any)
	for _, raw := range members {
		member, _ := hostedMapping(raw)
		name := ""
		if v := member["handle"]; pyrepr.Truthy(v) {
			name = pyrepr.Str(v)
		} else if v := member["member_id"]; pyrepr.Truthy(v) {
			name = pyrepr.Str(v)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	memberID := strings.Join(names, ", ")
	if memberID == "" {
		memberID = roomID
	}
	return map[string]any{"kind": "retry", "member_id": memberID, "task_id": PrepareRetryTaskPrefix + roomID, "execution_generation": 1}
}

// Appends the room's one stall notice, a stall already last in the log not repeated
func ReportPrepareStall(ctx context.Context, dbPath, roomID string, cause error) {
	if err := reportPrepareStall(ctx, dbPath, roomID, cause); err != nil {
		prepareStallLog().Warn(fmt.Sprintf("hosted room %s: recording the stalled preparation failed", roomID), "exc_info", err)
	}
}

func reportPrepareStall(ctx context.Context, dbPath, roomID string, cause error) error {
	room, err := RoomState(ctx, dbPath, roomID, false)
	if err != nil {
		return err
	}
	latestSeq, err := pyint.Int64(room["latest_seq"])
	if err != nil {
		return err
	}
	page, err := ReadEvents(ctx, dbPath, roomID, max(0, latestSeq-1), 1, false)
	if err != nil {
		return err
	}
	if tail, _ := page["events"].([]any); len(tail) > 0 {
		last, _ := hostedMapping(tail[len(tail)-1])
		if strings.HasPrefix(pyrepr.Str(last["event_id"]), prepareStallEventPrefix) {
			return nil
		}
	}
	gatewayID := pyrepr.Str(room["authority_gateway_id"])
	epoch, err := pyint.Int64(room["authority_epoch"])
	if err != nil {
		return err
	}
	_, err = AppendEvent(ctx, dbPath, AppendEventInput{
		RoomID: roomID, EventID: fmt.Sprintf("%s%d", prepareStallEventPrefix, latestSeq), Kind: "member.unavailable",
		Actor:              map[string]any{"kind": "gateway", "id": gatewayID},
		Payload:            map[string]any{"error": "Members could not be woken: " + prepareStallDescribe(cause), "reason_code": "prepare_failed"},
		AuthorityGatewayID: gatewayID, AuthorityEpoch: epoch, ControlReserve: true,
	})
	return err
}
