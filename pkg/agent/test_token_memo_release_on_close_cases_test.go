package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestCompressionTelemetryDoesNotPinTheOneShotSummaryPrompt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prompt := strings.Repeat("summarize this serialized transcript ", 20000)
	probe := []map[string]any{{"content": prompt}}
	estimateRowsRough(context.Background(), []map[string]any{{"role": "user", "content": prompt}})
	if provider.ForgetMessageTokenMemo(probe, nil) != 1 {
		t.Fatal("a plain estimate must pin the prompt so the release below is observable")
	}
	recorder := &CompressionTelemetry{Active: jsonutil.NewObject()}
	recorder.Active.Set("effective_aux_context", nil)
	recorder.RecordAuxCall(context.Background(), AuxCompressionCall{PromptMessages: []map[string]any{{"role": "user", "content": prompt}}, DurationMs: 1})
	if n, _ := recorder.Active.Get("aux_prompt_tokens"); n.(int) <= 0 {
		t.Fatalf("aux_prompt_tokens %v", n)
	}
	if pinned := provider.ForgetMessageTokenMemo(probe, nil); pinned != 0 {
		t.Fatalf("summary prompt still pinned by %d memo entries", pinned)
	}
}
