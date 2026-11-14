package dashboard

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Default profile home holding the given gateway_state.json text, none when text is empty
func stoppedCheckHome(t *testing.T, text string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if text != "" {
		if err := os.WriteFile(filepath.Join(h, "gateway_state.json"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntentionallyStoppedTrueOnDesiredStateStopped(t *testing.T) {
	stoppedCheckHome(t, `{"gateway_state":"stopped","desired_state":"stopped"}`)
	if !gatewayIntentionallyStopped(context.Background(), "default") {
		t.Fatal("not stopped")
	}
}

func TestIntentionallyStoppedFalseWhenDesiredRunning(t *testing.T) {
	stoppedCheckHome(t, `{"gateway_state":"startup_failed","desired_state":"running"}`)
	if gatewayIntentionallyStopped(context.Background(), "default") {
		t.Fatal("stopped")
	}
}

func TestIntentionallyStoppedFalseOnLegacyFileWithoutDesiredState(t *testing.T) {
	stoppedCheckHome(t, `{"gateway_state":"stopped"}`)
	if gatewayIntentionallyStopped(context.Background(), "default") {
		t.Fatal("stopped")
	}
}

func TestIntentionallyStoppedFalseOnMissingOrBadFile(t *testing.T) {
	stoppedCheckHome(t, "")
	ctx := context.Background()
	if gatewayIntentionallyStopped(ctx, "default") {
		t.Fatal("missing file stopped")
	}
	h := os.Getenv("ICEBATH_HOME")
	for _, text := range []string{"{not json", `["list"]`} {
		if err := os.WriteFile(filepath.Join(h, "gateway_state.json"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if gatewayIntentionallyStopped(ctx, "default") {
			t.Fatalf("%s stopped", text)
		}
	}
}

func TestIntentionallyStoppedFalseWhenProfileResolutionFails(t *testing.T) {
	stoppedCheckHome(t, `{"desired_state":"stopped"}`)
	if gatewayIntentionallyStopped(context.Background(), "ghost") {
		t.Fatal("unresolvable profile stopped")
	}
}

func TestFireEndpointDefaultPort(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("API_SERVER_PORT", "")
	os.Unsetenv("API_SERVER_PORT")
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "")
	os.Unsetenv("GATEWAY_MULTIPLEX_PROFILES")
	ctx := context.Background()
	if url := gatewayFireEndpoint(ctx, "default", h); url != "http://127.0.0.1:8642/api/cron/fire" {
		t.Fatalf("url %s", url)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(fmt.Sprintf("platforms:\n  api_server:\n    extra:\n      port: %d\n", port)), 0o644); err != nil {
		t.Fatal(err)
	}
	status, body, err := forwardCronFireToGateway(ctx, "default", "j1", "Bearer x")
	if err != nil || status != nil || body != nil {
		t.Fatalf("forward %v %v %v", status, body, err)
	}
}

func TestFireEndpointConfigYamlPortWins(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("API_SERVER_PORT", "9999")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("platforms:\n  api_server:\n    extra:\n      port: 8700\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if url := gatewayFireEndpoint(context.Background(), "default", h); url != "http://127.0.0.1:8700/api/cron/fire" {
		t.Fatalf("url %s", url)
	}
}

func TestFireEndpointProfileEnvPort(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("API_SERVER_PORT", "9999")
	workerHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(workerHome, ".env"), []byte("API_SERVER_PORT=8701\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if url := gatewayFireEndpoint(context.Background(), "worker_alpha", workerHome); url != "http://127.0.0.1:8701/api/cron/fire" {
		t.Fatalf("url %s", url)
	}
}
