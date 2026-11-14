package gateway

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// A frame as the owner reads it back off the worker's pipe
func wireFrame(t *testing.T, kind string, body *pyjson.Object) *pyjson.Object {
	t.Helper()
	frame := pyjson.ObjectOf("type", kind)
	for _, k := range body.Keys {
		frame.Set(k, body.Vals[k])
	}
	data, err := EncodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ReadFrame(bufio.NewReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWorkerToolFramesPublishTheInProcessPayload(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "owned", "owner", db, epoch)
	owner := "human"
	live := NewLiveSession(&SessionSource{UserID: &owner}, "route")
	a.Sessions["s"] = live
	if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "r",
		Payload: pyjson.ObjectOf("text", "go")}); err != nil {
		t.Fatal(err)
	}
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "s"})
	if err != nil || row == nil {
		t.Fatalf("claim %v %v", row, err)
	}
	if err := a.StampClaim(ctx, testRef, live, row); err != nil {
		t.Fatal(err)
	}
	var frames []*pyjson.Object
	live.EventStream.Observers[&SessionEventObserver{Observe: func(frame *pyjson.Object) { frames = append(frames, frame) }}] = true
	var observed [][]any
	record := func(kind string) APIObserverCallback {
		return func(args []any, _ map[string]any) error {
			observed = append(observed, append([]any{kind}, args...))
			return nil
		}
	}
	admission, _ := row.GetOr("admission_id", "").(string)
	a.apiObservers = map[string][]*APIObserver{admission: {{Callbacks: map[string]APIObserverCallback{
		"tool_start_callback": record("start"), "tool_complete_callback": record("complete")}}}}
	args := map[string]any{"command": "echo OUT; exit 3", "timeout": int64(10)}
	result := `{"output": "OUT", "exit_code": 3, "error": null}`
	startBody, err := ToolFrame(ctx, "call-1", "terminal", args)
	if err != nil {
		t.Fatal(err)
	}
	doneBody, err := ToolFrame(ctx, "call-1", "terminal", args, result)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := row.GetOr("generation", int64(0)).(int64)
	for _, frame := range []*pyjson.Object{wireFrame(t, "tool.start", startBody), wireFrame(t, "tool.complete", doneBody)} {
		if ok, err := publishWorkerToolEvent(ctx, a, "s", generation, frame); !ok || err != nil {
			t.Fatalf("publish %v %v %v", frame, ok, err)
		}
	}
	argsObj, _ := pyjson.AsObject(args)
	isError := true
	want := []*pyjson.Object{toolLifecyclePayload("call-1", "terminal", argsObj), toolCompletePayload(ctx, "call-1", "terminal", argsObj, result, &isError, false)}
	var kinds []string
	for i, f := range frames {
		params := f.GetOr("params", nil).(*pyjson.Object)
		kinds = append(kinds, params.GetOr("type", "").(string))
		if !pyjson.Equal(params.GetOr("payload", nil), want[i]) {
			t.Fatalf("payload %d %v want %v", i, params.GetOr("payload", nil), want[i])
		}
	}
	if len(kinds) != 2 || kinds[0] != "tool.start" || kinds[1] != "tool.complete" {
		t.Fatalf("published %v", kinds)
	}
	complete := frames[1].GetOr("params", nil).(*pyjson.Object).GetOr("payload", nil).(*pyjson.Object)
	if complete.GetOr("tool_id", nil) != "call-1" || complete.GetOr("context", nil) == nil || complete.GetOr("is_error", nil) != true ||
		complete.GetOr("result", nil) != result || !pyjson.Equal(complete.GetOr("args", nil), argsObj) {
		t.Fatalf("complete %v", complete)
	}
	if len(observed) != 2 || observed[0][0] != "start" || observed[1][0] != "complete" || !pyjson.Equal(observed[0][1:], []any{"call-1", "terminal", argsObj}) ||
		!pyjson.Equal(observed[1][1:], []any{"call-1", "terminal", argsObj, result}) {
		t.Fatalf("observed %v", observed)
	}
	if ok, _ := publishWorkerToolEvent(ctx, a, "s", generation, pyjson.ObjectOf("type", "tool.start", "tool_call_id", "x", "name", "terminal")); ok {
		t.Fatal("id and name only frame published")
	}
}
