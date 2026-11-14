package gatewayapi

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestDeclaredKeyIsDurableAndCannotRebind(t *testing.T) {
	ctx, owner, api := newOwnerAPI(t)
	admit := func(sid, key string, bind bool) (*gateway.APIAdmission, error) {
		return gateway.AdmitAPITurn(ctx, api, &gateway.APITurn{SessionID: sid, GatewaySessionKey: key, BindDeclaredConversation: bind,
			UserMessage: "hello", ConversationHistory: []any{}})
	}
	first, err := admit("first", "conversation", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := api.declaredConversationSession(ctx, "conversation"); got != first.Ref.SessionID {
		t.Fatalf("declared session = %q, want %q", got, first.Ref.SessionID)
	}
	clear(owner.Sessions)
	retried, err := admit("second", "conversation", true)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Ref != first.Ref {
		t.Fatalf("retried ref = %v, want %v", retried.Ref, first.Ref)
	}
	if _, err := admit(first.Ref.SessionID, "other", false); err != nil {
		t.Fatal(err)
	}
	if got := api.declaredConversationSession(ctx, "other"); got != "" {
		t.Fatalf("explicit selection bound header other to %q", got)
	}
	_, err = admit(first.Ref.SessionID, "other", true)
	requireRefusal(t, err, "admission_conflict")
	if got := api.declaredConversationSession(ctx, "conversation"); got != first.Ref.SessionID {
		t.Fatalf("declared session = %q after refused rebind", got)
	}
}

func TestPersistedOwnerScopeIsPrivateStrictAndLegacySafe(t *testing.T) {
	ctx, _, api := newOwnerAPI(t)
	scope := strings.Repeat("a", 64)
	admitted, err := gateway.AdmitAPITurn(ctx, api, &gateway.APITurn{SessionID: "private-owner", ActiveRunID: "run_private_owner",
		UserMessage: "hello", ConversationHistory: []any{}, RunOwnerScope: scope})
	if err != nil {
		t.Fatal(err)
	}
	payload := admitted.Row.GetOr("payload", nil).(*pyjson.Object)
	if stored := payload.GetOr("api_turn_v1", nil).(*pyjson.Object).GetOr("run_owner_scope", nil); stored != scope {
		t.Fatalf("stored scope = %v", stored)
	}
	prepared, err := gateway.PrepareAPIExecution(ctx, admitted.Authority, admitted.Ref, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := prepared.Get("run_owner_scope"); leaked {
		t.Fatal("prepared execution carries run_owner_scope")
	}
	projection, err := runProjection(ctx, api, "run_private_owner")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := pyjson.Dumps(projection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "run_owner_scope") {
		t.Fatalf("projection leaks the owner scope: %s", encoded)
	}
	if owns, err := gateway.OwnsAPIRun(ctx, api, "run_private_owner", scope); err != nil || !owns {
		t.Fatalf("owns = %v, %v", owns, err)
	}
	for _, malformed := range []any{strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("g", 64), int64(7)} {
		_, err := gateway.AdmitAPITurn(ctx, api, &gateway.APITurn{SessionID: "invalid-owner", UserMessage: "x",
			ConversationHistory: []any{}, RunOwnerScope: malformed})
		requireRefusal(t, err, "invalid_params")
	}
	if _, err := gateway.AdmitAPITurn(ctx, api, &gateway.APITurn{SessionID: "legacy-ownerless", ActiveRunID: "run_legacy_ownerless",
		UserMessage: "old", ConversationHistory: []any{}}); err != nil {
		t.Fatal(err)
	}
	if owns, err := gateway.OwnsAPIRun(ctx, api, "run_legacy_ownerless", scope); err != nil || owns {
		t.Fatalf("legacy run owned = %v, %v", owns, err)
	}
}

func TestCompressedAPIConversationKeepsItsRootQueueAndFollowsTheTip(t *testing.T) {
	for _, queuedFollower := range []bool{false, true} {
		name := "next-request"
		if queuedFollower {
			name = "queued-follower"
		}
		t.Run(name, func(t *testing.T) {
			ctx, owner, api := newOwnerAPI(t)
			admit := func(requestID string) *gateway.APIAdmission {
				t.Helper()
				admitted, err := gateway.AdmitAPITurn(ctx, api, &gateway.APITurn{RequestID: requestID, GatewaySessionKey: "compressed",
					BindDeclaredConversation: true, UserMessage: requestID, ConversationHistory: []any{}})
				if err != nil {
					t.Fatal(err)
				}
				return admitted
			}
			first := admit("first")
			ref := first.Ref
			var follower *gateway.APIAdmission
			if queuedFollower {
				follower = admit("follower")
			}
			if _, _, err := owner.DB.ClaimSessionInput(ctx, state.ClaimInput{Epoch: owner.Epoch, SessionID: ref.SessionID}); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.DB.AppendMessage(ctx, ref.SessionID, map[string]any{"role": "user", "content": "first"}, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
			child := ref.SessionID + "-child"
			publish := state.NewCompressionChildPublish(ref.SessionID, child, "api_server", []map[string]any{{"role": "user", "content": "summary"}})
			publish.RequireCompressionLease = false
			if err := owner.DB.PublishCompressionChild(ctx, publish); err != nil {
				t.Fatal(err)
			}
			route := owner.Sessions[ref.SessionID].Route
			store := owner.Runner.SessionStore()
			if entry, err := store.AdvanceCompressionSession(ctx, route, ref.SessionID, child); err != nil || entry == nil {
				t.Fatalf("advance = %v, %v", entry, err)
			}
			if queuedFollower {
				if err := gateway.CheckAPITurn(ctx, owner, ref, follower.Row.GetOr("payload", nil).(*pyjson.Object)); err != nil {
					t.Fatal(err)
				}
			} else {
				again := admit("second")
				if again.Ref != ref || again.Row.GetOr("target_session_id", nil) != ref.SessionID {
					t.Fatalf("second admission = %v %v", again.Ref, again.Row.GetOr("target_session_id", nil))
				}
			}
			if tip, _ := store.PeekSessionID(route); tip != child {
				t.Fatalf("route tip = %q, want %q", tip, child)
			}
			rows, err := owner.DB.ListSessionAdmissions(ctx, ref.SessionID, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.GetOr("target_session_id", nil) != ref.SessionID {
					t.Fatalf("admission targets %v", row.GetOr("target_session_id", nil))
				}
			}
			if first.Row.GetOr("target_session_id", nil) != ref.SessionID {
				t.Fatalf("first target = %v", first.Row.GetOr("target_session_id", nil))
			}
		})
	}
}
