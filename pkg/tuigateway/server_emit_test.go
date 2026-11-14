package tuigateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestEmitWithPayload(t *testing.T) {
	buf := captureStdout(t)
	Emit(context.Background(), "test.event", "s1", map[string]any{"key": "val"})
	msg, err := pyjson.LoadsDict(buf.String())
	if err != nil {
		t.Fatalf("stdout %q: %v", buf.String(), err)
	}
	params, _ := msg.GetOr("params", nil).(*pyjson.Object)
	if msg.GetOr("method", nil) != "event" || params == nil || params.GetOr("type", nil) != "test.event" || params.GetOr("session_id", nil) != "s1" {
		t.Fatalf("frame = %q", buf.String())
	}
	if payload, _ := params.GetOr("payload", nil).(*pyjson.Object); payload == nil || payload.GetOr("key", nil) != "val" {
		t.Fatalf("frame = %q", buf.String())
	}
}

// Stdout stand in that counts writes in flight and keeps every line written
type recordingStdout struct {
	mu                sync.Mutex
	active, maxActive int
	frames            []string
}

func (r *recordingStdout) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.active++
	r.maxActive = max(r.maxActive, r.active)
	r.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	r.mu.Lock()
	r.frames = append(r.frames, string(p))
	r.active--
	r.mu.Unlock()
	return len(p), nil
}

func swapRealStdout(t *testing.T, w io.Writer) {
	t.Helper()
	saved := realStdout
	realStdout = w
	t.Cleanup(func() { realStdout = saved })
}

func TestWriteJsonSerializesConcurrentWrites(t *testing.T) {
	marker := strings.Repeat("x", 24)
	out := &recordingStdout{}
	swapRealStdout(t, out)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for seq := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			WriteJSON(context.Background(), pyjson.ObjectOf("seq", seq, "text", marker))
		}()
	}
	close(start)
	wg.Wait()
	if out.maxActive != 1 {
		t.Fatalf("writes in flight = %d", out.maxActive)
	}
	seqs := []int{}
	for _, frame := range out.frames {
		if !strings.HasSuffix(frame, "\n") {
			t.Fatalf("frame %q has no line end", frame)
		}
		obj, err := pyjson.LoadsDict(frame)
		if err != nil {
			t.Fatal(err)
		}
		if seq, has := obj.Get("seq"); has && obj.GetOr("text", nil) == marker {
			seqs = append(seqs, int(seq.(int64)))
		}
	}
	slices.Sort(seqs)
	if !slices.Equal(seqs, []int{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("seqs = %v", seqs)
	}
}

type brokenStdout struct{}

func (brokenStdout) Write([]byte) (int, error) { return 0, syscall.EPIPE }

func TestWriteJsonReturnsFalseOnBrokenPipe(t *testing.T) {
	swapRealStdout(t, brokenStdout{})
	if WriteJSON(context.Background(), pyjson.ObjectOf("ok", true)) {
		t.Fatal("a write to a broken pipe reported success")
	}
}

func TestWriteJsonUnserializablePayloadBecomesErrorFrame(t *testing.T) {
	var out, logs bytes.Buffer
	swapRealStdout(t, &out)
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(saved) })
	created := pydatetime.DateTime{Year: 2026, Month: 8, Day: 22}
	if !WriteJSON(context.Background(), pyjson.ObjectOf("jsonrpc", "2.0", "id", "profiles", "result", pyjson.ObjectOf("created", created))) {
		t.Fatal("the error frame was not written")
	}
	frame, err := pyjson.LoadsDict(out.String())
	if err != nil {
		t.Fatalf("stdout %q: %v", out.String(), err)
	}
	errObj, _ := frame.GetOr("error", nil).(*pyjson.Object)
	if frame.GetOr("id", nil) != "profiles" || errObj == nil || !jsonIsInt(errObj.GetOr("code", nil), -32603) {
		t.Fatalf("frame = %q", out.String())
	}
	if message, _ := errObj.GetOr("message", nil).(string); !strings.HasPrefix(message, "response serialization error") || !strings.Contains(message, "datetime") {
		t.Fatalf("message = %q", message)
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("logs = %q", logs.String())
	}
}

