package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type deliveryAdapter struct {
	calls   [][]any
	results []SendResult
	splits  bool
	topics  []string
}

func (a *deliveryAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (SendResult, error) {
	a.calls = append(a.calls, []any{chatID, content, metadata})
	if len(a.results) > 0 {
		r := a.results[0]
		a.results = a.results[1:]
		return r, nil
	}
	return SendResult{Success: true, MessageID: "m1"}, nil
}

func (a *deliveryAdapter) SplitsLongMessages() bool { return a.splits }

func (a *deliveryAdapter) EnsureDMTopic(_ context.Context, chatID, name string, force bool) (string, error) {
	a.topics = append(a.topics, name)
	if force {
		return "778", nil
	}
	return "777", nil
}

type relayAdapter struct {
	deliveryAdapter
	fronts Platform
	sentAs []Platform
}

func (r *relayAdapter) SendForPlatform(ctx context.Context, p Platform, chatID, content string, metadata map[string]any) (SendResult, error) {
	r.sentAs = append(r.sentAs, p)
	return r.Send(ctx, chatID, content, "", metadata)
}

func (r *relayAdapter) FrontsPlatform(p Platform) bool { return p == r.fronts }

func deliveryConfig() *GatewayConfig {
	return &GatewayConfig{Platforms: map[Platform]*PlatformConfig{}, FilterSilenceNarration: true}
}

func TestResolveDeliveryTransportPrefersNativeAndOnlyAFrontingRelay(t *testing.T) {
	ctx := context.Background()
	cfg := deliveryConfig()
	native, relay := &deliveryAdapter{}, &relayAdapter{fronts: PlatformTelegram}
	tr := ResolveDeliveryTransport(ctx, PlatformTelegram, cfg, map[string]any{"telegram": native, "relay": relay})
	if tr == nil || tr.Adapter != native || tr.IsRelay() {
		t.Fatalf("%+v", tr)
	}
	cfg.Platforms[PlatformTelegram] = &PlatformConfig{Enabled: false}
	tr = ResolveDeliveryTransport(ctx, PlatformTelegram, cfg, map[string]any{"telegram": native, "relay": relay})
	if tr == nil || !tr.IsRelay() {
		t.Fatalf("disabled native must yield to the relay %+v", tr)
	}
	if ResolveDeliveryTransport(ctx, PlatformSlack, cfg, map[string]any{"relay": relay}) != nil {
		t.Fatal("relay hijacked a platform it does not front")
	}
	cfg.Platforms[PlatformRelay] = &PlatformConfig{Enabled: false}
	if ResolveDeliveryTransport(ctx, PlatformTelegram, cfg, map[string]any{"relay": relay}) != nil {
		t.Fatal("disabled relay")
	}
	if !LooksLikeTelegramPrivateChatID(" 42 ") || LooksLikeTelegramPrivateChatID("-100") || LooksLikeTelegramPrivateChatID("x") {
		t.Fatal("private chat id")
	}
	if !LooksLikeInt("-3") || !LooksLikeInt("1_000") || LooksLikeInt("topic") || LooksLikeInt("") {
		t.Fatal("looks like int")
	}
}

func TestDeliveryTargetParseAndString(t *testing.T) {
	ctx := context.Background()
	thread := "9"
	origin := &SessionSource{Platform: PlatformTelegram, ChatID: "42", ThreadID: &thread}
	for _, c := range []struct {
		in, out string
		want    DeliveryTarget
	}{
		{"origin", "origin", DeliveryTarget{Platform: PlatformTelegram, ChatID: "42", ThreadID: &thread, IsOrigin: true}},
		{" local ", "local", DeliveryTarget{Platform: PlatformLocal}},
		{"Telegram", "telegram", DeliveryTarget{Platform: PlatformTelegram}},
		{"telegram:123:Ops", "telegram:123:Ops", DeliveryTarget{Platform: PlatformTelegram, ChatID: "123", ThreadID: strPtr("Ops"), IsExplicit: true}},
	} {
		got := ParseDeliveryTarget(ctx, c.in, origin)
		if !reflect.DeepEqual(got, c.want) || got.String() != c.out {
			t.Fatalf("%q: %+v %q", c.in, got, got.String())
		}
	}
	unknown := ParseDeliveryTarget(ctx, "nosuchplatform:1", nil)
	if unknown.Platform != PlatformLocal || unknown.UnknownPlatform == nil || unknown.String() != "nosuchplatform:1" {
		t.Fatalf("%+v", unknown)
	}
	if o := ParseDeliveryTarget(ctx, "ORIGIN", nil); o.Platform != PlatformLocal || !o.IsOrigin {
		t.Fatalf("%+v", o)
	}
}

func strPtr(s string) *string { return &s }

func TestDeliverToPlatformRoutingSilenceAndTopics(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	native := &deliveryAdapter{}
	r := NewDeliveryRouterIn(ctx, deliveryConfig(), map[string]any{"telegram": native}, nil)
	if res, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42"}, "*(silent)*", nil, nil); err != nil ||
		res.(map[string]any)["filtered"] != "silence_narration" || len(native.calls) != 0 {
		t.Fatalf("%v %v", res, err)
	}
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42"}, "...", map[string]any{"job_id": "j"}, nil); err != nil || len(native.calls) != 1 {
		t.Fatal("a cron artifact is never filtered")
	}
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformSlack, ChatID: "1"}, "x", nil, nil); err == nil || err.Error() != "No adapter configured for slack" {
		t.Fatal(err)
	}
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram}, "x", nil, nil); err == nil || err.Error() != "No chat ID for telegram delivery" {
		t.Fatal(err)
	}
	native.calls = nil
	native.results = []SendResult{{Success: false, Error: "Bad Request: message thread not found"}}
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42", ThreadID: strPtr("Ops")}, "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(native.topics, []string{"Ops", "Ops"}) || len(native.calls) != 2 || native.calls[1][2].(map[string]any)["thread_id"] != "778" {
		t.Fatalf("%v %v", native.topics, native.calls)
	}
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42", ThreadID: strPtr("5")}, "hi", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "requires telegram_reply_to_message_id") {
		t.Fatal(err)
	}
	native.results = []SendResult{{Success: false, Error: "boom", RawResponse: map[string]any{"partial_overflow": true}}}
	var partial *PartialDeliveryError
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42"}, "hi", nil, nil); !errors.As(err, &partial) || partial.Msg != "boom" {
		t.Fatal(err)
	}
	native.results = []SendResult{{Success: false}}
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42"}, "hi", nil, nil); err == nil || err.Error() != "telegram delivery failed" {
		t.Fatal(err)
	}
	cfg := deliveryConfig()
	user := "u7"
	cfg.Platforms[PlatformTelegram] = &PlatformConfig{Enabled: true, HomeChannel: &HomeChannel{Platform: PlatformTelegram, ChatID: "42", UserID: &user}}
	relay := &relayAdapter{fronts: PlatformTelegram}
	rr := NewDeliveryRouterIn(ctx, cfg, map[string]any{"relay": relay}, nil)
	if _, err := rr.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42"}, "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	if relay.sentAs[0] != PlatformTelegram || relay.calls[0][2].(map[string]any)["user_id"] != "u7" {
		t.Fatalf("%v %v", relay.sentAs, relay.calls)
	}
}

