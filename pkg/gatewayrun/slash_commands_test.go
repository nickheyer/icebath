package gatewayrun

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/kanban"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
)

// Source of a platform with optional thread and message ids
func homeThreadSource(platform gateway.Platform, threadID, messageID *string) *gateway.SessionSource {
	return &gateway.SessionSource{Platform: platform, ThreadID: threadID, MessageID: messageID}
}

func TestHomeThreadFromSourceSyntheticSlackThreadDropped(t *testing.T) {
	src := homeThreadSource(gateway.PlatformSlack, kstr("1755043010.123456"), kstr("1755043010.123456"))
	if got := homeThreadFromSource(src); got != nil {
		t.Fatalf("got %s", *got)
	}
}

func TestHomeThreadFromSourceGenuineSlackThreadKept(t *testing.T) {
	src := homeThreadSource(gateway.PlatformSlack, kstr("1755040000.000100"), kstr("1755043010.123456"))
	if got := homeThreadFromSource(src); got == nil || *got != "1755040000.000100" {
		t.Fatalf("got %v", got)
	}
}

func TestHomeThreadFromSourceNoThreadReturnsNone(t *testing.T) {
	if got := homeThreadFromSource(homeThreadSource(gateway.PlatformSlack, nil, nil)); got != nil {
		t.Fatalf("got %s", *got)
	}
}

func TestHomeThreadFromSourceNoMessageIDKeepsThread(t *testing.T) {
	src := homeThreadSource(gateway.PlatformSlack, kstr("1755040000.000100"), nil)
	if got := homeThreadFromSource(src); got == nil || *got != "1755040000.000100" {
		t.Fatalf("got %v", got)
	}
}

func TestHomeThreadFromSourceNonSlackPlatformUntouched(t *testing.T) {
	src := homeThreadSource(gateway.PlatformTelegram, kstr("2203"), kstr("2203"))
	if got := homeThreadFromSource(src); got == nil || *got != "2203" {
		t.Fatalf("got %v", got)
	}
}

// Discord dm source of a user stamped with an optional profile
func slashAccessSource(userID string, profile *string) *gateway.SessionSource {
	return &gateway.SessionSource{Platform: gateway.PlatformDiscord, UserID: kstr(userID), ChatID: "c1",
		UserName: kstr("name-" + userID), ChatType: "dm", Profile: profile}
}

func TestSecondaryProfileSlashPolicyUsesItsOwnConfig(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(ctx)
	on := true
	r.config.MultiplexProfiles = &on
	primary := gateway.NewPlatformConfig()
	primary.Enabled = true
	primary.Token = "***"
	r.config.Platforms[gateway.PlatformDiscord] = primary
	r.adapters = adapterDict(gateway.PlatformDiscord, newKanbanRecordingAdapterOn(gateway.PlatformDiscord))
	beta := gateway.DefaultGatewayConfig(ctx)
	betaDiscord := gateway.NewPlatformConfig()
	betaDiscord.Enabled = true
	betaDiscord.Extra = map[string]any{"allow_admin_from": []any{"admin"}, "user_allowed_commands": []any{"status"}}
	beta.Platforms[gateway.PlatformDiscord] = betaDiscord
	r.profileConfigs = orderedutil.NewMap[string, *gateway.GatewayConfig]()
	r.profileConfigs.Set("beta", beta)

	user := slashAccessSource("user", kstr("beta"))
	if denied := r.checkSlashAccess(ctx, user, "restart"); denied == "" || !strings.Contains(denied, "⛔") {
		t.Fatalf("denied %q", denied)
	}
	whoami, err := r.handleWhoamiCommand(ctx, &gateway.MessageEvent{Text: "/whoami", Source: user, MessageID: kstr("m1")})
	if err != nil || !strings.Contains(whoami, "Tier: user") {
		t.Fatalf("whoami %q %v", whoami, err)
	}
	if r.resumeCallerIsAdmin(user) {
		t.Fatal("user resumes as admin")
	}
	admin := slashAccessSource("admin", kstr("beta"))
	if denied := r.checkSlashAccess(ctx, admin, "restart"); denied != "" {
		t.Fatalf("admin denied %q", denied)
	}
	if !r.resumeCallerIsAdmin(admin) {
		t.Fatal("admin not recognised")
	}
}

func kstr(s string) *string { return &s }

// One recorded kanban ping
type kanbanSent struct {
	chatID   string
	text     string
	metadata map[string]any
}

// Telegram adapter recording every send and accepting every wake turn
type kanbanRecordingAdapter struct {
	*gateway.BasePlatformAdapter
	mu       sync.Mutex
	sent     []kanbanSent
	handled  []*gateway.MessageEvent
	failSend bool
	failWake bool
}

func (a *kanbanRecordingAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *kanbanRecordingAdapter) Disconnect(context.Context) error            { return nil }
func (a *kanbanRecordingAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if metadata == nil {
		metadata = map[string]any{}
	}
	a.sent = append(a.sent, kanbanSent{chatID, content, metadata})
	if a.failSend {
		return gateway.SendResult{}, &kanban.RuntimeError{Msg: "transient send failure"}
	}
	return gateway.SendResult{Success: true}, nil
}
func (a *kanbanRecordingAdapter) HandleMessage(_ context.Context, event *gateway.MessageEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handled = append(a.handled, event)
	if a.failWake {
		return &kanban.RuntimeError{Msg: "simulated wake failure"}
	}
	event.SetGatewayAccepted(true)
	return nil
}
func (a *kanbanRecordingAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Recording Telegram adapter
func newKanbanRecordingAdapter() *kanbanRecordingAdapter {
	return newKanbanRecordingAdapterOn(gateway.PlatformTelegram)
}

// Recording adapter of a platform
func newKanbanRecordingAdapterOn(platform gateway.Platform) *kanbanRecordingAdapter {
	a := &kanbanRecordingAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), platform, a)
	return a
}
