package gatewayrun

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/kanban"
)

// Completed task with a dm subscription in the delivery mode
func makeCompletedWakeTask(t *testing.T, mode string) string {
	t.Helper()
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "wake ordering task", "worker", kstr("agent:main:telegram:dm:chat-1"))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-1", ChatType: kstr("dm"), DeliveryMode: kstr(mode)})
	kanbanComplete(t, conn, tid, "done")
	return tid
}

// Failure counts of the runner in key order
func failCountValues(r *GatewayRunner) []int {
	var out []int
	for _, v := range r.kanbanSubFailCounts {
		out = append(out, v)
	}
	return out
}

func TestWakeOnlySuccessAdvancesCursorSingleWake(t *testing.T) {
	kanbanTestDB(t, "wake-ok.db")
	tid := makeCompletedWakeTask(t, "wake")
	adapter := newKanbanRecordingAdapter()
	r := makeKanbanRunner(t, adapter)
	runOneNotifierTick(t, r)
	if len(adapter.sent) != 0 {
		t.Fatal("wake-only mode must not send a text ping")
	}
	if len(adapter.handled) != 1 {
		t.Fatal("wake injection is the sole delivery")
	}
	if len(unseenTerminalEventsFor(t, tid, "chat-1")) != 0 {
		t.Fatal("cursor must advance after a successful wake-only delivery")
	}
	if len(r.kanbanSubFailCounts) != 0 {
		t.Fatalf("fail counts %v", r.kanbanSubFailCounts)
	}
}

func TestWakeOnlyFailureRewindsAndRedelivers(t *testing.T) {
	kanbanTestDB(t, "wake-fail.db")
	tid := makeCompletedWakeTask(t, "wake")
	adapter := newKanbanRecordingAdapter()
	adapter.failWake = true
	r := makeKanbanRunner(t, adapter)
	runOneNotifierTick(t, r)
	if len(adapter.handled) != 1 {
		t.Fatal("one wake attempt on the first tick")
	}
	if len(unseenTerminalEventsFor(t, tid, "chat-1")) != 1 {
		t.Fatal("a failed wake-only delivery must REWIND the claim so the event is redelivered on a later tick")
	}
	if v := failCountValues(r); len(v) != 1 || v[0] != 1 {
		t.Fatalf("failure counter must bump on a failed wake-only delivery: %v", v)
	}
	if len(kanbanSubsOf(t, tid)) != 1 {
		t.Fatal("one transient failure must not drop the sub")
	}
	r2 := makeKanbanRunner(t, adapter)
	r2.kanbanSubFailCounts = r.kanbanSubFailCounts
	runOneNotifierTick(t, r2)
	if len(adapter.handled) != 2 {
		t.Fatal("event must be redelivered next tick")
	}
	if v := failCountValues(r2); len(v) != 1 || v[0] != 2 {
		t.Fatalf("counts %v", v)
	}
}

func TestNotifyWakeFailureRetriesWithoutRepeatingPing(t *testing.T) {
	kanbanTestDB(t, "notify-wake.db")
	tid := makeCompletedWakeTask(t, "notify+wake")
	adapter := newKanbanRecordingAdapter()
	adapter.failWake = true
	r := makeKanbanRunner(t, adapter)
	runOneNotifierTick(t, r)
	if len(adapter.sent) != 1 || len(adapter.handled) != 1 || len(unseenTerminalEventsFor(t, tid, "chat-1")) != 1 {
		t.Fatalf("sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
	if v := failCountValues(r); len(v) != 1 || v[0] != 1 {
		t.Fatalf("counts %v", v)
	}
	r.running.Store(true)
	runOneNotifierTick(t, r)
	if len(adapter.sent) != 1 || len(adapter.handled) != 2 {
		t.Fatalf("sent %d handled %d", len(adapter.sent), len(adapter.handled))
	}
	if v := failCountValues(r); len(v) != 1 || v[0] != 2 {
		t.Fatalf("counts %v", v)
	}
}

func TestWakeOnlyFailureCapDropsSubscription(t *testing.T) {
	kanbanTestDB(t, "wake-cap.db")
	tid := makeCompletedWakeTask(t, "wake")
	adapter := newKanbanRecordingAdapter()
	adapter.failWake = true
	r := makeKanbanRunner(t, adapter)
	r.kanbanSubFailCounts = map[kanbanSubKey]int{{tid, "telegram", "chat-1", ""}: 11}
	runOneNotifierTick(t, r)
	if len(adapter.handled) != 1 {
		t.Fatalf("handled %d", len(adapter.handled))
	}
	if len(kanbanSubsOf(t, tid)) != 0 {
		t.Fatal("subscription must drop after MAX_SEND_FAILURES consecutive wake-only delivery failures, like text sends do")
	}
	if len(r.kanbanSubFailCounts) != 0 {
		t.Fatal("counter entry must clear when the subscription is dropped")
	}
}
