package gatewayrun

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	guardGroupChat  = "-1001234567890"
	guardOtherGroup = "-1009876543210"
	guardBotA       = "111111111"
	guardBotB       = "222222222"
	guardBotC       = "333333333"
	guardHuman      = "100200300"
)

// Mutable guard clock of the hermes clock fixture
type guardClock struct{ t float64 }

func (c *guardClock) now() float64         { return c.t }
func (c *guardClock) advance(secs float64) { c.t += secs }

// Bare runner with an empty pairing store and a guard on the test clock and settings holder
func guardRunner(t *testing.T, clock *guardClock, settings *gateway.BotLoopGuardSettings) *GatewayRunner {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, name := range []string{"TELEGRAM_ALLOW_BOTS", "TELEGRAM_ALLOWED_USERS", "TELEGRAM_ALLOW_ALL_USERS", "TELEGRAM_GROUP_ALLOWED_USERS",
		"TELEGRAM_GROUP_ALLOWED_CHATS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"} {
		t.Setenv(name, "")
	}
	registerGuardPlatformEntries(t)
	r := allocGatewayRunner()
	store, err := gateway.NewPairingStore(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	r.pairingStore = store
	r.botLoopGuard = gateway.NewBotLoopGuard(func(context.Context) gateway.BotLoopGuardSettings { return *settings }, clock.now)
	return r
}

// Telegram and Discord entries as their bundled plugins register them on discovery
func registerGuardPlatformEntries(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	entries := []*gateway.PlatformEntry{
		{Name: "telegram", Label: "Telegram", AllowedUsersEnv: "TELEGRAM_ALLOWED_USERS", AllowAllEnv: "TELEGRAM_ALLOW_ALL_USERS",
			AllowBotsEnv: "TELEGRAM_ALLOW_BOTS", GroupAllowedChatsEnv: "TELEGRAM_GROUP_ALLOWED_CHATS", AllowlistSuppressesPairing: true},
		{Name: "discord", Label: "Discord", AllowedUsersEnv: "DISCORD_ALLOWED_USERS", AllowAllEnv: "DISCORD_ALLOW_ALL_USERS",
			AllowBotsEnv: "DISCORD_ALLOW_BOTS", AllowlistSuppressesPairing: true},
	}
	for _, entry := range entries {
		if err := gateway.Registry.Register(ctx, entry, ""); err != nil {
			t.Fatal(err)
		}
		name := entry.Name
		t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, name, "") })
	}
}

// Default guard fixture settings of a 20 message budget per 60 seconds
func guardSettings() *gateway.BotLoopGuardSettings {
	return &gateway.BotLoopGuardSettings{Enabled: true, MaxEvents: 20, WindowSeconds: 60, CooldownSeconds: 60}
}

// Bot authored Telegram source
func guardBot(userID, chatID, chatType string) *gateway.SessionSource {
	name := "Bot" + userID
	return &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: chatID, ChatType: chatType, UserID: &userID, UserName: &name, IsBot: true}
}

// Human authored Telegram source
func guardHumanSource(chatID, chatType string) *gateway.SessionSource {
	userID, name := guardHuman, "Alice"
	return &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: chatID, ChatType: chatType, UserID: &userID, UserName: &name}
}

// ALLOW_BOTS on, a human allowlist and the group chat allowlist that short circuits authorization
func incidentConfig(t *testing.T) {
	t.Setenv("TELEGRAM_ALLOW_BOTS", "mentions")
	t.Setenv("TELEGRAM_ALLOWED_USERS", guardHuman)
	t.Setenv("TELEGRAM_GROUP_ALLOWED_CHATS", guardGroupChat+","+guardOtherGroup)
}

// Verdict and one count for an admitted bot, what the ingress gate does per message
func guardInbound(t *testing.T, r *GatewayRunner, source *gateway.SessionSource) bool {
	t.Helper()
	ctx := context.Background()
	authorized, err := r.isUserAuthorized(ctx, source, true)
	if err != nil {
		t.Fatal(err)
	}
	return authorized && r.admitBotMessage(ctx, source)
}

// Alternating two bot exchange in one chat
func guardPingPong(t *testing.T, r *GatewayRunner, turns int, chatID string) []bool {
	out := make([]bool, 0, turns)
	for turn := range turns {
		bot := guardBotA
		if turn%2 == 1 {
			bot = guardBotB
		}
		out = append(out, guardInbound(t, r, guardBot(bot, chatID, "group")))
	}
	return out
}

