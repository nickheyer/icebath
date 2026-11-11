package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestATurnQueuedWithBrowserControlRecoversAndRunsWithoutIt(t *testing.T) {
	ctx, owner, api := newAPITurnOwner(t)
	admitted, err := AdmitAPITurn(ctx, api, &APITurn{SessionID: "legacy-browser-control", RequestID: "queued-turn",
		UserMessage: "hello", ConversationHistory: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	stored := pyjson.Clone(admitted.Row.GetOr("payload", nil)).(*pyjson.Object)
	stored.GetOr("api_turn_v1", nil).(*pyjson.Object).Set("browser_control",
		pyjson.ObjectOf("principal", "principal:default:abc", "transport_family", "local-api"))
	encoded, err := pyjson.Dumps(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE session_admissions SET payload_json=? WHERE admission_id=?", encoded,
			admitted.Row.GetOr("admission_id", nil))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owner.mu.Lock()
	clear(owner.Sessions)
	owner.mu.Unlock()
	var scheduled []string
	saved := scheduleSession
	scheduleSession = func(_ context.Context, _ *SessionAuthority, ref SessionRef) error {
		scheduled = append(scheduled, ref.SessionID)
		return nil
	}
	t.Cleanup(func() { scheduleSession = saved })
	if err := RecoverAPITurns(ctx, api); err != nil {
		t.Fatal(err)
	}
	if len(scheduled) != 1 || scheduled[0] != admitted.Ref.SessionID {
		t.Fatalf("scheduled = %v", scheduled)
	}
	pending, err := owner.DB.ListSessionAdmissions(ctx, admitted.Ref.SessionID, true)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	legacy := pending[0]
	if _, carried := legacy.GetOr("payload", nil).(*pyjson.Object).GetOr("api_turn_v1", nil).(*pyjson.Object).Get("browser_control"); !carried {
		t.Fatal("stored payload lost browser_control")
	}
	var texts []string
	var executions []*pyjson.Object
	owner.Runner.(*apiOwnerRunner).handle = func(ctx context.Context, event *MessageEvent) (any, error) {
		texts = append(texts, event.Text)
		executions = append(executions, APIExecution(ctx).Copy())
		executionResultOf(ctx).Set("result", pyjson.ObjectOf("final_response", "ok"))
		return "ok", nil
	}
	reply, err := executeAdmission(ctx, owner, admitted.Ref, legacy)
	if err != nil || reply != "ok" {
		t.Fatalf("reply = %v, %v", reply, err)
	}
	if len(texts) != 1 || texts[0] != "hello" {
		t.Fatalf("texts = %v", texts)
	}
	if _, leaked := executions[0].Get("browser_control"); leaked {
		t.Fatal("execution carries browser_control")
	}
}
