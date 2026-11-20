package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestAutoTitleSessionRecordsTitleUsageAgainstStateDB(t *testing.T) {
	titleTestHome(t)
	ctx := context.Background()
	db := newTitleTestDB(t, "cli", "sess-1")
	swapTitleSeam(t, &generateTitleFn, func(ctx context.Context, _ string, _ generateTitleOptions) string {
		provider.RecordAuxUsage(ctx, map[string]any{"model": "fake-model", "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20}},
			"title_generation", "custom", "http://127.0.0.1:9/v1/")
		return "Say hello"
	})
	autoTitleSession(ctx, db, "sess-1", "Say hello", autoTitleSessionOptions{})
	byTask, err := db.AuxiliaryUsageByTask(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(byTask)
	for _, want := range []string{`"title_generation":{`, `"input_tokens":100`, `"output_tokens":20`, `"api_calls":1`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("title usage %s lacks %s", raw, want)
		}
	}
}
