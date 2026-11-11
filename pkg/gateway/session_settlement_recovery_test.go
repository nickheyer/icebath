package gateway

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Completion payload publishCompletion sends for an outcome, row fields and captured result
func publishedCompletion(t *testing.T, outcome string, response any, row, captured *pyjson.Object) *pyjson.Object {
	t.Helper()
	live := NewLiveSession(nil, "route")
	var frames []*pyjson.Object
	live.EventStream.Observers[&SessionEventObserver{Observe: func(frame *pyjson.Object) { frames = append(frames, frame) }}] = true
	live.EventStream.Lock.Lock()
	err := publishCompletion(context.Background(), &SessionAuthority{}, live, SessionRef{SessionID: "s"}, row,
		pyjson.ObjectOf("outcome", outcome), response, captured)
	live.EventStream.Lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var payloads []*pyjson.Object
	for _, frame := range frames {
		params, _ := frame.Get("params")
		if kind, _ := params.(*pyjson.Object).Get("type"); kind == "message.complete" {
			payload, _ := params.(*pyjson.Object).Get("payload")
			payloads = append(payloads, payload.(*pyjson.Object))
		}
	}
	if len(payloads) != 1 {
		t.Fatalf("completions %d", len(payloads))
	}
	return payloads[0]
}

// Captured result of a turn interrupted with the recorded cause
func interruptCause(message string, extra ...any) *pyjson.Object {
	result := pyjson.ObjectOf("interrupted", true, "interrupt_message", message)
	for i := 0; i+1 < len(extra); i += 2 {
		result.Set(extra[i].(string), extra[i+1])
	}
	return pyjson.ObjectOf("result", result)
}

func personRow() *pyjson.Object {
	return pyjson.ObjectOf("admission_id", "adm-1", "request_id", "sub-1")
}

func TestOtherEndingsGetNoRestartNotice(t *testing.T) {
	for name, frame := range map[string]*pyjson.Object{
		"completed": publishedCompletion(t, "completed", "partial reply", personRow(), interruptCause("Gateway restarting")),
		"no cause":  publishedCompletion(t, "interrupted", "partial reply", personRow(), pyjson.ObjectOf("result", pyjson.ObjectOf("interrupted", true))),
		"user stop": publishedCompletion(t, "interrupted", "partial reply", personRow(), interruptCause("User requested stop")),
		"no result": publishedCompletion(t, "interrupted", "partial reply", personRow(), nil),
	} {
		if _, warned := frame.Get("warning"); warned {
			t.Fatalf("%s carries a restart notice: %v", name, frame)
		}
	}
}

func TestATurnWarningIsNotOverwritten(t *testing.T) {
	captured := interruptCause("Gateway restarting", "local_viewer", true, "warning", "own warning")
	if warning, _ := publishedCompletion(t, "interrupted", "partial reply", personRow(), captured).Get("warning"); warning != "own warning" {
		t.Fatalf("warning %v", warning)
	}
}

func TestAutomationTurnTheOwnerStopInterruptedIsNeverAskedToResend(t *testing.T) {
	rows := []func() *pyjson.Object{
		func() *pyjson.Object {
			return pyjson.ObjectOf("principal_id", "cron-owner", "payload", pyjson.ObjectOf("text", "job"))
		},
		func() *pyjson.Object {
			return pyjson.ObjectOf("principal_id", "automation:agent:main:telegram:dm:x", "payload", pyjson.ObjectOf("text", "done"))
		},
		func() *pyjson.Object {
			return pyjson.ObjectOf("principal_id", "person", "payload",
				pyjson.ObjectOf("text", "hook", "native_text_v1", pyjson.ObjectOf("webhook_delivery", pyjson.NewObject())))
		},
	}
	for _, cause := range []string{"Gateway restarting", "Gateway shutting down"} {
		for _, build := range rows {
			row := build()
			row.Set("admission_id", "adm-1")
			row.Set("request_id", "sub-1")
			frame := publishedCompletion(t, "interrupted", "", row, interruptCause(cause))
			warning, _ := frame.Get("warning")
			if warning != automationInterruptedNotice {
				t.Fatalf("%s %v: warning %v", cause, row, warning)
			}
			if strings.Contains(strings.ReplaceAll(warning.(string), "not run again", ""), "again") {
				t.Fatalf("resend advice in %q", warning)
			}
		}
	}
}

