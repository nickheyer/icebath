package gatewayrun

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// Discord adapter stand in exposing what the voice callback reads, counting dispatched turns
type voiceBot struct {
	*gateway.BasePlatformAdapter
	mu       sync.Mutex
	channels map[int64]int64
	handled  int
}

func (b *voiceBot) VoiceTextChannel(guildID int64) (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.channels[guildID]
	return id, ok
}

func (b *voiceBot) Connect(context.Context, bool) (bool, error) { return true, nil }
func (b *voiceBot) Disconnect(context.Context) error            { return nil }
func (b *voiceBot) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: true}, nil
}
func (b *voiceBot) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (b *voiceBot) GuildMemberDisplayName(int64, int64) any          { return nil }
func (b *voiceBot) CachedUserDisplayName(int64) any                  { return nil }
func (b *voiceBot) CachedGuild(int64) any                            { return nil }
func (b *voiceBot) CachedChannel(int64) any                          { return nil }
func (b *voiceBot) SendToChannel(context.Context, any, string) error { return nil }
func (b *voiceBot) VoiceSource(int64) (map[string]any, bool)         { return nil, false }
func (b *voiceBot) HandleMessage(context.Context, *gateway.MessageEvent) error {
	b.mu.Lock()
	b.handled++
	b.mu.Unlock()
	return nil
}

// Voice bot of one profile bound to text channel 700 in guild 1
func newVoiceBot(profile string) *voiceBot {
	b := &voiceBot{channels: map[int64]int64{1: 700}}
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	b.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformDiscord, b)
	b.SetOwnerProfile(profile)
	return b
}

// test_voice_transcript_dedup_scope.py test_dedup_is_scoped_to_the_receiving_bot_and_its_binding
func TestDedupIsScopedToTheReceivingBotAndItsBinding(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	ctx := context.Background()
	botA, botB := newVoiceBot(""), newVoiceBot("bot-b")
	r := allocGatewayRunner()
	r.adapters = adapterDict(gateway.PlatformDiscord, gateway.PlatformAdapter(botA))
	say := func(bot *voiceBot) {
		if err := r.handleVoiceChannelInput(ctx, 1, 42, "what broke on the ingest box", bot); err != nil {
			t.Fatal(err)
		}
	}
	say(botA)
	say(botB)
	say(botA)
	if botA.handled != 1 || botB.handled != 1 {
		t.Fatalf("handled = (%d, %d), want (1, 1)", botA.handled, botB.handled)
	}
	botA.channels[1] = 800
	say(botA)
	if botA.handled != 2 {
		t.Fatalf("handled after rebind = %d, want 2", botA.handled)
	}
}
