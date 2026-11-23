package whatsapp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

func TestGroupAllowlistOfTheProfileInScopeGatesIntake(t *testing.T) {
	hermeticHome(t)
	profile := t.TempDir()
	for key, value := range map[string]any{settings.WhatsappGroupAllowFrom.Key: []any{"120363001234567890@g.us"}, settings.WhatsappGroupPolicy.Key: "allowlist"} {
		if _, err := settings.Set(home.WithOverride(context.Background(), profile), key, value, settings.Global); err != nil {
			t.Fatal(err)
		}
	}
	ctx := provider.WithSecretScope(context.Background(), provider.BuildProfileSecretScope(profile), profile)
	a, err := NewWhatsAppAdapter(ctx, &gateway.PlatformConfig{Extra: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if a.GroupPolicy != "allowlist" || len(a.GroupAllowFrom) != 1 || !a.GroupAllowFrom["120363001234567890@g.us"] {
		t.Fatalf("group policy %q allowlist = %v", a.GroupPolicy, a.GroupAllowFrom)
	}
	if ok, _ := a.IsGroupAllowed(context.Background(), "120363001234567890@g.us", ""); !ok {
		t.Fatal("listed group rejected")
	}
	if ok, _ := a.IsGroupAllowed(context.Background(), "99999999999@g.us", ""); ok {
		t.Fatal("unlisted group accepted")
	}
	b, err := NewWhatsAppAdapter(context.Background(), &gateway.PlatformConfig{Extra: map[string]any{}})
	if err != nil || b.GroupPolicy != "pairing" || len(b.GroupAllowFrom) != 0 {
		t.Fatalf("the default profile borrowed another profile's group access: %q %v %v", b.GroupPolicy, b.GroupAllowFrom, err)
	}
}
