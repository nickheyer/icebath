package gatewaycli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
)

// Slot state the way s6-svstat reports it, plus what stop and start do to it
type fakeS6Slots struct {
	scandir string
	up      map[string]bool
	calls   [][2]string
}

func (f *fakeS6Slots) IsRunning(name string) (bool, error) { return f.up[name], nil }
func (f *fakeS6Slots) Stop(_ context.Context, name string) error {
	f.calls = append(f.calls, [2]string{"stop", name})
	delete(f.up, name)
	return nil
}
func (f *fakeS6Slots) Start(name string) error {
	f.calls = append(f.calls, [2]string{"start", name})
	f.up[name] = true
	return nil
}
func (f *fakeS6Slots) Restart(name string) error {
	f.calls = append(f.calls, [2]string{"restart", name})
	f.up[name] = true
	return nil
}
func (f *fakeS6Slots) SlotScandir() string { return f.scandir }

// s6 container host with alpha and beta parked down by the boot and the multiplex default unset
func newS6Host(t *testing.T) (string, *fakeS6Slots) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "data")
	for _, sub := range []string{"profiles/alpha", "profiles/beta"} {
		mkdirs(t, filepath.Join(root, sub))
		os.WriteFile(filepath.Join(root, sub, "SOUL.md"), []byte("x"), 0o644)
	}
	os.WriteFile(filepath.Join(root, "config.yaml"), []byte("# my config\nmodel:\n  default: x\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".env"), []byte("TELEGRAM_BOT_TOKEN=111111:default-token\n"), 0o644)
	os.WriteFile(filepath.Join(root, "profiles/alpha/.env"), []byte("TELEGRAM_BOT_TOKEN=222222:alpha-token\n"), 0o644)
	os.WriteFile(filepath.Join(root, "profiles/beta/.env"), []byte("DISCORD_BOT_TOKEN=beta-discord-333333\n"), 0o644)
	t.Setenv("ICEBATH_HOME", root)
	for _, name := range []string{"GATEWAY_MULTIPLEX_PROFILES", "TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "API_SERVER_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	scandir := filepath.Join(tmp, "run-service")
	fake := &fakeS6Slots{scandir: scandir, up: map[string]bool{}}
	for _, slot := range []string{"gateway-default", "gateway-alpha", "gateway-beta"} {
		mkdirs(t, filepath.Join(scandir, slot))
	}
	saved, savedFor, savedWait := gatewayrun.MigrateS6, s6SlotManagerFor, s6WaitDown
	t.Cleanup(func() { gatewayrun.MigrateS6, s6SlotManagerFor, s6WaitDown = saved, savedFor, savedWait })
	InstallMigrateS6Hooks()
	gatewayrun.MigrateS6.Running = func() bool { return true }
	gatewayrun.MigrateS6.Scandir = func() string { return scandir }
	s6SlotManagerFor = func() s6SlotManager { return fake }
	s6WaitDown = func(string, int) {}
	return root, fake
}

func TestParkedNamedSlotsDoNotVetoTheUnsetDefault(t *testing.T) {
	_, fake := newS6Host(t)
	ctx := context.Background()
	resolve := func() gateway.MultiplexDecision {
		cfg, err := gateway.GatewayConfigFromDict(ctx, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		decision, err := gatewayrun.ResolveMultiplexMode(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}
	if decision := resolve(); !decision.Enabled || decision.Source != "default" {
		t.Fatalf("decision %+v", decision)
	}
	fake.up["gateway-alpha"] = true
	if blocked := resolve(); blocked.Source != "guard" || !strings.Contains(blocked.Reason, "alpha") {
		t.Fatalf("blocked %+v", blocked)
	}
}
