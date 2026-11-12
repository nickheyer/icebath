package tuiserver

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestCompletionDisplayKeepsPayloadSeparateAcrossSurfaces(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rows := []struct {
		status    string
		truncated bool
		label     string
	}{{"completed", false, "Completed"}, {"failed", false, "Failed"}, {"cancelled", false, "Cancelled"}, {"completed", true, "Incomplete"},
		{"stalled", false, "Stalled"}, {"unknown", false, "Unknown"}, {"rejected", false, "Failed"}, {"other_terminal_state", false, "Incomplete"}}
	saved := notifDispatchEvent
	t.Cleanup(func() { notifDispatchEvent = saved })
	for _, row := range rows {
		event := map[string]any{"type": "async_delegation", "session_key": "display-session", "delegation_id": "deleg-test",
			"goals":   []any{"Do not display this sibling", "Review [bold]changes[/bold]"},
			"results": []any{map[string]any{"task_index": 1, "status": row.status, "truncated": row.truncated, "summary": "Full result evidence"}}}
		original := pycopy.DeepCopy(event)
		payload := tools.FormatProcessNotification(event)
		expected := fmt.Sprintf("Subagent Task %s: Review [bold]changes[/bold]", row.label)
		recorder := &frameRecorder{}
		ctx := tuigateway.BindTransport(context.Background(), recorder)
		var dispatched []string
		notifDispatchEvent = func(_ context.Context, _ string, _ *tuigateway.Session, _ map[string]any, text string) {
			dispatched = append(dispatched, text)
		}
		session := tuigateway.NewSession(map[string]any{"session_key": event["session_key"]})
		notifHandleEvent(ctx, "ui-session", session, event, map[string]bool{}, tools.NewProcessRegistry(), tools.FormatProcessNotification, nil, nil, false)
		frames := recordedFrames(recorder)
		if len(frames) == 0 {
			t.Fatalf("%s: nothing emitted", row.status)
		}
		emitted := frames[0]["params"].(map[string]any)["payload"].(map[string]any)
		if emitted["text"] != expected {
			t.Fatalf("%s: emitted %v", row.status, emitted)
		}
		if len(dispatched) != 1 || dispatched[0] != payload {
			t.Fatalf("%s: dispatched %q", row.status, dispatched)
		}
		if got := asyncDelegationDisplayMetadata(event)["display_text"]; got != expected {
			t.Fatalf("%s: display metadata text %v", row.status, got)
		}
		if !reflect.DeepEqual(event, original) {
			t.Fatalf("%s: event mutated to %v", row.status, event)
		}
	}
}
