package gateway

import (
	"reflect"
	"testing"
	"time"
)

func TestOldResetConfigCannotRotateDurableConversation(t *testing.T) {
	for _, mode := range []string{"idle", "daily", "both", "none"} {
		t.Run(mode, func(t *testing.T) {
			ctx, db := newTestStateDB(t)
			policy := map[string]any{"mode": mode, "idle_minutes": int64(1)}
			cfg, err := GatewayConfigFromDict(ctx, map[string]any{"default_reset_policy": policy,
				"reset_by_type":     map[string]any{"dm": policy},
				"reset_by_platform": map[string]any{"telegram": policy}})
			if err != nil {
				t.Fatal(err)
			}
			store := makeTestSessionStore(ctx, t, t.TempDir(), db, cfg)
			source := &SessionSource{Platform: PlatformTelegram, ChatID: "time-invariant", UserID: strPtr("test")}
			old, err := store.GetOrCreateSession(ctx, source, false, true)
			if err != nil {
				t.Fatal(err)
			}
			messages := []map[string]any{{"role": "user", "content": "keep my conversation"},
				{"role": "assistant", "content": "including after a restart"}}
			for _, m := range messages {
				if err := store.AppendToTranscript(ctx, old.SessionID, m, false); err != nil {
					t.Fatal(err)
				}
			}
			store.mu.Lock()
			old.UpdatedAt = pyDatetime{T: time.Now().Add(-72 * time.Hour)}
			old.ResumePending = true
			marked := old.UpdatedAt
			old.LastResumeMarkedAt = &marked
			if err := store.save(ctx); err != nil {
				store.mu.Unlock()
				t.Fatal(err)
			}
			store.mu.Unlock()
			roleContent := func(sessionID string) []map[string]any {
				t.Helper()
				rows, err := store.LoadTranscript(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				out := []map[string]any{}
				for _, r := range rows {
					m := r.(map[string]any)
					out = append(out, map[string]any{"role": m["role"], "content": m["content"]})
				}
				return out
			}
			endReason := func() any {
				t.Helper()
				row, err := db.GetSession(ctx, old.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				v, _ := row.Get("end_reason")
				return v
			}
			routed, err := store.GetOrCreateSession(ctx, source, false, true)
			if err != nil || routed.SessionID != old.SessionID {
				t.Fatalf("routed %v %v", routed, err)
			}
			if got := roleContent(routed.SessionID); !reflect.DeepEqual(got, messages) {
				t.Fatalf("transcript %v", got)
			}
			if r := endReason(); r != nil {
				t.Fatalf("end_reason %v", r)
			}
			if _, err := db.ReplaceGatewayRoutingEntries(ctx, map[string]string{}, store.routingScope()); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			store.entries = newSessionEntries()
			store.mu.Unlock()
			recovered, err := store.GetOrCreateSession(ctx, source, false, true)
			if err != nil || recovered.SessionID != old.SessionID {
				t.Fatalf("recovered %v %v", recovered, err)
			}
			explicit, err := store.ResetSession(ctx, recovered.SessionKey, nil)
			if err != nil || explicit == nil || explicit.SessionID == old.SessionID {
				t.Fatalf("explicit %v %v", explicit, err)
			}
			if r := endReason(); r != "session_reset" {
				t.Fatalf("end_reason %v", r)
			}
			if got := roleContent(old.SessionID); !reflect.DeepEqual(got, messages) {
				t.Fatalf("transcript after reset %v", got)
			}
		})
	}
}
