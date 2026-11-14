package discord

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestYamlConfigSeedsWebsocketHealthWithPrimaryPrecedence(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_DISCORD_LIVENESS_INTERVAL_SECONDS", "")
	t.Setenv("ICEBATH_DISCORD_LIVENESS_FAILURE_THRESHOLD", "")
	seeded, err := applyYAMLConfig(context.Background(), map[string]any{}, map[string]any{
		"websocket_liveness_interval_seconds": 11, "liveness_interval_seconds": 99, "websocket_liveness_failure_threshold": 2,
		"websocket_heartbeat_ack_max_age_seconds": 75, "websocket_max_latency_seconds": 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ICEBATH_DISCORD_LIVENESS_INTERVAL_SECONDS") != "11" || os.Getenv("ICEBATH_DISCORD_LIVENESS_FAILURE_THRESHOLD") != "2" {
		t.Fatal("env not bridged")
	}
	want := map[string]any{"websocket_liveness_interval_seconds": 11, "websocket_liveness_failure_threshold": 2,
		"websocket_heartbeat_ack_max_age_seconds": 75, "websocket_max_latency_seconds": 30}
	if !reflect.DeepEqual(seeded, want) {
		t.Fatalf("seeded %v", seeded)
	}
}
