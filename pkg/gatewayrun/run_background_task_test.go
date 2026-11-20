package gatewayrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

func TestBackgroundTaskDeliversReplyUnderCompleteHeader(t *testing.T) {
	server, err := fakellm.New([]any{fakellm.Text{Text: "the answer is 4"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	installTurnFakes(t, server, "")
	adapter := newCleanupCaptureAdapter()
	r := makeTurnRunner(t, adapter)
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-1001", ChatType: "group"}
	if err := r.runBackgroundTask(context.Background(), "what is 2+2", source, "bg_120000_abcdef", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.sent) != 1 {
		t.Fatalf("sent %v", adapter.sent)
	}
	want := "✅ Background task complete\nPrompt: \"what is 2+2\"\n\nthe answer is 4"
	if adapter.sent[0]["chat_id"] != "-1001" || adapter.sent[0]["content"] != want {
		t.Fatalf("sent %#v", adapter.sent[0])
	}
	requests := server.MainRequests()
	if len(requests) != 1 {
		t.Fatalf("model requests %d", len(requests))
	}
	body, _ := json.Marshal(requests[0]["messages"])
	if !json.Valid(body) || !strings.Contains(string(body), "what is 2+2") {
		t.Fatalf("prompt missing from the model request %s", body)
	}
}

func TestBackgroundResponseOfEmptyAndFailedRuns(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := backgroundResponse(ctx, nil); got != "" {
		t.Fatalf("nil result gave %q", got)
	}
	if got := backgroundResponse(ctx, map[string]any{"final_response": nil}); got != "" {
		t.Fatalf("null final gave %q", got)
	}
	if got := backgroundResponse(ctx, map[string]any{"final_response": "", "error": "boom"}); !strings.Contains(got, "boom") {
		t.Fatalf("error result gave %q", got)
	}
}

func TestBackgroundResponseRepairsMediaPathFromAgentRows(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	captureName := "computer_use_0123456789abcdef0123456789abcdef.png"
	canonical := `C:\Users\Alice\AppData\Local\icebath\cache\images\` + captureName
	toolResult, err := json.Marshal(map[string]any{"screenshot_path": canonical})
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{
		"final_response": "Here is the screenshot.\nMEDIA:/Users/Alice/AppData/Local/icebath/cache/images/" + captureName,
		"messages": []map[string]any{
			{"role": "assistant", "tool_calls": []any{map[string]any{"id": "capture", "function": map[string]any{"name": "computer_use"}}}},
			{"role": "tool", "tool_call_id": "capture", "content": string(toolResult)},
		},
	}
	if got, want := backgroundResponse(context.Background(), result), "Here is the screenshot.\nMEDIA:"+canonical; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