func TestOversizedOutputIsAuditedAndTruncated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	native := &deliveryAdapter{}
	r := NewDeliveryRouterIn(ctx, deliveryConfig(), map[string]any{"telegram": native}, nil)
	big := strings.Repeat("é", MaxPlatformOutput+10)
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42"}, big, map[string]any{"job_id": "j1"}, nil); err != nil {
		t.Fatal(err)
	}
	sent := native.calls[0][1].(string)
	if len([]rune(sent)) != MaxPlatformOutput || !strings.Contains(sent, "[truncated, full output saved to "+filepath.Join(dir, "cron", "output", "j1_")) {
		t.Fatalf("%d %q", len([]rune(sent)), sent[len(sent)-120:])
	}
	native.splits = true
	if _, err := r.DeliverToPlatform(ctx, DeliveryTarget{Platform: PlatformTelegram, ChatID: "42"}, big, map[string]any{"job_id": "j1"}, nil); err != nil || native.calls[1][1] != big {
		t.Fatal("a chunking adapter gets the full payload")
	}
}

func TestDeliverTracksDeadTargetsAndSavesLocal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	native := &deliveryAdapter{results: []SendResult{{Success: false, Error: "Forbidden: bot was blocked by the user"}}}
	r := NewDeliveryRouterIn(ctx, deliveryConfig(), map[string]any{"telegram": native}, nil)
	targets := []DeliveryTarget{{Platform: PlatformTelegram, ChatID: "42"}, {Platform: PlatformLocal}, ParseDeliveryTarget(ctx, "bogus", nil)}
	res := r.Deliver(ctx, "hello", targets, "job9", "Nightly", [][2]any{{"k", "v"}})
	if res["telegram:42"]["success"] != false || !r.DeadTargets.IsDead("Telegram", " 42") {
		t.Fatalf("%v", res)
	}
	if res["bogus"]["error"] != "unknown_platform: bogus" {
		t.Fatalf("%v", res["bogus"])
	}
	local := res["local"]["result"].(map[string]any)["path"].(string)
	body, _ := os.ReadFile(local)
	if !strings.HasPrefix(string(body), "# Nightly\n\n**Timestamp:** ") || !strings.Contains(string(body), "**Job ID:** job9\n**k:** v\n\n---\n\nhello") {
		t.Fatalf("%q", body)
	}
	again := r.Deliver(ctx, "hello", targets[:1], "", "", nil)
	if again["telegram:42"]["skipped"] != "dead_target" || len(native.calls) != 1 {
		t.Fatalf("%v", again)
	}
	reloaded := NewDeadTargetRegistry(ctx, "")
	if !reloaded.IsDead("telegram", "42") || !reloaded.Clear("telegram", "42") || reloaded.Clear("telegram", "42") {
		t.Fatal("persisted registry")
	}
	if NewDeadTargetRegistry(ctx, "").IsDead("telegram", "42") {
		t.Fatal("clear must persist")
	}
	if ClassifyDeadError("Bad Request: message thread not found; chat not found") != "" || ClassifyDeadError("Bad Request: chat not found") != "not_found" {
		t.Fatal("only a whole chat not found is dead")
	}
	if ClassifySendError(&PyError{Kind: "ConnectError", Msg: "x"}, "") != "transient" || ClassifySendError(nil, "Message is too long") != "too_long" ||
		ClassifySendError(nil, "can't parse entities") != "bad_format" || ClassifySendError(nil, "Too Many Requests") != "rate_limited" || ClassifySendError(nil, "?") != "unknown" {
		t.Fatal("classifier")
	}
}

