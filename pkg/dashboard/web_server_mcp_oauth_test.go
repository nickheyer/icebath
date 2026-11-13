package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestDashboardMCPOAuthFailureRestoresTokensAndMarksTheFlowFailed(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	tokens := filepath.Join(h, "mcp-tokens", "fixture.json")
	if err := os.MkdirAll(filepath.Dir(tokens), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokens, []byte(`{"access_token":"kept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	flow := mcp.NewDashboardOAuthFlow("flow-1", "fixture", "", h, "http://127.0.0.1:9/callback", false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runDashboardMCPOAuth(ctx, &tools.Env{Home: h}, flow, map[string]any{"url": srv.URL + "/mcp", "connect_timeout": 5})
	if !flow.WorkerDone() || flow.Status() == "approved" || flow.Failure() == "" {
		t.Fatalf("done %v status %q failure %q", flow.WorkerDone(), flow.Status(), flow.Failure())
	}
	if b, err := os.ReadFile(tokens); err != nil || string(b) != `{"access_token":"kept"}` {
		t.Fatalf("token file %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(h, "config.yaml")); err == nil {
		t.Fatal("a failed probe saved the server")
	}
	if lock := mcpOAuthTransaction(flow); !lock.TryLock() {
		t.Fatal("transaction lock still held after the worker returned")
	}
}
