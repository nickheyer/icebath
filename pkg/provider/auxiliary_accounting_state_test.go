package provider

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestStateDBRecordsAuxiliaryUsageThroughTheRecorder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var recorder AuxUsageRecorder = AuxUsageRecorderFor(db)
	if AuxUsageRecorderFor(nil) != nil {
		t.Fatal("a nil store produced a recorder")
	}
	actx := SetAccountingContext(ctx, recorder, "s-aux")
	RecordAuxUsage(actx, map[string]any{"model": "vision-model", "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3}}, "vision", "openrouter", "")
	byTask, err := db.AuxiliaryUsageByTask(ctx, "s-aux")
	if err != nil || byTask.Len() != 1 {
		t.Fatalf("by task %v %v", byTask, err)
	}
	row, _ := byTask.Get("vision")
	text, _ := pyjson.Dumps(row)
	if !strings.Contains(text, "7") || !strings.Contains(text, "3") {
		t.Fatalf("vision row %s", text)
	}
	got := StateAuxiliaryUsage(AuxiliaryUsage{Model: "m", BillingProvider: "openrouter", InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5})
	if got.Model == nil || *got.Model != "m" || got.BillingProvider == nil || *got.BillingProvider != "openrouter" || got.BillingBaseURL != nil ||
		got.InputTokens != 1 || got.OutputTokens != 2 || got.CacheReadTokens != 3 || got.CacheWriteTokens != 4 || got.ReasoningTokens != 5 || got.APICallCount != nil {
		t.Fatalf("converted %+v", got)
	}
}
