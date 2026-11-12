package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Canonical authority over a fresh profile store that its routing store shares
func newOwnerAuthority(t *testing.T) (context.Context, *state.SessionDB, *SessionAuthority) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	dir := filepath.Join(home.Dir(ctx), "sessions")
	store := makeTestSessionStore(ctx, t, dir, db, &GatewayConfig{SessionsDir: dir})
	epoch, err := db.BeginRuntimeEpoch(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, NewSessionAuthority(&authorityRunnerDouble{store: store}, "default", "first", db, epoch)
}

// Moves the owner to a fresh epoch that recovers the inputs the previous one lost
func restartOwner(t *testing.T, ctx context.Context, db *state.SessionDB, a *SessionAuthority) {
	t.Helper()
	epoch, err := db.BeginRuntimeEpoch(ctx, "restart")
	if err != nil {
		t.Fatal(err)
	}
	a.Epoch = epoch
	if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardClosureFailureKeepsUnknownFenceAndRetryIsExact(t *testing.T) {
	for _, rotated := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "rotated"}[rotated], func(t *testing.T) {
			ctx, db, a := newOwnerAuthority(t)
			if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
				t.Fatal(err)
			}
			a.Sessions["s"] = NewLiveSession(nil, "route")
			ref := SessionRef{ProfileID: a.ProfileID, SessionID: "s"}
			actor := Principal{Subject: "human", ProfileID: a.ProfileID, Capabilities: map[string]bool{"session:submit": true, "session:control": true}, TransportID: "t"}
			source := filepath.Join(filepath.Dir(db.Path()), "input.png")
			if err := os.WriteFile(source, []byte("image"), 0o600); err != nil {
				t.Fatal(err)
			}
			references, err := CaptureNativeMedia(ctx, []string{source})
			if err != nil {
				t.Fatal(err)
			}
			retained := rowString(references[0].(*pyjson.Object), "path")
			first, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "human", SessionID: "s", RequestID: "lost",
				Payload: pyjson.ObjectOf("text", "LOST", "attachments_v1", pyjson.ObjectOf("media", references, "media_types", []any{"image/png"}))})
			if err != nil {
				t.Fatal(err)
			}
			firstID := rowString(first, "admission_id")
			claim, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "s"})
			if err != nil || claim == nil {
				t.Fatalf("claim %v %v", claim, err)
			}
			generation := rowGeneration(claim)
			target := "s"
			if rotated {
				if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: "s", ChildSessionID: "child", Source: "test",
					Messages: []map[string]any{{"role": "assistant", "content": "summary"}}}); err != nil {
					t.Fatal(err)
				}
				target = "child"
			}
			appendRow := func(fields map[string]any) {
				t.Helper()
				if _, err := db.AppendMessage(ctx, target, fields, state.AppendGuards{}); err != nil {
					t.Fatal(err)
				}
			}
			latestRole := func() string {
				t.Helper()
				role, err := db.LatestConversationRole(ctx, target)
				if err != nil {
					t.Fatal(err)
				}
				return role
			}
			appendRow(map[string]any{"role": "user", "content": "LOST"})
			if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "human", SessionID: "s", RequestID: "next",
				Payload: pyjson.ObjectOf("text", "FOLLOWER")}); err != nil {
				t.Fatal(err)
			}
			restartOwner(t, ctx, db, a)
			holdDrains(t)
			closeTail := closeDiscardedTurn
			t.Cleanup(func() { closeDiscardedTurn = closeTail })
			closeDiscardedTurn = func(ctx context.Context, db *state.SessionDB, conn state.SQLiteConn, row *pyjson.Object, notice, replaceAfter any, metadata *pyjson.Object) (bool, error) {
				if _, err := closeTail(ctx, db, conn, row, notice, replaceAfter, metadata); err != nil {
					return false, err
				}
				return false, &os.PathError{Op: "write", Path: "transcript", Err: errors.New("closure failed")}
			}
			if _, err := a.ResolveUnknown(ctx, actor, ref, firstID, generation); err == nil || !strings.Contains(err.Error(), "closure failed") {
				t.Fatalf("the closure failure did not surface: %v", err)
			}
			if row, err := db.GetSessionAdmission(ctx, firstID); err != nil || rowString(row, "status") != "unknown" {
				t.Fatalf("fence %v %v", row, err)
			}
			if role := latestRole(); role != "user" {
				t.Fatalf("tail role %s", role)
			}
			if !onDisk(retained) {
				t.Fatal("the failed discard released its media")
			}
			if _, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "s"}); refusalReason(err) != "unknown_execution" {
				t.Fatalf("claim behind the fence: %v", err)
			}
			closeDiscardedTurn = closeTail
			settled, err := a.ResolveUnknown(ctx, actor, ref, firstID, generation)
			if err != nil {
				t.Fatal(err)
			}
			if onDisk(retained) {
				t.Fatal("the discarded input kept its media")
			}
			if role := latestRole(); role != "assistant" {
				t.Fatalf("tail role after discard %s", role)
			}
			successor, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "s"})
			if err != nil || rowString(successor, "request_id") != "next" {
				t.Fatalf("successor %v %v", successor, err)
			}
			appendRow(map[string]any{"role": "user", "content": "FOLLOWER"})
			again, err := a.ResolveUnknown(ctx, actor, ref, firstID, generation)
			if err != nil || !reflect.DeepEqual(again, settled) {
				t.Fatalf("retry %+v %v, settled %+v", again, err, settled)
			}
			if role := latestRole(); role != "user" {
				t.Fatal("retry closed a successor turn")
			}
		})
	}
}

func TestDiscardClosesTurnLostWhileParkedOnATool(t *testing.T) {
	ctx, db, a := newOwnerAuthority(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	a.Sessions["s"] = NewLiveSession(nil, "route")
	ref := SessionRef{ProfileID: a.ProfileID, SessionID: "s"}
	actor := Principal{Subject: "human", ProfileID: a.ProfileID, Capabilities: map[string]bool{"session:submit": true, "session:control": true}, TransportID: "t"}
	first, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "human", SessionID: "s", RequestID: "lost",
		Payload: pyjson.ObjectOf("text", "LOST")})
	if err != nil {
		t.Fatal(err)
	}
	claim, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "s"})
	if err != nil || claim == nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	for _, fields := range []map[string]any{
		{"role": "user", "content": "LOST"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_gate", "type": "function",
			"function": map[string]any{"name": "clarify", "arguments": "{}"}}}},
	} {
		if _, err := db.AppendMessage(ctx, "s", fields, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	restartOwner(t, ctx, db, a)
	holdDrains(t)
	if _, err := a.ResolveUnknown(ctx, actor, ref, rowString(first, "admission_id"), rowGeneration(claim)); err != nil {
		t.Fatal(err)
	}
	history, err := db.GetMessagesAsConversation(ctx, "s", state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	history = append(history, map[string]any{"role": "user", "content": "FOLLOWER"})
	if _, err := state.RepairMessageSequence(&history); err != nil {
		t.Fatal(err)
	}
	var asked []string
	for _, message := range history {
		if message["role"] == "user" {
			asked = append(asked, message["content"].(string))
		}
	}
	if !slices.Equal(asked, []string{"LOST", "FOLLOWER"}) {
		t.Fatalf("history %v", history)
	}
}
