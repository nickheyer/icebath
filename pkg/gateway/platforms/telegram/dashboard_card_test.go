package telegram

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestTelegramDashboardCardRules(t *testing.T) {
	var e gateway.PlatformEntry
	applyDashboardCard(&e)
	if e.DashboardDescription != "Run Icebath from Telegram DMs, groups, and topics." || len(e.DashboardEnvVars) != 3 {
		t.Fatalf("card %+v", e)
	}
	for _, c := range []struct {
		env, value string
		ok         bool
	}{
		{"TELEGRAM_BOT_TOKEN", "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef", true},
		{"TELEGRAM_BOT_TOKEN", "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ_1234", true},
		{"TELEGRAM_BOT_TOKEN", "123456789:short", false},
		{"TELEGRAM_BOT_TOKEN", " 123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef", false},
		{"TELEGRAM_ALLOWED_USERS", "123, 456 ,", true}, {"TELEGRAM_ALLOWED_USERS", "١٢٣", true},
		{"TELEGRAM_ALLOWED_USERS", "123,@alice", false}, {"TELEGRAM_ALLOWED_USERS", "-100", false},
	} {
		if got := e.EnvValueRules[c.env].Check(c.value); got != c.ok {
			t.Fatalf("%s=%q -> %v", c.env, c.value, got)
		}
	}
	if e.EnvValueRules["TELEGRAM_BOT_TOKEN"].Message != "Telegram bot token must be the complete token from @BotFather, such as 123456789:ABC…" {
		t.Fatal(e.EnvValueRules["TELEGRAM_BOT_TOKEN"].Message)
	}
}
