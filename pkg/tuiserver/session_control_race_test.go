package tuiserver

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Context key marking the goroutine that plays the agent thread of a tool completion
type toolThreadKey struct{}

func TestAToolCompletionRacingAUserChangeNeverPublishesTheOlderSnapshot(t *testing.T) {
	sid, key := controlSession(t)
	saveGoal(key, nil)
	entry := tuigateway.SessionRecord(sid)
	publishSessionControlSnapshot(context.Background(), sid, entry, false, false)
	recorder := &frameRecorder{}
	ctx := tuigateway.BindTransport(context.Background(), recorder)
	read, release := &pythread.Event{}, &pythread.Event{}
	t.Cleanup(release.Set)
	realSnapshot := snapshotControlFn
	snapshotControlFn = func(ctx context.Context, sessionKey string) (map[string]any, error) {
		control, err := realSnapshot(ctx, sessionKey)
		if ctx.Value(toolThreadKey{}) != nil {
			read.Set()
			release.Wait(5)
		}
		return control, err
	}
	t.Cleanup(func() { snapshotControlFn = realSnapshot })
	var tool, user sync.WaitGroup
	tool.Add(1)
	go func() {
		defer tool.Done()
		onToolComplete(context.WithValue(ctx, toolThreadKey{}, true), sid, "call", "terminal", map[string]any{"command": "true"}, `{"exit_code": 0}`)
	}()
	if !read.Wait(5) {
		t.Fatal("the tool completion never read the snapshot")
	}
	user.Add(1)
	userDone := make(chan struct{})
	go func() {
		defer user.Done()
		defer close(userDone)
		controlCall(ctx, "session.control", map[string]any{"session_id": sid, "action": "subgoal.add", "args": map[string]any{"text": "newer criterion"}})
	}()
	select {
	case <-userDone:
	case <-time.After(500 * time.Millisecond):
	}
	release.Set()
	tool.Wait()
	user.Wait()
	var updates []any
	for _, update := range controlUpdates(recorder) {
		payload, _ := update[1].(map[string]any)
		control, _ := payload["control"].(map[string]any)
		goal, _ := control["goal"].(map[string]any)
		updates = append(updates, goal["subgoals"])
	}
	if len(updates) == 0 || !reflect.DeepEqual(updates[len(updates)-1], []any{"newer criterion"}) {
		t.Fatalf("published subgoals %v, the last must be the user's", updates)
	}
}
