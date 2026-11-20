package acp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestUpdatesModelConfig(t *testing.T) {
	db := provenanceDB(t)
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "acp", Model: "gpt-4"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionMeta(ctx, "s1", `{"cwd": "/new/path", "provider": "openai"}`, nil); err != nil {
		t.Fatal(err)
	}
	stored := jsonField(t, getRow(t, db, "s1"), "model_config")
	if stored.GetOr("cwd", nil) != "/new/path" || stored.GetOr("provider", nil) != "openai" {
		t.Fatalf("stored = %v", stored.Vals)
	}
}
