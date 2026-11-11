package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Admits and claims one API input on a fresh epoch of the store
func claimedAPIInput(t *testing.T, ctx context.Context, db *state.SessionDB, instance, requestID string) (int64, *pyjson.Object, *pyjson.Object) {
	t.Helper()
	epoch, err := db.BeginRuntimeEpoch(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "api", SessionID: "api-session", RequestID: requestID,
		Payload: pyjson.ObjectOf("text", "hello")})
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "api-session"})
	if err != nil || row == nil {
		t.Fatalf("claim %v %v", row, err)
	}
	return epoch, admitted, row
}

func TestResultSurvivesRestartWithoutReexecuting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	quietHandleHealth(t)
	ctx := context.Background()
	path := filepath.Join(dir, "state.db")
	db, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "api-session", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	epoch, admitted, row := claimedAPIInput(t, ctx, db, "first", "retry")
	result := func() *pyjson.Object {
		return pyjson.ObjectOf("final_response", "reply", "messages", []any{}, "usage", pyjson.ObjectOf("input_tokens", int64(7), "output_tokens", int64(3)))
	}
	if _, err := retainResult(ctx, db, epoch, row, result()); err != nil {
		t.Fatal(err)
	}
	admissionID := rowString(admitted, "admission_id")
	if stored, err := admissionResult(ctx, db, admissionID); err != nil || !pyjson.Equal(stored, result()) {
		t.Fatalf("stored %v %v", stored, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	epoch, err = db.BeginRuntimeEpoch(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	retried, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "api", SessionID: "api-session", RequestID: "retry",
		Payload: pyjson.ObjectOf("text", "hello")})
	if err != nil {
		t.Fatal(err)
	}
	if rowString(retried, "admission_id") != admissionID {
		t.Fatalf("retried %v", retried)
	}
	if stored, err := admissionResult(ctx, db, admissionID); err != nil || !pyjson.Equal(stored, result()) {
		t.Fatalf("stored after restart %v %v", stored, err)
	}
	if claimed, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "api-session"}); err != nil || claimed != nil {
		t.Fatalf("the settled input was claimed again: %v %v", claimed, err)
	}
}

func TestStaleResultCannotOverwriteSettledOrUnknownClaim(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "api-session", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	epoch, _, row := claimedAPIInput(t, ctx, db, "first", "one")
	if _, err := retainResult(ctx, db, epoch, row, pyjson.ObjectOf("final_response", "first")); err != nil {
		t.Fatal(err)
	}
	if _, err := retainResult(ctx, db, epoch, row, pyjson.ObjectOf("final_response", "late")); refusalReason(err) != "stale_generation" {
		t.Fatalf("late result: %v", err)
	}
	if stored, err := admissionResult(ctx, db, rowString(row, "admission_id")); err != nil || !pyjson.Equal(stored, pyjson.ObjectOf("final_response", "first")) {
		t.Fatalf("stored %v %v", stored, err)
	}
	newer, err := db.BeginRuntimeEpoch(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainResult(ctx, db, epoch, row, pyjson.NewObject()); refusalReason(err) != "stale_epoch" {
		t.Fatalf("stale owner: %v", err)
	}
	if newer == epoch {
		t.Fatal("the second owner kept the first epoch")
	}
}

// Settles one API turn over the growing history, returning its row and the result readers get back
func settledAPITurn(t *testing.T, ctx context.Context, a *SessionAuthority, sid string, n int, history *[]any, reply string) (*pyjson.Object, any) {
	t.Helper()
	text := fmt.Sprintf("question %d", n)
	if _, err := a.DB.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "api", SessionID: sid, RequestID: fmt.Sprintf("r%d", n),
		Payload: pyjson.ObjectOf("text", text)}); err != nil {
		t.Fatal(err)
	}
	row, _, err := a.DB.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: sid})
	if err != nil || row == nil {
		t.Fatalf("claim %v %v", row, err)
	}
	call := fmt.Sprintf("c%d", n)
	*history = append(*history, pyjson.ObjectOf("role", "user", "content", text),
		pyjson.ObjectOf("role", "assistant", "content", "checking", "tool_calls", []any{pyjson.ObjectOf("id", call)}),
		pyjson.ObjectOf("role", "tool", "tool_call_id", call, "content", "tool output"),
		pyjson.ObjectOf("role", "assistant", "content", reply, "reasoning", "why"))
	build := func() *pyjson.Object {
		return pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", reply, "messages", slices.Clone(*history), "completed", true,
			"current_turn_user_idx", int64(len(*history)-4)), "usage", pyjson.ObjectOf("input_tokens", int64(3)))
	}
	compact, err := state.CompactResult(build(), text)
	if err != nil {
		t.Fatal(err)
	}
	expected := redactedResult(compact)
	if _, err := retainResult(ctx, a.DB, a.Epoch, row, build()); err != nil {
		t.Fatal(err)
	}
	return row, expected
}

