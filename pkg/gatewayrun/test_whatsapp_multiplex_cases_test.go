package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestCredentialsFollowProfileHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "default"))
	a, b := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	for _, h := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(h, "platforms/whatsapp/session"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "platforms/whatsapp/session/creds.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		home     string
		expected bool
	}{{a, true}, {b, false}, {a, true}} {
		ctx := gateway.ProfileRuntimeScope(context.Background(), step.home, false)
		if got := platformHasBotCredential(ctx, gateway.PlatformWhatsApp, &gateway.PlatformConfig{Extra: map[string]any{}}); got != step.expected {
			t.Fatalf("home %s credential %v want %v", step.home, got, step.expected)
		}
		overridden := &gateway.PlatformConfig{Extra: map[string]any{"session_path": filepath.Join(a, "platforms/whatsapp/session")}}
		if !platformHasBotCredential(ctx, gateway.PlatformWhatsApp, overridden) {
			t.Fatalf("home %s ignored the session_path override", step.home)
		}
	}
	update := gateway.RuntimeStatusUpdate{Fields: map[string]any{"platform": "work:whatsapp", "platform_state": "disabled",
		"error_code": "whatsapp_unpaired", "error_message": "pair it: icebath -p work whatsapp"}}
	if _, err := gateway.WriteRuntimeStatus(context.Background(), update, false, nil); err != nil {
		t.Fatal(err)
	}
	saved := gateway.LiveDefaultGatewayPid
	t.Cleanup(func() { gateway.LiveDefaultGatewayPid = saved })
	gateway.LiveDefaultGatewayPid = func() (int, bool, error) { return os.Getpid(), true, nil }
	unserved, err := gateway.ServedProfileUnservedPlatforms("work")
	if err != nil {
		t.Fatal(err)
	}
	if unserved.Len() != 1 || unserved.GetOr("whatsapp", nil) != "pair it: icebath -p work whatsapp" {
		t.Fatalf("unserved %v", unserved)
	}
}
