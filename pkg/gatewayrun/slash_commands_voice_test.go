package gatewayrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Voice channel of a fixed name
type namedVoiceChannel string

func (c namedVoiceChannel) Name() string { return string(c) }

// Discord voice adapter recording joins, leaves and its voice bindings
type fakeVoiceAdapter struct {
	*gateway.BasePlatformAdapter
	channel       voiceChannelRef
	joinOK        bool
	joinErr       error
	leaveErr      error
	inVoice       bool
	leaves        []int64
	callback      func(ctx context.Context, guildID, userID int64, transcript string) error
	textChannels  map[int64]int64
	voiceSources  map[int64]*pyjson.Object
	callbackWired bool
}

// Discord voice adapter on the platform
func newFakeVoiceAdapter(platform gateway.Platform) *fakeVoiceAdapter {
	a := &fakeVoiceAdapter{textChannels: map[int64]int64{}, voiceSources: map[int64]*pyjson.Object{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), platform, a)
	return a
}

func (a *fakeVoiceAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *fakeVoiceAdapter) Disconnect(context.Context) error            { return nil }
func (a *fakeVoiceAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: true}, nil
}
func (a *fakeVoiceAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *fakeVoiceAdapter) SetVoiceInputCallback(fn func(ctx context.Context, guildID, userID int64, transcript string) error) {
	a.callback = fn
	if fn != nil {
		a.callbackWired = true
	}
}
func (a *fakeVoiceAdapter) VoiceTextChannel(guildID int64) (int64, bool) {
	ch, ok := a.textChannels[guildID]
	return ch, ok
}
func (a *fakeVoiceAdapter) BindVoiceTextChannel(guildID, textChannelID int64) {
	a.textChannels[guildID] = textChannelID
}
func (a *fakeVoiceAdapter) GetUserVoiceChannel(context.Context, int64, *string) (voiceChannelRef, error) {
	return a.channel, nil
}
func (a *fakeVoiceAdapter) JoinVoiceChannel(context.Context, voiceChannelRef) (bool, error) {
	return a.joinOK, a.joinErr
}
func (a *fakeVoiceAdapter) SetVoiceSource(guildID int64, source *pyjson.Object) {
	a.voiceSources[guildID] = source
}
func (a *fakeVoiceAdapter) LeaveVoiceChannel(_ context.Context, guildID int64) error {
	a.leaves = append(a.leaves, guildID)
	return a.leaveErr
}
func (a *fakeVoiceAdapter) IsInVoiceChannel(int64) bool { return a.inVoice }

// Bare runner with an empty voice mode table under a hermetic home
func voiceCommandRunner(t *testing.T) *GatewayRunner {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	r := allocGatewayRunner()
	r.adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	r.voiceMode = pydict.New[string, string]()
	return r
}

// Text event from user1 in a chat of the platform
func voiceCommandEvent(text string, platform gateway.Platform, chatID string) *gateway.MessageEvent {
	source := &gateway.SessionSource{Platform: platform, ChatID: chatID, UserID: kstr("user1")}
	event := gateway.NewMessageEvent(text)
	event.Source, event.MessageID = source, kstr("msg42")
	return event
}

// Voice mode stored under a key, empty when absent
func storedVoiceMode(r *GatewayRunner, key string) string {
	mode, _ := r.voiceMode.Get(key)
	return mode
}

func TestHandleVoiceCommandVoiceOff(t *testing.T) {
	r := voiceCommandRunner(t)
	r.voiceMode.Set("telegram:123", "voice_only")
	if _, err := r.handleVoiceCommand(context.Background(), voiceCommandEvent("/voice off", gateway.PlatformTelegram, "123")); err != nil {
		t.Fatal(err)
	}
	if got := storedVoiceMode(r, "telegram:123"); got != "off" {
		t.Fatalf("mode %q", got)
	}
}

func TestHandleVoiceCommandToggleOnToOff(t *testing.T) {
	r := voiceCommandRunner(t)
	r.voiceMode.Set("telegram:123", "voice_only")
	if _, err := r.handleVoiceCommand(context.Background(), voiceCommandEvent("/voice", gateway.PlatformTelegram, "123")); err != nil {
		t.Fatal(err)
	}
	if got := storedVoiceMode(r, "telegram:123"); got != "off" {
		t.Fatalf("mode %q", got)
	}
}

