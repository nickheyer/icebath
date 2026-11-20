package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Child half of the exit test, registers the target, fires the hook and leaves through the exit table without a flush
func TestOutboundWebhookExitChild(t *testing.T) {
	url := os.Getenv("ICEBATH_TEST_WEBHOOK_URL")
	if url == "" {
		t.Skip("child mode only")
	}
	cfg := map[string]any{"hooks": map[string]any{"outbound": []any{map[string]any{"url": url, "events": []any{"on_session_end"}}}}}
	ctx := context.Background()
	if _, err := plugins.RegisterOutboundWebhooksFromConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	plugins.GetPluginManager(ctx).InvokeHook(ctx, "on_session_end", map[string]any{"session_id": "exit_test"})
	exit(0)
}

func TestEventsEnqueuedAtExitStillDelivered(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOutboundWebhookExitChild$")
	cmd.Env = append(os.Environ(), "ICEBATH_TEST_WEBHOOK_URL="+server.URL+"/hook", "ICEBATH_HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("captured %d deliveries", len(bodies))
	}
	var payload map[string]any
	if err := json.Unmarshal(bodies[0], &payload); err != nil {
		t.Fatal(err)
	}
	if payload["session_id"] != "exit_test" {
		t.Fatalf("%s", bodies[0])
	}
}
