package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestCompressionChildKeepsParentSource(t *testing.T) {
	for _, parentSource := range []string{"tool", "oneshot", "kanban"} {
		t.Run(parentSource, func(t *testing.T) {
			t.Setenv("ICEBATH_SESSION_ID", "")
			db := newRecoveryDB(t)
			ctx := context.Background()
			if err := db.CreateSession(state.NewSession{ID: "parent", Source: parentSource}); err != nil {
				t.Fatal(err)
			}
			for _, row := range []map[string]any{{"role": "user", "content": "hello"}, {"role": "assistant", "content": "world"}} {
				if _, err := db.AppendMessage(ctx, "parent", row, state.AppendGuards{}); err != nil {
					t.Fatal(err)
				}
			}
			a := &AIAgent{sessionDB: db, sessionID: "parent", platform: "cli", model: "m", cachedSystemPrompt: "sys"}
			a.contextCompressor = newStubEngine(200000, 0.5)
			lease := &compressionLease{a: a, db: db, sid: "parent", ttl: 60}
			messages := []map[string]any{{"role": "user", "content": "hello"}, {"role": "assistant", "content": "world"}}
			if err := publishRotatedCompaction(ctx, a, messages, []map[string]any{{"role": "user", "content": "[handoff]"}}, "sys", lease, "parent", "none"); err != nil {
				t.Fatal(err)
			}
			if a.sessionID == "parent" {
				t.Fatal("compaction did not rotate onto a child")
			}
			row, err := db.GetSession(ctx, a.sessionID)
			if err != nil || row == nil {
				t.Fatalf("child row %v err=%v", row, err)
			}
			if source, _ := row.Get("source"); source != parentSource {
				t.Fatalf("child source = %v, want %q", source, parentSource)
			}
		})
	}
}
