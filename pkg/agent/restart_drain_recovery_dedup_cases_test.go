package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Agent narrowed to what the override and the flush read, bound to a real gateway session
func drainWindowAgent(t *testing.T, sessionID string) (*AIAgent, *state.SessionDB) {
	t.Helper()
	db := restoreRealDB(t, sessionID, "gateway")
	return &AIAgent{sessionDB: db, sessionDBCreated: true, sessionID: sessionID, flushedDBMessageIDs: map[any]bool{}}, db
}

func TestRestartDrainRecoveryDedup(t *testing.T) {
	caseHome(t)
	t.Run("test_build_turn_context_stamps_the_platform_message_id_on_the_user_turn", func(t *testing.T) {
		a := prologueAgent(t)
		patchPluginHooks(t, func(string, map[string]any) []any { return nil })
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.PersistUserPlatformID = "discord-991" })
		var last *state.Message
		for _, m := range tc.Messages {
			if m.Role == "user" {
				last = m
			}
		}
		if last == nil {
			t.Fatal("no user turn in the built context")
		}
		if last.PlatformMessageID != "discord-991" {
			t.Fatalf("the user turn reached persistence without its platform message id %v", last)
		}
	})
	t.Run("test_persisted_interrupted_turn_is_findable_by_platform_message_id", func(t *testing.T) {
		sessionID := "sess-drain-window"
		a, db := drainWindowAgent(t, sessionID)
		idx, platformID := 0, "discord-4242"
		a.persistUserMessageIdx, a.persistUserMessagePlatformID = &idx, &platformID
		messages := []map[string]any{{"role": "user", "content": "please do the thing"}}
		a.applyPersistUserMessageOverride(messages)
		if _, err := a.flushMessagesToSessionDBWithHistory(messages, nil); err != nil {
			t.Fatal(err)
		}
		if found, err := db.HasPlatformMessageID(t.Context(), sessionID, "discord-4242"); err != nil || !found {
			t.Fatalf("the interrupted turn carries no platform_message_id %v %v", found, err)
		}
	})
	t.Run("test_platform_message_id_survives_a_persist_content_override", func(t *testing.T) {
		sessionID := "sess-override"
		a, db := drainWindowAgent(t, sessionID)
		idx, platformID := 0, "discord-7777"
		a.persistUserMessageIdx, a.persistUserMessageOverride, a.persistUserMessagePlatformID = &idx, "clean transcript text", &platformID
		messages := []map[string]any{{"role": "user", "content": "api-facing text with context"}}
		a.applyPersistUserMessageOverride(messages)
		if _, err := a.flushMessagesToSessionDBWithHistory(messages, nil); err != nil {
			t.Fatal(err)
		}
		if found, err := db.HasPlatformMessageID(t.Context(), sessionID, "discord-7777"); err != nil || !found {
			t.Fatalf("override lost the platform id %v %v", found, err)
		}
	})
}