// One claimed admission on a real store, stamped on its live session
func claimedOnRealStore(t *testing.T, principal string, earlier func(ctx context.Context, db *state.SessionDB)) (context.Context, *state.SessionDB, *SessionAuthority, *LiveSession, *pyjson.Object) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if earlier != nil {
		earlier(ctx, db)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "owned", "owner", db, epoch)
	live := NewLiveSession(nil, "route")
	a.Sessions["s"] = live
	if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: principal, SessionID: "s", RequestID: "r",
		Payload: pyjson.ObjectOf("text", "please work")}); err != nil {
		t.Fatal(err)
	}
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "s"})
	if err != nil || row == nil {
		t.Fatalf("claim %v %v", row, err)
	}
	if err := a.stampClaim(ctx, testRef, live, row); err != nil {
		t.Fatal(err)
	}
	return ctx, db, a, live, row
}

// Appends one transcript row of the test session
func appendTurnRow(t *testing.T, ctx context.Context, db *state.SessionDB, fields map[string]any) {
	t.Helper()
	if _, err := db.AppendMessage(ctx, "s", fields, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

// Settles one claimed admission through commitAndPublish with the cause recorded on its result
func settleOnRealStore(t *testing.T, cause, principal string) (context.Context, *state.SessionDB, *SessionAuthority, *pyjson.Object, *[]*pyjson.Object, *turnSettlement) {
	t.Helper()
	ctx, db, a, live, row := claimedOnRealStore(t, principal, nil)
	appendTurnRow(t, ctx, db, map[string]any{"role": "user", "content": "please work"})
	appendTurnRow(t, ctx, db, map[string]any{"role": "assistant", "content": "partial reply"})
	frames := &[]*pyjson.Object{}
	live.EventStream.Observers[&SessionEventObserver{Observe: func(frame *pyjson.Object) { *frames = append(*frames, frame) }}] = true
	result := pyjson.ObjectOf("final_response", "partial reply", "messages", []any{}, "interrupted", true, "completed", false)
	if cause != "" {
		result.Set("interrupt_message", cause)
	}
	settlement := &turnSettlement{}
	if err := commitAndPublish(ctx, a, live, testRef, row, "partial reply", "interrupted", pyjson.ObjectOf("result", result), settlement); err != nil {
		t.Fatal(err)
	}
	return ctx, db, a, row, frames, settlement
}

// Contents of the failed turn boundaries of the test session's transcript
func failedTurnNotices(t *testing.T, ctx context.Context, db *state.SessionDB) []string {
	t.Helper()
	messages, err := db.GetMessages(ctx, "s", state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	notices := []string{}
	for _, message := range messages {
		if kind, _ := message.Get("display_kind"); kind == failedTurnDisplayKind {
			notices = append(notices, rowString(message, "content"))
		}
	}
	return notices
}

func TestOwnerStopReasonIsInTheTranscriptForAViewerThatReconnects(t *testing.T) {
	for cause, notice := range map[string]string{"Gateway restarting": restartInterruptNotice, "Gateway shutting down": shutdownInterruptNotice} {
		t.Run(cause, func(t *testing.T) {
			ctx, db, a, row, frames, settlement := settleOnRealStore(t, cause, "person")
			if rowString(settlement.Settled, "outcome") != "interrupted" {
				t.Fatalf("settled %v", settlement.Settled)
			}
			if got := failedTurnNotices(t, ctx, db); !slices.Equal(got, []string{notice}) {
				t.Fatalf("notices %v", got)
			}
			var completions []*pyjson.Object
			for _, frame := range *frames {
				params, _ := frame.Get("params")
				if kind, _ := params.(*pyjson.Object).Get("type"); kind == "message.complete" {
					payload, _ := params.(*pyjson.Object).Get("payload")
					completions = append(completions, payload.(*pyjson.Object))
				}
			}
			if len(completions) != 1 {
				t.Fatalf("completions %d", len(completions))
			}
			if warning, _ := completions[0].Get("warning"); warning != notice {
				t.Fatalf("warning %v", warning)
			}
			retried := pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "partial reply", "messages", []any{}, "interrupted", true,
				"interrupt_message", cause))
			if _, _, err := finishResult(ctx, db, a.Epoch, row, "partial reply", "interrupted", retried, nil, nil); err != nil {
				t.Fatal(err)
			}
			if got := failedTurnNotices(t, ctx, db); !slices.Equal(got, []string{notice}) {
				t.Fatalf("a settlement retry wrote the notice again: %v", got)
			}
		})
	}
}

func TestAUserStopWritesNoTranscriptNotice(t *testing.T) {
	for _, cause := range []string{"", "User requested stop"} {
		ctx, db, _, _, _, _ := settleOnRealStore(t, cause, "person")
		if got := failedTurnNotices(t, ctx, db); len(got) != 0 {
			t.Fatalf("cause %q wrote %v", cause, got)
		}
	}
}

func TestAutomationTurnTranscriptNoticeIsTheNeutralCopy(t *testing.T) {
	ctx, db, _, _, _, _ := settleOnRealStore(t, "Gateway restarting", "cron-owner")
	if got := failedTurnNotices(t, ctx, db); !slices.Equal(got, []string{automationInterruptedNotice}) {
		t.Fatalf("notices %v", got)
	}
}

func TestAnEarlierTurnsFailedBoundaryIsKeptAndTheRestartNoticeAppended(t *testing.T) {
	const failedTurnNotice = "Your request was not processed. Send it again if you still want me to carry it out."
	ctx, db, a, live, row := claimedOnRealStore(t, "person", func(ctx context.Context, db *state.SessionDB) {
		appendTurnRow(t, ctx, db, map[string]any{"role": "user", "content": "earlier request"})
		appendTurnRow(t, ctx, db, map[string]any{"role": "assistant", "content": failedTurnNotice, "display_kind": failedTurnDisplayKind})
	})
	result := pyjson.ObjectOf("final_response", "", "interrupted", true, "completed", false, "interrupt_message", "Gateway restarting")
	if err := commitAndPublish(ctx, a, live, testRef, row, "", "interrupted", pyjson.ObjectOf("result", result), &turnSettlement{}); err != nil {
		t.Fatal(err)
	}
	if got := failedTurnNotices(t, ctx, db); !slices.Equal(got, []string{failedTurnNotice, restartInterruptNotice}) {
		t.Fatalf("notices %v", got)
	}
}

func TestUserStopIsNotRelabelledWhenShutdownStartsDuringSettlement(t *testing.T) {
	ctx, db, a, live, row := claimedOnRealStore(t, "person", nil)
	runner := a.Runner.(*authorityRunnerDouble)
	var frames []*pyjson.Object
	live.EventStream.Observers[&SessionEventObserver{Observe: func(frame *pyjson.Object) { frames = append(frames, frame) }}] = true
	captured := pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "", "messages", []any{}, "interrupted", true,
		"completed", false, "interrupt_message", "User requested stop"))
	raw, err := sql.Open("sqlite", db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	lock, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	settled := make(chan error, 1)
	go func() {
		settled <- commitAndPublish(ctx, a, live, testRef, row, "", "interrupted", captured, &turnSettlement{})
	}()
	select {
	case err := <-settled:
		t.Fatalf("competing SQLite writer should block settlement: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	runner.draining, runner.stopped = true, true
	if _, err := lock.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := <-settled; err != nil {
		t.Fatal(err)
	}
	var complete []*pyjson.Object
	for _, frame := range frames {
		params, _ := frame.Get("params")
		if kind, _ := params.(*pyjson.Object).Get("type"); kind == "message.complete" {
			payload, _ := params.(*pyjson.Object).Get("payload")
			complete = append(complete, payload.(*pyjson.Object))
		}
	}
	persisted, err := admissionResult(ctx, db, rowString(row, "admission_id"))
	if err != nil {
		t.Fatal(err)
	}
	if len(complete) != 1 {
		t.Fatalf("completions %d", len(complete))
	}
	result, _ := persisted.(*pyjson.Object).Get("result")
	if cause, _ := result.(*pyjson.Object).Get("interrupt_message"); cause != "User requested stop" {
		t.Fatalf("persisted cause %v", cause)
	}
	if _, warned := complete[0].Get("warning"); warned {
		t.Fatalf("a user stop was relabelled: %v", complete[0])
	}
}

func TestRestartDrainTimeoutDefaultStaysZero(t *testing.T) {
	agent, _ := config.Defaults()["agent"].(map[string]any)
	if value, declared := agent["restart_drain_timeout"]; !declared || !pyjson.Equal(value, 0) {
		t.Fatalf("agent.restart_drain_timeout default %v", value)
	}
	if DefaultGatewayRestartDrainTimeout != 0.0 {
		t.Fatalf("restart drain timeout default %v", DefaultGatewayRestartDrainTimeout)
	}
}
