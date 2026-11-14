package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Whether the home's log file holds needle
func logContains(h, filename, needle string) bool {
	data, err := os.ReadFile(filepath.Join(h, "logs", filename))
	return err == nil && strings.Contains(string(data), needle)
}

func TestMultiplexGatewayRoutesProfileRecordsToTheirOwnLogs(t *testing.T) {
	logging.ResetQueuedHandlers()
	t.Cleanup(logging.ResetQueuedHandlers)
	root := t.TempDir()
	defaultHome := filepath.Join(root, "default")
	betaHome := filepath.Join(defaultHome, "profiles", "beta")
	if err := os.MkdirAll(betaHome, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := logging.SetupLogging(ctx, logging.Options{Home: defaultHome, Mode: "gateway", Force: true}); err != nil {
		t.Fatal(err)
	}
	cfg := &gateway.GatewayConfig{}
	cfg.SetRuntimeProfileHomes([]profiles.ServedProfile{{Name: "default", Home: defaultHome}, {Name: "beta", Home: betaHome}})
	cfg.MultiplexProfiles = boolRef(false)
	if enableMultiplexLogRouting(ctx, cfg) {
		t.Fatal("a single-profile gateway must leave the handlers static")
	}
	cfg.MultiplexProfiles = boolRef(true)
	if !enableMultiplexLogRouting(ctx, cfg) {
		t.Fatal("a multiplex gateway must enable the profile routers")
	}
	logging.GetLogger("gateway.run").LogContext(home.WithOverride(ctx, betaHome), logging.LevelWarning, "BETA-GATEWAY-WARN")
	logging.GetLogger("gateway.run").LogContext(home.WithOverride(ctx, defaultHome), logging.LevelInfo, "DEFAULT-GATEWAY-INFO")
	logging.FlushLogQueue()
	for _, filename := range []string{"agent.log", "errors.log", "gateway.log"} {
		if !logContains(betaHome, filename, "BETA-GATEWAY-WARN") || logContains(defaultHome, filename, "BETA-GATEWAY-WARN") {
			t.Fatal(filename)
		}
	}
	if !logContains(defaultHome, "gateway.log", "DEFAULT-GATEWAY-INFO") || logContains(betaHome, "gateway.log", "DEFAULT-GATEWAY-INFO") {
		t.Fatal("the default profile's record must land only in its own gateway.log")
	}
}
