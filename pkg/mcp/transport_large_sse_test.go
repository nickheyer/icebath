package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// catalogSSEServer answers every streamable HTTP request as one SSE event and lists count tools of descBytes each
func catalogSSEServer(t *testing.T, count, descBytes int) *httptest.Server {
	t.Helper()
	description := strings.Repeat("d", descBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": LatestHandshakeVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "catalog", "version": "1"}}
		case "tools/list":
			list := make([]any, 0, count)
			for i := range count {
				list = append(list, map[string]any{"name": fmt.Sprintf("tool_%d", i), "description": description,
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
			}
			result = map[string]any{"tools": list}
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "catalog-session")
		_, _ = fmt.Fprintf(w, "event: message\r\ndata: %s\r\n\r\n", payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamableHTTPToolsListOverOneMiBConnects(t *testing.T) {
	const count, descBytes, sdkDefaultEventLimit = 162, 6_600, 1024 * 1024
	if count*descBytes <= sdkDefaultEventLimit {
		t.Fatal("catalog must exceed one MiB")
	}
	srv := catalogSSEServer(t, count, descBytes)
	seedServers(t, map[string]any{"catalog": map[string]any{"url": srv.URL + "/mcp"}})
	t.Cleanup(func() { ShutdownMCPServers(context.Background(), nil, nil, 10*time.Second) })
	ui := &cliUI{}
	code := cmdMCPTest(context.Background(), &tools.Env{}, ui, "catalog")
	out := ui.out.String()
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, fmt.Sprintf("Tools discovered: %d", count)) {
		t.Fatal(out)
	}
}
