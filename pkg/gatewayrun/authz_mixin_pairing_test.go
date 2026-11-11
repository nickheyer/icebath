package gatewayrun

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
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

func TestUnpairedUserInAllowlistStillAuthorized(t *testing.T) {
	r := pairingUnionRunner(t, false, "")
	t.Setenv("TELEGRAM_ALLOWED_USERS", "owner1")
	if !authorized(t, r, pairingSource("owner1")) {
		t.Fatal("an allowlisted user must stay authorized")
	}
}
