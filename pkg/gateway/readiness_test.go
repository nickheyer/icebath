package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Home with a state database holding one probe table
func readinessHome(t *testing.T, withDB bool) (context.Context, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if withDB {
		conn, err := state.ConnectSQLite(filepath.Join(h, "state.db"), state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), "CREATE TABLE probe (id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	return home.WithOverride(context.Background(), h), h
}

func readinessCheckStatus(t *testing.T, result *pyjson.Object, name string) any {
	t.Helper()
	checks, _ := result.Get("checks")
	check, _ := checks.(*pyjson.Object).Get(name)
	status, _ := check.(*pyjson.Object).Get("status")
	return status
}

func TestCollectRuntimeReadinessReportsHealthyLocalRuntime(t *testing.T) {
	ctx, h := readinessHome(t, true)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  provider: openrouter\n  model: test/model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := CollectRuntimeReadiness(ctx, "test/model", pyjson.ObjectOf("gateway_state", "running",
		"platforms", pyjson.ObjectOf("telegram", pyjson.ObjectOf("state", "connected")), "updated_at", "2026-07-09T00:00:00Z"), 2, 0, 0)
	if s, _ := result.Get("status"); s != "ok" {
		t.Fatalf("status = %v, result %v", s, result)
	}
	for _, name := range []string{"state_db", "session_store", "config", "model", "gateway"} {
		if got := readinessCheckStatus(t, result, name); got != "ok" {
			t.Fatalf("%s = %v", name, got)
		}
	}
	checks, _ := result.Get("checks")
	queues, _ := checks.(*pyjson.Object).Get("background_queues")
	if runs, _ := queues.(*pyjson.Object).Get("active_api_runs"); runs != int64(2) {
		t.Fatalf("active_api_runs = %v", runs)
	}
	if disk := readinessCheckStatus(t, result, "disk"); disk != "ok" && disk != "degraded" {
		t.Fatalf("disk = %v", disk)
	}
}

func TestCollectRuntimeReadinessDegradesOnInvalidConfigAndStoppedGateway(t *testing.T) {
	ctx, h := readinessHome(t, false)
	path := filepath.Join(h, "config.yaml")
	if err := os.WriteFile(path, []byte("model: [unterminated"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := CollectRuntimeReadiness(ctx, "", pyjson.ObjectOf("gateway_state", "stopped", "platforms", pyjson.NewObject()), 0, 0, 0)
	if s, _ := result.Get("status"); s != "degraded" {
		t.Fatalf("status = %v", s)
	}
	for _, name := range []string{"config", "model", "gateway"} {
		if got := readinessCheckStatus(t, result, name); got != "degraded" {
			t.Fatalf("%s = %v", name, got)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "model: [unterminated" {
		t.Fatalf("config rewritten: %q", data)
	}
}

func TestReadinessUsesRunningSessionStoreStateOverIndependentProbe(t *testing.T) {
	ctx, _ := readinessHome(t, true)
	unavailable := CollectRuntimeReadiness(ctx, "test/model", pyjson.ObjectOf("gateway_state", "running", "platforms", pyjson.NewObject(),
		"session_store", pyjson.ObjectOf("status", "unavailable")), 0, 0, 0)
	if got := readinessCheckStatus(t, unavailable, "state_db"); got != "ok" {
		t.Fatalf("state_db = %v", got)
	}
	checks, _ := unavailable.Get("checks")
	store, _ := checks.(*pyjson.Object).Get("session_store")
	if !pyjson.Equal(store, pyjson.ObjectOf("status", "unavailable")) {
		t.Fatalf("session_store = %v", store)
	}
	if s, _ := unavailable.Get("status"); s != "degraded" {
		t.Fatalf("status = %v", s)
	}
	recovered := CollectRuntimeReadiness(ctx, "test/model", pyjson.ObjectOf("gateway_state", "running", "platforms", pyjson.NewObject(),
		"session_store", pyjson.ObjectOf("status", "ok")), 0, 0, 0)
	checks, _ = recovered.Get("checks")
	store, _ = checks.(*pyjson.Object).Get("session_store")
	if !pyjson.Equal(store, pyjson.ObjectOf("status", "ok")) {
		t.Fatalf("session_store = %v", store)
	}
}
