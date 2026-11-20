package tools

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestProgressThrottleIsPerCallAndAlwaysPassesFirstAndFinal(t *testing.T) {
	clock := 0
	old := toolProgressNow
	toolProgressNow = func() time.Duration { return 100*time.Second + time.Duration(clock)*time.Second/8 }
	t.Cleanup(func() { toolProgressNow = old })
	var events []float64
	ctx := BoundToolProgress(context.Background(), func(_, _, _ string, _ map[string]any, extra map[string]any) {
		events = append(events, extra["progress"].(float64))
	}, "t", "call-1")
	report := CurrentToolProgress(ctx)
	total := 100.0
	for step := 1; step <= 10; step++ {
		report(float64(step)*10, &total, "")
		clock++
	}
	if fmt.Sprint(events) != "[10 50 90 100]" {
		t.Fatalf("events %v", events)
	}
	var other []float64
	otherCtx := BoundToolProgress(context.Background(), func(_, _, _ string, _ map[string]any, extra map[string]any) {
		other = append(other, extra["progress"].(float64))
	}, "t", "call-2")
	CurrentToolProgress(otherCtx)(5, &total, "")
	if fmt.Sprint(other) != "[5]" {
		t.Fatalf("second call throttled by the first %v", other)
	}
}

type progressEvent struct {
	event, name, preview string
	extra                map[string]any
}

func TestToolProgressNotificationsReachToolProgressCallback(t *testing.T) {
	e, host, _ := registryExecutor(t, handlerDef("mcp__prog__slow", func(ctx context.Context, _ *Call) string {
		report := CurrentToolProgress(ctx)
		if report == nil {
			return "no reporter"
		}
		total := 100.0
		for i := 1; i <= 20; i++ {
			report(float64(i*5), &total, fmt.Sprintf("stage %d", i))
		}
		return "done"
	}))
	var mu sync.Mutex
	var events []progressEvent
	host.progressCb = func(event, name, preview string, _ map[string]any, extra map[string]any) {
		if event != ToolProgressEvent {
			return
		}
		mu.Lock()
		events = append(events, progressEvent{event, name, preview, extra})
		mu.Unlock()
	}
	var messages []*state.Message
	if err := e.ExecuteToolCallsSequential(context.Background(), toolCallsFor("mcp__prog__slow", "call-7"), &messages, "task", true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messageText(messages[0]) != "done" {
		t.Fatalf("messages %v", messages)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 || len(events) >= 20 {
		t.Fatalf("throttle passed %d of 20 updates", len(events))
	}
	for _, ev := range events {
		if ev.name != "mcp__prog__slow" || ev.extra["tool_call_id"] != "call-7" {
			t.Fatalf("event %+v", ev)
		}
	}
	last := events[len(events)-1]
	if last.extra["percent"] != 100 || last.preview != "stage 20" {
		t.Fatalf("final update %+v", last)
	}
}

func TestToolProgressPayloadMatchesPythonCoercions(t *testing.T) {
	cases := []struct {
		name, message string
		kwargs        map[string]any
		want          string
	}{
		{"mcp", "stage 1", map[string]any{"tool_call_id": "c1", "progress": 5.0, "total": 100.0, "percent": 5}, "map[message:stage 1 name:mcp percent:5 progress:5 tool_id:c1 total:100]"},
		{"", "", map[string]any{}, "map[message:<nil> name: percent:<nil> progress:0 tool_id: total:<nil>]"},
		{"x", "", map[string]any{"tool_call_id": 7, "progress": true}, "map[message:<nil> name:x percent:<nil> progress:1 tool_id:7 total:<nil>]"},
		{"x", "", map[string]any{"tool_call_id": "", "progress": nil}, "map[message:<nil> name:x percent:<nil> progress:0 tool_id: total:<nil>]"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(ToolProgressPayload(c.name, c.message, c.kwargs)); got != c.want {
			t.Fatalf("payload %s want %s", got, c.want)
		}
	}
}
