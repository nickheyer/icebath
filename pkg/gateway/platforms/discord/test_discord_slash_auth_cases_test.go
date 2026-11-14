package discord

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Slash interaction from a user in a guild channel
func makeInteraction(userID, channelID string) *discordgo.Interaction {
	return &discordgo.Interaction{ID: "1", AppID: "999", Token: "tok", Type: discordgo.InteractionApplicationCommand, ChannelID: channelID, GuildID: "g1",
		Member: &discordgo.Member{User: &discordgo.User{ID: userID, Username: "user" + userID}}}
}

// Alert target adapter recording the sends it receives
type alertAdapter struct {
	*gateway.BasePlatformAdapter
	mu     sync.Mutex
	sends  [][2]string
	result gateway.SendResult
}

func newAlertAdapter(p gateway.Platform, result gateway.SendResult) *alertAdapter {
	a := &alertAdapter{result: result}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), p, a)
	return a
}

func (a *alertAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *alertAdapter) Disconnect(context.Context) error            { return nil }
func (a *alertAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (a *alertAdapter) Send(_ context.Context, chatID, content, _ string, _ map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	a.sends = append(a.sends, [2]string{chatID, content})
	a.mu.Unlock()
	return a.result, nil
}

func (a *alertAdapter) sent() [][2]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][2]string(nil), a.sends...)
}

// Runner seam answering with the given adapters and home channels
func installAlertRunner(t *testing.T, a *DiscordAdapter, adapters map[gateway.Platform]gateway.PlatformAdapter, homes map[gateway.Platform]string) {
	m := pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	for p, ad := range adapters {
		m.Set(p, ad)
	}
	cfg := &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}
	for p, chat := range homes {
		pc := gateway.NewPlatformConfig()
		pc.HomeChannel = &gateway.HomeChannel{Platform: p, ChatID: chat, Name: "Home"}
		cfg.Platforms[p] = pc
	}
	orig := gateway.ProfileAdaptersAndConfig
	gateway.ProfileAdaptersAndConfig = func(context.Context, any, string) (*gateway.AdapterMap, *gateway.GatewayConfig, error) {
		return m, cfg, nil
	}
	t.Cleanup(func() { gateway.ProfileAdaptersAndConfig = orig })
	a.GatewayRunner = struct{}{}
}

func waitSends(fa *alertAdapter, n int) [][2]string {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(fa.sent()) < n {
		time.Sleep(10 * time.Millisecond)
	}
	return fa.sent()
}

func TestChannelNotInAllowlistRejected(t *testing.T) {
	a, _, _ := inboundAdapter(t)
	t.Setenv("DISCORD_ALLOWED_CHANNELS", "1111,2222")
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if a.checkSlashAuthorization(t.Context(), makeInteraction("100200300", "9999"), "/bg hi") {
		t.Fatal("channel outside the allowlist must be rejected")
	}
	if !strings.Contains(logs.String(), "DISCORD_ALLOWED_CHANNELS") {
		t.Fatalf("log %q", logs.String())
	}
}

func TestUnauthorizedAttemptNotifiesTelegram(t *testing.T) {
	a, _, _ := inboundAdapter(t)
	tg := newAlertAdapter(gateway.PlatformTelegram, gateway.SendResult{Success: true})
	installAlertRunner(t, a, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformTelegram: tg}, map[gateway.Platform]string{gateway.PlatformTelegram: "987654321"})
	a.allowedUserIDs = newStrSet("100200300")
	a.checkSlashAuthorization(t.Context(), makeInteraction("999999999", "555"), "/bg hi")
	sent := waitSends(tg, 1)
	if len(sent) != 1 || sent[0][0] != "987654321" {
		t.Fatalf("sends %v", sent)
	}
	for _, want := range []string{"Unauthorized", "999999999", "/bg hi", "DISCORD_ALLOWED_USERS"} {
		if !strings.Contains(sent[0][1], want) {
			t.Fatalf("missing %q in %q", want, sent[0][1])
		}
	}
}

func TestNotifyFallsBackToSlackOnTelegramSoftFail(t *testing.T) {
	a, _, _ := inboundAdapter(t)
	tg := newAlertAdapter(gateway.PlatformTelegram, gateway.SendResult{Success: false, Error: "rate limited"})
	sl := newAlertAdapter(gateway.PlatformSlack, gateway.SendResult{Success: true})
	installAlertRunner(t, a, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformTelegram: tg, gateway.PlatformSlack: sl},
		map[gateway.Platform]string{gateway.PlatformTelegram: "1", gateway.PlatformSlack: "C12345"})
	a.allowedUserIDs = newStrSet("100200300")
	a.checkSlashAuthorization(t.Context(), makeInteraction("999999999", "555"), "/bg hi")
	if sent := waitSends(sl, 1); len(sent) != 1 || sent[0][0] != "C12345" {
		t.Fatalf("slack sends %v", sent)
	}
	if len(tg.sent()) != 1 {
		t.Fatal("telegram must be tried first")
	}
}
