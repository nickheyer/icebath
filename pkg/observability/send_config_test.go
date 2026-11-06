package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func sharedConfig(pairs ...any) map[string]any {
	shared := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		shared[pairs[i].(string)] = pairs[i+1]
	}
	return map[string]any{"telemetry": map[string]any{"shared_metrics": shared}}
}

func TestSendConfigDefaults(t *testing.T) {
	shared := config.Defaults()["telemetry"].(map[string]any)["shared_metrics"].(map[string]any)
	if shared["enabled"] != false || shared["send"] != false || shared["endpoint"] != DefaultEndpoint {
		t.Fatal(shared)
	}
	if !strings.HasPrefix(DefaultEndpoint, "https://") {
		t.Fatal(DefaultEndpoint)
	}
	if r := ResolveSendConfig(map[string]any{}); r.Enabled || r.Send {
		t.Fatalf("%+v", r)
	}
	if ResolveSendConfig(nil).Send {
		t.Fatal("nil config sends")
	}
	if r := ResolveSendConfig(config.Defaults()); r.Enabled || r.Send || r.Endpoint != DefaultEndpoint {
		t.Fatalf("%+v", r)
	}
}

func TestSendRequiresCollection(t *testing.T) {
	if r := ResolveSendConfig(sharedConfig("enabled", true)); !r.Enabled || r.Send {
		t.Fatalf("%+v", r)
	}
	if r := ResolveSendConfig(sharedConfig("enabled", true, "send", true)); !r.Send {
		t.Fatalf("%+v", r)
	}
	if r := ResolveSendConfig(sharedConfig("enabled", false, "send", true)); r.Send || r.Enabled {
		t.Fatalf("%+v", r)
	}
}

func TestSendEndpointPrecedence(t *testing.T) {
	if r := ResolveSendConfig(sharedConfig("enabled", true, "send", true, "endpoint", "https://example.test/v1")); r.Endpoint != "https://example.test/v1" {
		t.Fatal(r.Endpoint)
	}
	for _, name := range []string{"ICEBATH_TELEMETRY_ENDPOINT", "TELEMETRY_ENDPOINT", "ICEBATH_SHARED_METRICS_ENDPOINT"} {
		t.Setenv(name, "https://attacker.test/v1")
	}
	if r := ResolveSendConfig(sharedConfig("enabled", true, "send", true)); r.Endpoint != DefaultEndpoint {
		t.Fatal(r.Endpoint)
	}
	if r := ResolveSendConfig(sharedConfig("enabled", true, "send", true, "endpoint", "   ")); r.Endpoint != DefaultEndpoint {
		t.Fatal(r.Endpoint)
	}
	if r := ResolveSendConfig(sharedConfig("enabled", true, "send", true, "endpoint", "  https://staging.test/v1  ")); r.Endpoint != "https://staging.test/v1" {
		t.Fatal(r.Endpoint)
	}
}

func TestSendTransportSafety(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	r := ResolveSendConfig(sharedConfig("enabled", true, "send", true, "endpoint", "http://example.test/v1"))
	slog.SetDefault(previous)
	if r.Send || !r.Enabled || !strings.Contains(buf.String(), "https") {
		t.Fatalf("%+v %s", r, buf.String())
	}
	for _, endpoint := range []string{"http://localhost:8099/v1/telemetry", "http://127.0.0.1:8099/v1/telemetry", "http://[::1]:8099/v1"} {
		if !ResolveSendConfig(sharedConfig("enabled", true, "send", true, "endpoint", endpoint)).Send {
			t.Fatal(endpoint)
		}
	}
	for _, endpoint := range []string{"ftp://example.test/v1", "ftp://localhost/v1/telemetry", "gopher://localhost/v1/telemetry",
		"ws://127.0.0.1/v1/telemetry", "http://[::1/v1"} {
		if ResolveSendConfig(sharedConfig("enabled", true, "send", true, "endpoint", endpoint)).Send {
			t.Fatal(endpoint)
		}
	}
}