var hostedRoomTask = map[string]any{"room_id": "room-a", "task_id": "task-1", "thread_id": "thread-1", "turn_id": "turn-1",
	"execution_generation": 2, "member_id": "reviewer"}

// Records every room activity hook the emit path queues, with stdout discarded and every session answerable
func roomObserver(t *testing.T) *[]map[string]any {
	t.Helper()
	swapRealStdout(t, io.Discard)
	seen := &[]map[string]any{}
	savedHook := enqueueRoomStreamHook
	enqueueRoomStreamHook = func(_ context.Context, hookName string, payload map[string]any) bool {
		if hookName == RoomMemberActivityHook {
			*seen = append(*seen, payload)
		}
		return true
	}
	wireSinks(t)
	BindSinks(func(frame *pyjson.Object) { WriteJSON(context.Background(), frame) },
		func(event, sid string, payload map[string]any) { Emit(context.Background(), event, sid, payload) },
		func(string) bool { return true }, nil)
	t.Cleanup(func() { enqueueRoomStreamHook = savedHook })
	return seen
}

func roomSession(t *testing.T, sid string, hosted bool) {
	t.Helper()
	entry := map[string]any{"session_key": sid, "transport": nil, "agent": nil, "created_at": float64(time.Now().Unix())}
	if hosted {
		task := map[string]any{}
		for k, v := range hostedRoomTask {
			task[k] = v
		}
		entry["_hosted_room_task"] = task
	}
	SetSessionRecord(sid, NewSession(entry))
	t.Cleanup(func() { SetSessionRecord(sid, nil) })
}

func TestRoomMemberSessionEventsReachPluginsWithRoomCoordinates(t *testing.T) {
	observer := roomObserver(t)
	roomSession(t, "room-sid", true)
	ctx := context.Background()
	Emit(ctx, "tool.start", "room-sid", map[string]any{"tool_id": "call-1", "name": "terminal", "args": map[string]any{"command": "ls"}})
	settle, err := SendAsync("approval", "room-sid", pyjson.ObjectOf("request_id", "req-1", "command", "rm -rf build"), func(*pyjson.Object) {})
	if err != nil {
		t.Fatal(err)
	}
	settle("test_done")
	Emit(ctx, "session.info", "room-sid", map[string]any{"title": "chrome, not member activity"})
	Emit(ctx, "tool.complete", "room-sid", map[string]any{"tool_id": "call-1", "name": "terminal", "result": "ok"})
	kinds := []string{}
	for _, event := range *observer {
		kinds = append(kinds, event["kind"].(string))
	}
	if !slices.Equal(kinds, []string{"tool.started", "request.opened", "tool.completed"}) {
		t.Fatalf("kinds = %v", kinds)
	}
	started := (*observer)[0]
	for k, want := range hostedRoomTask {
		if started[k] != want {
			t.Fatalf("coordinate %s = %v want %v", k, started[k], want)
		}
	}
	payload := started["payload"].(map[string]any)
	if args, _ := payload["args"].(map[string]any); payload["tool_id"] != "call-1" || args["command"] != "ls" {
		t.Fatalf("payload = %v", payload)
	}
	if (*observer)[1]["payload"].(map[string]any)["request_id"] != "req-1" {
		t.Fatalf("request payload = %v", (*observer)[1]["payload"])
	}
	seqs := []int{}
	for _, event := range *observer {
		if seq, ok := event["seq"].(int); ok {
			seqs = append(seqs, seq)
		} else if event["seq"] != nil {
			t.Fatalf("seq = %T %v", event["seq"], event["seq"])
		}
	}
	if len(seqs) != 2 || !slices.IsSorted(seqs) {
		t.Fatalf("seqs = %v", seqs)
	}
}

func TestOrdinarySessionEventsNeverFireTheRoomHook(t *testing.T) {
	observer := roomObserver(t)
	roomSession(t, "plain-sid", false)
	ctx := context.Background()
	Emit(ctx, "tool.start", "plain-sid", map[string]any{"tool_id": "call-1", "name": "terminal"})
	Emit(ctx, "approval.request", "plain-sid", map[string]any{"request_id": "req-1", "command": "ls"})
	Emit(ctx, "message.delta", "plain-sid", map[string]any{"text": "hi"})
	if len(*observer) != 0 {
		t.Fatalf("observer = %v", *observer)
	}
}