// Authorization verdict failing the test on error
func authorized(t *testing.T, r *GatewayRunner, source *gateway.SessionSource) bool {
	t.Helper()
	ok, err := r.isUserAuthorized(context.Background(), source, true)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestOneInboundIsCountedOnceHoweverOftenTheVerdictIsAsked(t *testing.T) {
	r := guardRunner(t, &guardClock{1_000_000}, guardSettings())
	incidentConfig(t)
	for range 20 {
		bot := guardBot(guardBotA, guardGroupChat, "group")
		for range 3 {
			if !authorized(t, r, bot) {
				t.Fatal("verdict asked again must stay true")
			}
		}
		if !r.admitBotMessage(context.Background(), bot) {
			t.Fatal("admit within the budget")
		}
	}
	if !authorized(t, r, guardBot(guardBotB, guardGroupChat, "group")) {
		t.Fatal("budget not yet tripped")
	}
	if r.admitBotMessage(context.Background(), guardBot(guardBotB, guardGroupChat, "group")) {
		t.Fatal("21st bot message must trip the guard")
	}
	if authorized(t, r, guardBot(guardBotB, guardGroupChat, "group")) {
		t.Fatal("chat in cooldown must refuse bots")
	}
}

func TestAdmittedBotTrafficIsCutAtTheBudget(t *testing.T) {
	cases := []struct {
		name           string
		senders        []string
		chatID         string
		chatType       string
		groupAllowlist bool
	}{
		{"ping-pong of 40 turns", []string{guardBotA, guardBotB}, guardGroupChat, "group", true},
		{"three bots share one budget", []string{guardBotA, guardBotB, guardBotC}, guardGroupChat, "group", true},
		{"plain ALLOW_BOTS dm without a chat allowlist", []string{guardBotA}, "123", "dm", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := guardRunner(t, &guardClock{1_000_000}, guardSettings())
			incidentConfig(t)
			if !tc.groupAllowlist {
				t.Setenv("TELEGRAM_GROUP_ALLOWED_CHATS", "")
			}
			for i := range 40 {
				verdict := guardInbound(t, r, guardBot(tc.senders[i%len(tc.senders)], tc.chatID, tc.chatType))
				if verdict != (i < 20) {
					t.Fatalf("verdict %d = %v", i, verdict)
				}
			}
		})
	}
}

func TestHumansAreNeverMeteredAndStayAuthorizedDuringCooldown(t *testing.T) {
	clock := &guardClock{1_000_000}
	r := guardRunner(t, clock, guardSettings())
	incidentConfig(t)
	for range 50 {
		if !guardInbound(t, r, guardHumanSource("123", "dm")) {
			t.Fatal("human must stay admitted")
		}
	}
	if n := r.botLoopGuard.TrackedConversations(); n != 0 {
		t.Fatalf("tracked = %d", n)
	}
	guardPingPong(t, r, 25, guardGroupChat)
	if authorized(t, r, guardBot(guardBotA, guardGroupChat, "group")) {
		t.Fatal("bot must be refused in cooldown")
	}
	if !authorized(t, r, guardHumanSource(guardGroupChat, "group")) {
		t.Fatal("human must stay authorized")
	}
	clock.advance(5)
	if !authorized(t, r, guardHumanSource(guardGroupChat, "group")) {
		t.Fatal("human must stay authorized")
	}
}

func TestBudgetIsScopedByChatAndPlatform(t *testing.T) {
	r := guardRunner(t, &guardClock{1_000_000}, guardSettings())
	incidentConfig(t)
	guardPingPong(t, r, 25, guardGroupChat)
	if authorized(t, r, guardBot(guardBotA, guardGroupChat, "group")) {
		t.Fatal("tripped chat must refuse")
	}
	if !guardInbound(t, r, guardBot(guardBotA, guardOtherGroup, "group")) || !guardInbound(t, r, guardBot(guardBotB, guardOtherGroup, "group")) {
		t.Fatal("other chat keeps its own budget")
	}
	t.Setenv("DISCORD_ALLOW_BOTS", "all")
	userID := guardBotA
	discordBot := &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatID: guardGroupChat, ChatType: "group", UserID: &userID, IsBot: true}
	if !guardInbound(t, r, discordBot) {
		t.Fatal("other platform keeps its own budget")
	}
}

func TestCooldownExpiryReadmitsWithAFreshBudget(t *testing.T) {
	clock := &guardClock{1_000_000}
	r := guardRunner(t, clock, guardSettings())
	incidentConfig(t)
	guardPingPong(t, r, 21, guardGroupChat)
	if authorized(t, r, guardBot(guardBotA, guardGroupChat, "group")) {
		t.Fatal("tripped chat must refuse")
	}
	clock.advance(61)
	for i, ok := range guardPingPong(t, r, 20, guardGroupChat) {
		if !ok {
			t.Fatalf("fresh budget refused turn %d", i)
		}
	}
	if guardInbound(t, r, guardBot(guardBotA, guardGroupChat, "group")) {
		t.Fatal("21st message of the fresh budget must trip")
	}
}

func TestDisabledViaConfigAdmitsEverything(t *testing.T) {
	settings := guardSettings()
	r := guardRunner(t, &guardClock{1_000_000}, settings)
	incidentConfig(t)
	*settings = gateway.BotLoopGuardSettingsFromConfig(map[string]any{"gateway": map[string]any{"bot_loop_guard": map[string]any{"enabled": false}}})
	for i, ok := range guardPingPong(t, r, 40, guardGroupChat) {
		if !ok {
			t.Fatalf("disabled guard refused turn %d", i)
		}
	}
}

func TestRejectedBotMessagesDoNotConsumeBudget(t *testing.T) {
	r := guardRunner(t, &guardClock{1_000_000}, guardSettings())
	t.Setenv("TELEGRAM_ALLOWED_USERS", guardHuman)
	for range 30 {
		if guardInbound(t, r, guardBot(guardBotA, "123", "dm")) {
			t.Fatal("unauthorized bot admitted")
		}
	}
	if n := r.botLoopGuard.TrackedConversations(); n != 0 {
		t.Fatalf("tracked = %d", n)
	}
}