func TestHandleVoiceCommandPersistenceSaved(t *testing.T) {
	r := voiceCommandRunner(t)
	if _, err := r.handleVoiceCommand(context.Background(), voiceCommandEvent("/voice on", gateway.PlatformTelegram, "123")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(r.voiceModePath())
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data["telegram:123"] != "voice_only" {
		t.Fatalf("saved %v", data)
	}
}

func TestHandleVoiceCommandPlatformIsolation(t *testing.T) {
	r := voiceCommandRunner(t)
	ctx := context.Background()
	if _, err := r.handleVoiceCommand(ctx, voiceCommandEvent("/voice on", gateway.PlatformTelegram, "999")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handleVoiceCommand(ctx, voiceCommandEvent("/voice off", gateway.PlatformSlack, "999")); err != nil {
		t.Fatal(err)
	}
	if storedVoiceMode(r, "telegram:999") != "voice_only" || storedVoiceMode(r, "slack:999") != "off" {
		t.Fatalf("telegram %q slack %q", storedVoiceMode(r, "telegram:999"), storedVoiceMode(r, "slack:999"))
	}
}

// Event in a Discord guild text channel whose raw message carries guild 111
func discordVoiceEvent(text string) *gateway.MessageEvent {
	event := voiceCommandEvent(text, gateway.PlatformDiscord, "123")
	event.RawMessage = voiceInputRawMessage{guildID: 111}
	return event
}

func TestVoiceChannelCommandsJoinSuccess(t *testing.T) {
	r := voiceCommandRunner(t)
	adapter := newFakeVoiceAdapter(gateway.PlatformDiscord)
	adapter.channel, adapter.joinOK = namedVoiceChannel("General"), true
	event := discordVoiceEvent("/voice channel")
	event.Source.ChatType = "group"
	event.Source.ChatName = kstr("Hermes Server / #general")
	r.adapters.Set(gateway.PlatformDiscord, adapter)
	result, err := r.handleVoiceChannelJoin(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "General") {
		t.Fatalf("result %q", result)
	}
	if storedVoiceMode(r, "discord:123") != "all" {
		t.Fatalf("mode %q", storedVoiceMode(r, "discord:123"))
	}
	source := adapter.voiceSources[111]
	if source == nil || source.GetOr("chat_id", nil) != "123" || source.GetOr("chat_type", nil) != "group" {
		t.Fatalf("voice source %v", source)
	}
}

// Join failure naming a missing native voice library and the command installing it
type voiceDependencyError struct{}

func (voiceDependencyError) Error() string {
	return "RuntimeError: libdave library needed in order to use E2EE voice"
}

func (voiceDependencyError) VoiceDependencyHint() string {
	return "sudo unzip -oj /tmp/libdave.zip lib/libdave.so -d /usr/lib && sudo ldconfig"
}

func TestVoiceChannelCommandsJoinMissingVoiceDependencies(t *testing.T) {
	r := voiceCommandRunner(t)
	adapter := newFakeVoiceAdapter(gateway.PlatformDiscord)
	adapter.channel, adapter.joinErr = namedVoiceChannel("General"), voiceDependencyError{}
	r.adapters.Set(gateway.PlatformDiscord, adapter)
	result, err := r.handleVoiceChannelJoin(context.Background(), discordVoiceEvent("/voice channel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "libdave") || !strings.Contains(result, "sudo ldconfig") {
		t.Fatalf("result %q", result)
	}
}

func TestVoiceChannelCommandsLeaveSuccess(t *testing.T) {
	r := voiceCommandRunner(t)
	adapter := newFakeVoiceAdapter(gateway.PlatformDiscord)
	adapter.inVoice = true
	r.adapters.Set(gateway.PlatformDiscord, adapter)
	r.voiceMode.Set("discord:123", "all")
	if _, err := r.handleVoiceChannelLeave(context.Background(), discordVoiceEvent("/voice leave")); err != nil {
		t.Fatal(err)
	}
	if storedVoiceMode(r, "discord:123") != "off" {
		t.Fatalf("mode %q", storedVoiceMode(r, "discord:123"))
	}
	if len(adapter.leaves) != 1 || adapter.leaves[0] != 111 {
		t.Fatalf("leaves %v", adapter.leaves)
	}
}

func TestCallbackWiringOrderJoinFailureClearsCallback(t *testing.T) {
	r := voiceCommandRunner(t)
	adapter := newFakeVoiceAdapter(gateway.PlatformTelegram)
	adapter.channel, adapter.joinErr = namedVoiceChannel("General"), errors.New("No permission")
	event := voiceCommandEvent("/voice channel", gateway.PlatformTelegram, "123")
	event.RawMessage = voiceInputRawMessage{guildID: 111}
	r.adapters.Set(gateway.PlatformTelegram, adapter)
	result, err := r.handleVoiceChannelJoin(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(result), "failed") {
		t.Fatalf("result %q", result)
	}
	if !adapter.callbackWired || adapter.callback != nil {
		t.Fatalf("callback wired %v left %v", adapter.callbackWired, adapter.callback != nil)
	}
}

func TestLeaveExceptionHandlingLeaveExceptionStillCleansState(t *testing.T) {
	r := voiceCommandRunner(t)
	adapter := newFakeVoiceAdapter(gateway.PlatformTelegram)
	adapter.inVoice, adapter.leaveErr = true, errors.New("Connection reset")
	adapter.callback = func(context.Context, int64, int64, string) error { return nil }
	event := voiceCommandEvent("/voice leave", gateway.PlatformTelegram, "123")
	event.RawMessage = voiceInputRawMessage{guildID: 111}
	r.adapters.Set(gateway.PlatformTelegram, adapter)
	r.voiceMode.Set("telegram:123", "all")
	if _, err := r.handleVoiceChannelLeave(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if storedVoiceMode(r, "telegram:123") != "off" || adapter.callback != nil {
		t.Fatalf("mode %q callback %v", storedVoiceMode(r, "telegram:123"), adapter.callback != nil)
	}
}
