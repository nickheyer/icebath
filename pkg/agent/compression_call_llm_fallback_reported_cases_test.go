package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

const compressionFallbackSummary = "## Goal\nAnswer the numbered questions.\n## Progress\nQuestions answered.\n## Next Steps\nContinue.\n"

func compressionSSEChunk(text, finish string) string {
	delta := map[string]any{}
	if text != "" {
		delta["content"] = text
	}
	var finishValue any
	if finish != "" {
		finishValue = finish
	}
	payload, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishValue}}})
	return "data: " + string(payload) + "\n\n"
}

// Main model endpoint answering every chat completion with the summary and recording the requested models
func compressionMainModelEndpoint(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		model, _ := body["model"].(string)
		mu.Lock()
		seen = append(seen, model)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, compressionSSEChunk(compressionFallbackSummary, "")+compressionSSEChunk("", "stop")+"data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// Loopback URL whose port nothing listens on
func compressionClosedPortURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

func compressionFallbackHistory() []map[string]any {
	var messages []map[string]any
	for i := 0; i < 30; i++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": fmt.Sprintf("Question %d: explain topic %d. ", i, i) + strings.Repeat("lorem ipsum ", 200)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("Answer %d: ", i) + strings.Repeat("dolor sit amet ", 200)})
	}
	return messages
}

func TestSummaryRecoveredOnTheMainModelByCallLlmIsReported(t *testing.T) {
	mainURL, seen := compressionMainModelEndpoint(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	config := "model:\n  provider: custom\n  base_url: " + mainURL + "\n  api_key: dummy\n  default: main-fake\n  context_length: 64000\n" +
		"auxiliary:\n  compression:\n    provider: custom\n    base_url: " + compressionClosedPortURL(t) + "\n    api_key: dummy\n    model: dead-summarizer\n    timeout: 20\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := home.WithOverride(context.Background(), h)
	t.Cleanup(func() { provider.ShutdownCachedClients(ctx) })
	contextLength := 64000
	opts := DefaultContextCompressorOptions("main-fake")
	opts.Provider, opts.BaseURL, opts.APIKey, opts.ConfigContextLength, opts.QuietMode = "custom", mainURL, "dummy", &contextLength, true
	compressor := NewContextCompressor(ctx, opts)

	compressed, err := compressor.Compress(compressionFallbackHistory(), nil, nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) >= 60 || strings.Join(seen(), ",") != "main-fake" {
		t.Fatalf("compressed %d messages, main model requests %v", len(compressed), seen())
	}
	fallbackUsed, _ := compressor.Last.Get("fallback_used")
	method, _ := compressor.Last.Get("method")
	if fallbackUsed != true || method != "aux_fallback_main" {
		t.Fatalf("telemetry fallback_used %v method %v", fallbackUsed, method)
	}
	if compressor.LastAuxModelFailureModel != "dead-summarizer" || !strings.Contains(compressor.LastAuxModelFailureError, "connection error") {
		t.Fatalf("aux failure %q %q", compressor.LastAuxModelFailureModel, compressor.LastAuxModelFailureError)
	}
}
