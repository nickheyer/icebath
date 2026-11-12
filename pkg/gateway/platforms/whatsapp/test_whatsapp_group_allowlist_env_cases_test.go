package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestGroupAllowlistEnvEnvOnlyGroupAllowlistGatesIntakeThroughTheSecretScope(t *testing.T) {
	hermeticHome(t)
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, ".env"), []byte("WHATSAPP_GROUP_ALLOWED_USERS=120363001234567890@g.us\nWHATSAPP_GROUP_POLICY=allowlist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("WHATSAPP_GROUP_ALLOWED_USERS")
	os.Unsetenv("WHATSAPP_GROUP_ALLOW_FROM")
	provider.SetMultiplexActive(true)
	ctx := provider.WithSecretScope(context.Background(), provider.BuildProfileSecretScope(profile), profile)
	a, err := NewWhatsAppAdapter(ctx, &gateway.PlatformConfig{Extra: map[string]any{}})
	provider.SetMultiplexActive(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.GroupAllowFrom) != 1 || !a.GroupAllowFrom["120363001234567890@g.us"] {
		t.Fatalf("group allowlist = %v", a.GroupAllowFrom)
	}
	if ok, _ := a.IsGroupAllowed(context.Background(), "120363001234567890@g.us", ""); !ok {
		t.Fatal("listed group rejected")
	}
	if ok, _ := a.IsGroupAllowed(context.Background(), "99999999999@g.us", ""); ok {
		t.Fatal("unlisted group accepted")
	}
}

func TestGroupAllowlistEnvConfigGroupAllowFromWinsOverEnv(t *testing.T) {
	hermeticHome(t)
	t.Setenv("WHATSAPP_GROUP_ALLOWED_USERS", "1111111111@g.us")
	a, err := NewWhatsAppAdapter(context.Background(), &gateway.PlatformConfig{Extra: map[string]any{"group_allow_from": []any{"120363001234567890@g.us"}}})
	if err != nil || len(a.GroupAllowFrom) != 1 || !a.GroupAllowFrom["120363001234567890@g.us"] {
		t.Fatalf("group allowlist = %v %v", a.GroupAllowFrom, err)
	}
	b, err := NewWhatsAppAdapter(context.Background(), &gateway.PlatformConfig{Extra: map[string]any{"groupAllowFrom": []any{}}})
	if err != nil || len(b.GroupAllowFrom) != 0 {
		t.Fatalf("group allowlist = %v %v", b.GroupAllowFrom, err)
	}
}
