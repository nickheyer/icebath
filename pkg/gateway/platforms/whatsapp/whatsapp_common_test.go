package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Context under multiplexing with the secret scope built from the profile .env text
func profileScope(t *testing.T, dotenv string) context.Context {
	t.Helper()
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0o644); err != nil {
		t.Fatal(err)
	}
	return provider.WithSecretScope(context.Background(), provider.BuildProfileSecretScope(dir), dir)
}

func TestEffectiveReplyPrefixUsesScope(t *testing.T) {
	t.Setenv("WHATSAPP_MODE", "")
	os.Unsetenv("WHATSAPP_MODE")
	t.Setenv("WHATSAPP_REPLY_PREFIX", "")
	os.Unsetenv("WHATSAPP_REPLY_PREFIX")
	ctx := profileScope(t, "WHATSAPP_MODE=bot\n")
	mixin := NewWhatsAppBehaviorMixin("test", &gateway.PlatformConfig{Extra: map[string]any{}})
	if prefix, err := mixin.EffectiveReplyPrefix(ctx); err != nil || prefix != "" {
		t.Fatalf("prefix %q err %v", prefix, err)
	}
}

func TestWhatsappRequireMentionUsesScope(t *testing.T) {
	t.Setenv("WHATSAPP_REQUIRE_MENTION", "")
	os.Unsetenv("WHATSAPP_REQUIRE_MENTION")
	ctx := profileScope(t, "WHATSAPP_REQUIRE_MENTION=true\n")
	mixin := NewWhatsAppBehaviorMixin("test", &gateway.PlatformConfig{Extra: map[string]any{}})
	if required, err := mixin.WhatsAppRequireMention(ctx); err != nil || !required {
		t.Fatalf("required %v err %v", required, err)
	}
}