// Stored size in characters of an admission's result row
func storedResultSize(t *testing.T, ctx context.Context, db *state.SessionDB, admissionID string) int {
	t.Helper()
	var size int
	if err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		return conn.QueryRowContext(ctx, "SELECT length(value) FROM state_meta WHERE key=?", state.TerminalResultPrefix+admissionID).Scan(&size)
	}); err != nil {
		t.Fatal(err)
	}
	return size
}

func TestPerTurnResultBytesAreAboutOneReplyNotTwo(t *testing.T) {
	ctx, db, a := newOwnerAuthority(t)
	if err := db.CreateSession(state.NewSession{ID: "live", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	reply := strings.Repeat("x", 2000)
	var history []any
	var sizes []int
	for n := 1; n <= 10; n++ {
		row, expected := settledAPITurn(t, ctx, a, "live", n, &history, reply)
		admissionID := rowString(row, "admission_id")
		if stored, err := admissionResult(ctx, db, admissionID); err != nil || !pyjson.Equal(stored, expected) {
			t.Fatalf("turn %d reads back %v %v", n, stored, err)
		}
		sizes = append(sizes, storedResultSize(t, ctx, db, admissionID))
	}
	if slices.Max(sizes)-slices.Min(sizes) > 8 {
		t.Fatalf("sizes %v", sizes)
	}
	if float64(sizes[0]) >= float64(len(reply))*1.5 {
		t.Fatalf("result row %d bytes for a %d-char reply", sizes[0], len(reply))
	}
}

func TestAReplyThatDiffersFromTheLastMessageIsKeptVerbatim(t *testing.T) {
	ctx, db, a := newOwnerAuthority(t)
	if err := db.CreateSession(state.NewSession{ID: "live", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "api", SessionID: "live", RequestID: "r",
		Payload: pyjson.ObjectOf("text", "q")}); err != nil {
		t.Fatal(err)
	}
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "live"})
	if err != nil || row == nil {
		t.Fatalf("claim %v %v", row, err)
	}
	build := func() *pyjson.Object {
		return pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "answer\nMEDIA:/tmp/a.png", "current_turn_user_idx", int64(0),
			"messages", []any{pyjson.ObjectOf("role", "user", "content", "q"), pyjson.ObjectOf("role", "assistant", "content", "answer")}),
			"usage", pyjson.NewObject())
	}
	compact, err := state.CompactResult(build(), "q")
	if err != nil {
		t.Fatal(err)
	}
	expected := redactedResult(compact)
	if _, err := retainResult(ctx, db, a.Epoch, row, build()); err != nil {
		t.Fatal(err)
	}
	if stored, err := admissionResult(ctx, db, rowString(row, "admission_id")); err != nil || !pyjson.Equal(stored, expected) {
		t.Fatalf("stored %v %v, expected %v", stored, err, expected)
	}
}
