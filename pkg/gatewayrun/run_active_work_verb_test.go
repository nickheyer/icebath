package gatewayrun

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Loop whose threadsafe scheduling runs the callback at once between the two hooks
type inlineLoop struct{ before, after func() }

func (l inlineLoop) CallSoonThreadsafe(fn func()) error {
	if l.before != nil {
		l.before()
	}
	fn()
	if l.after != nil {
		l.after()
	}
	return nil
}

func (l inlineLoop) CallLater(time.Duration, func()) (gateway.LoopTimer, error) { return nil, nil }

func (l inlineLoop) IsClosed() bool { return false }

func (l inlineLoop) IsRunning() bool { return true }

type fakeActiveWorkRunner struct {
	units     []map[string]any
	count     int
	authority func(profile string) unserveStopCounter
}

func (f *fakeActiveWorkRunner) describeActiveWork(context.Context) []map[string]any { return f.units }
func (f *fakeActiveWorkRunner) activeWorkCount() int                                { return f.count }
func (f *fakeActiveWorkRunner) servedProfileAuthority(profile string) unserveStopCounter {
	if f.authority == nil {
		return nil
	}
	return f.authority(profile)
}

type sideAgentAuthority struct{}

func (sideAgentAuthority) UnserveStopCounts() (int, int, int) { return 0, 0, 1 }

func TestActiveWorkVerbScopedToAProfileNamesOnlyItsTurns(t *testing.T) {
	units := []map[string]any{{"kind": "chat", "session": "agent:coder:telegram:dm:7", "pid": 1},
		{"kind": "chat", "session": "agent:main:telegram:dm:8", "pid": 1}}
	runner := &fakeActiveWorkRunner{units: units, count: 2}
	handler := activeWorkVerb(context.Background(), runner, inlineLoop{})

	all, err := handler(map[string]any{})
	if err != nil || all["active_agents"] != 2 {
		t.Fatalf("unscoped answer %v %v", all, err)
	}
	scoped, err := handler(map[string]any{"profile": "coder"})
	if err != nil {
		t.Fatal(err)
	}
	var sessions []any
	for _, u := range scoped["active_work"].([]map[string]any) {
		sessions = append(sessions, u["session"])
	}
	if scoped["active_agents"] != 1 || !reflect.DeepEqual(sessions, []any{"agent:coder:telegram:dm:7"}) {
		t.Fatalf("scoped answer %v", scoped)
	}
}

func TestActiveWorkVerbCountsAProfilesSideAgentAndReadsItOnTheLoop(t *testing.T) {
	var onLoop, reads []bool
	homes := map[string]string{"coder": "/h/coder"}
	runner := &fakeActiveWorkRunner{count: 1, authority: func(profile string) unserveStopCounter {
		if _, ok := homes[profile]; !ok {
			return nil
		}
		reads = append(reads, onLoop[len(onLoop)-1])
		return sideAgentAuthority{}
	}}
	loop := inlineLoop{before: func() { onLoop = append(onLoop, true) }, after: func() { onLoop = append(onLoop, false) }}
	answer, err := activeWorkVerb(context.Background(), runner, loop)(map[string]any{"profile": "coder"})
	if err != nil {
		t.Fatal(err)
	}
	work := answer["active_work"].([]map[string]any)
	if answer["active_agents"] != 1 || work[0]["kind"] != "side_agent" {
		t.Fatalf("answer %v", answer)
	}
	if !reflect.DeepEqual(reads, []bool{true}) {
		t.Fatalf("the authority must be read inside the loop callback, reads %v", reads)
	}
}
