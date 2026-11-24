package tuihost

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

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

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
			WriteJSON(context.Background(), jsonutil.ObjectOf("seq", seq, "text", marker))
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
		obj, err := jsonutil.DecodeObject(frame)
		if err != nil {
			t.Fatal(err)
		}
		if seq, has := obj.Get("seq"); has && obj.ValueOr("text", nil) == marker {
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
	if WriteJSON(context.Background(), jsonutil.ObjectOf("ok", true)) {
		t.Fatal("a write to a broken pipe reported success")
	}
}

func TestWriteJsonUnserializablePayloadBecomesErrorFrame(t *testing.T) {
	var out, logs bytes.Buffer
	swapRealStdout(t, &out)
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(saved) })
	created := make(chan int)
	if !WriteJSON(context.Background(), jsonutil.ObjectOf("jsonrpc", "2.0", "id", "profiles", "result", jsonutil.ObjectOf("created", created))) {
		t.Fatal("the error frame was not written")
	}
	frame, err := jsonutil.DecodeObject(out.String())
	if err != nil {
		t.Fatalf("stdout %q: %v", out.String(), err)
	}
	errObj, _ := frame.ValueOr("error", nil).(*jsonutil.Object)
	if frame.ValueOr("id", nil) != "profiles" || errObj == nil || !jsonIsInt(errObj.ValueOr("code", nil), -32603) {
		t.Fatalf("frame = %q", out.String())
	}
	if message, _ := errObj.ValueOr("message", nil).(string); !strings.HasPrefix(message, "response serialization error") || !strings.Contains(message, "chan int") {
		t.Fatalf("message = %q", message)
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("logs = %q", logs.String())
	}
}

var hostedRoomTask = map[string]any{"room_id": "room-a", "task_id": "task-1", "thread_id": "thread-1", "turn_id": "turn-1",
	"execution_generation": 2, "member_id": "reviewer"}
