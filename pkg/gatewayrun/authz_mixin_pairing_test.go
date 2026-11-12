package gatewayrun

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Bare runner whose pairing store holds the user approved or not
func pairingUnionRunner(t *testing.T, paired bool, userID string) *GatewayRunner {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, name := range []string{"TELEGRAM_ALLOWED_USERS", "TELEGRAM_ALLOW_ALL_USERS", "TELEGRAM_GROUP_ALLOWED_USERS",
		"TELEGRAM_GROUP_ALLOWED_CHATS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"} {
		t.Setenv(name, "")
	}
	registerGuardPlatformEntries(t)
	ctx := context.Background()
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if paired {
		code, ok, err := store.GenerateCode(ctx, "telegram", userID, "SomeHuman")
		if err != nil || !ok {
			t.Fatalf("generate code = %v %v", ok, err)
		}
		if _, err := store.ApproveCode(ctx, "telegram", code); err != nil {
			t.Fatal(err)
		}
	}
	r := allocGatewayRunner()
	r.pairingStore = store
	return r
}

// Telegram DM source of a human
func pairingSource(userID string) *gateway.SessionSource {
	name := "SomeHuman"
	return &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "123", ChatType: "dm", UserID: &userID, UserName: &name}
}

func TestPairedUserAuthorizedEvenWhenNotInAllowlist(t *testing.T) {
	r := pairingUnionRunner(t, true, "pairme")
	t.Setenv("TELEGRAM_ALLOWED_USERS", "owner1,owner2")
	if !authorized(t, r, pairingSource("pairme")) {
		t.Fatal("a paired user must be authorized alongside the allowlist")
	}
}

// WhatsApp host of the behaviour mixin, its own access policy winning over the base adapter as in the Python MRO
type liveWhatsAppAdapter struct {
	*gateway.BasePlatformAdapter
	*whatsapp.WhatsAppBehaviorMixin
}

func (a *liveWhatsAppAdapter) Name() string { return a.BasePlatformAdapter.Name() }
func (a *liveWhatsAppAdapter) EnforcesOwnAccessPolicy() bool {
	return a.WhatsAppBehaviorMixin.EnforcesOwnAccessPolicy()
}
func (a *liveWhatsAppAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *liveWhatsAppAdapter) Disconnect(context.Context) error            { return nil }
func (a *liveWhatsAppAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: true}, nil
}
func (a *liveWhatsAppAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

func TestRevokeWhatsappSoleEntryDeniesLiveAdapterWithoutRestart(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, name := range []string{"WHATSAPP_ALLOW_ALL_USERS", "WHATSAPP_GROUP_ALLOWED_USERS", "WHATSAPP_GROUP_ALLOWED_CHATS",
		"GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"} {
		t.Setenv(name, "")
	}
	t.Setenv("WHATSAPP_ALLOWED_USERS", "15551234567")
	ctx := context.Background()
	entry := &gateway.PlatformEntry{Name: "whatsapp", Label: "WhatsApp", AllowedUsersEnv: "WHATSAPP_ALLOWED_USERS", AllowAllEnv: "WHATSAPP_ALLOW_ALL_USERS"}
	if err := gateway.Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, "whatsapp", "") })

	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	cfg.Extra = map[string]any{"dm_policy": "allowlist", "allow_from": []any{"15551234567"}}
	adapter := &liveWhatsAppAdapter{WhatsAppBehaviorMixin: whatsapp.NewWhatsAppBehaviorMixin("LiveWhatsAppAdapter", cfg)}
	adapter.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformWhatsApp, adapter)
	if _, err := adapter.SelectDMAllowlist(cfg.Extra, []string{"WHATSAPP_ALLOWED_USERS"}, func(string) (string, error) { return "", nil }); err != nil {
		t.Fatal(err)
	}
	adapter.DMPolicy, adapter.GroupPolicy = "allowlist", "pairing"
	adapter.AllowFrom = map[string]bool{"15551234567": true}
	adapter.GroupAllowFrom = map[string]bool{}

	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	r := allocGatewayRunner()
	waCfg := gateway.NewPlatformConfig()
	waCfg.Enabled = true
	waCfg.Extra = map[string]any{"dm_policy": "allowlist", "allow_from": []any{"15551234567"}}
	r.config = &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformWhatsApp: waCfg}}
	r.adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	r.adapters.Set(gateway.PlatformWhatsApp, adapter)
	r.pairingStore = store
	gateway.SetGatewayRunnerRef(r)
	t.Cleanup(func() { gateway.SetGatewayRunnerRef(nil) })

	code, ok, err := store.GenerateCode(ctx, "whatsapp", "15551234567@s.whatsapp.net", "")
	if err != nil || !ok {
		t.Fatalf("generate code = %v %v", ok, err)
	}
	if _, err := store.ApproveCode(ctx, "whatsapp", code); err != nil {
		t.Fatal(err)
	}
	sender := "15551234567:47@s.whatsapp.net"
	name := "revoked"
	source := &gateway.SessionSource{Platform: gateway.PlatformWhatsApp, UserID: &sender, ChatID: sender, UserName: &name, ChatType: "dm"}
	if intake, err := adapter.IsDMIntakeAllowed(ctx, sender); err != nil || !intake {
		t.Fatalf("intake before revoke = %v %v", intake, err)
	}
	if !authorized(t, r, source) {
		t.Fatal("the paired sender must be authorized before the revoke")
	}

	if revoked, err := store.Revoke(ctx, "whatsapp", sender); err != nil || !revoked {
		t.Fatalf("revoke = %v %v", revoked, err)
	}
	if approved, err := store.IsApproved(ctx, "whatsapp", "15551234567@s.whatsapp.net"); err != nil || approved {
		t.Fatalf("is approved after revoke = %v %v", approved, err)
	}
	if value := os.Getenv("WHATSAPP_ALLOWED_USERS"); value != "" {
		t.Fatalf("WHATSAPP_ALLOWED_USERS = %q", value)
	}
	if adapter.AllowFromSnapshot()["15551234567"] {
		t.Fatal("the revoked number stays in the live adapter allowlist")
	}
	if intake, err := adapter.IsDMIntakeAllowed(ctx, sender); err != nil || intake {
		t.Fatalf("intake after revoke = %v %v", intake, err)
	}
	if allowed, err := adapter.IsDMAllowed(ctx, sender); err != nil || allowed {
		t.Fatalf("dm allowed after revoke = %v %v", allowed, err)
	}
	if authorized(t, r, source) {
		t.Fatal("the revoked sender is still authorized by the runner")
	}
}

func TestUnpairedUserInAllowlistStillAuthorized(t *testing.T) {
	r := pairingUnionRunner(t, false, "")
	t.Setenv("TELEGRAM_ALLOWED_USERS", "owner1")
	if !authorized(t, r, pairingSource("owner1")) {
		t.Fatal("an allowlisted user must stay authorized")
	}
}