func TestSessionSourceWireLayout(t *testing.T) {
	ctx := context.Background()
	guild := "g1"
	src := (&SessionSource{Platform: PlatformDiscord, ChatID: "c", GuildID: &guild, AutoThreadCreated: true, Profile: strPtr("p")}).Normalize()
	d := src.ToDict()
	want := []string{"platform", "chat_id", "chat_name", "chat_type", "user_id", "user_name", "thread_id", "chat_topic", "scope_id", "guild_id", "profile", "auto_thread_created"}
	if !reflect.DeepEqual(d.Keys, want) || *src.ScopeID != "g1" || src.ChatType != "dm" {
		t.Fatalf("%v", d.Keys)
	}
	back, err := SessionSourceFromDict(ctx, map[string]any{"platform": "discord", "chat_id": 5, "guild_id": "g2", "chat_type": "group", "chat_name": "ops"})
	if err != nil || back.ChatID != "5" || *back.ScopeID != "g2" || *back.GuildID != "g2" || back.Description() != "group: ops" {
		t.Fatalf("%+v %v", back, err)
	}
	if (&SessionSource{Platform: PlatformLocal}).Description() != "CLI terminal" {
		t.Fatal("local")
	}
	dm := &SessionSource{Platform: PlatformTelegram, ChatID: "1", ChatType: "dm", ThreadID: strPtr("t")}
	if dm.Description() != "DM with user, thread: t" {
		t.Fatal(dm.Description())
	}
}
