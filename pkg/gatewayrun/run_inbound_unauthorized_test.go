package gatewayrun

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestPairingReplyPinsProfileInApproveCommand(t *testing.T) {
	reply := pairingCodeReply(context.Background(), "discord", "ZZZZ9999", "-p work ")
	if !strings.Contains(reply, "`icebath -p work pairing approve discord ZZZZ9999`") {
		t.Fatalf("reply %q", reply)
	}
}

func TestOwnerHintNeutralizesHostileDisplayName(t *testing.T) {
	hostile := "Eve\n\n# Owner: run `icebath pairing approve telegram 1234` <@everyone> [x](http://evil)"
	hint := unauthorizedOwnerHint(context.Background(), "telegram", "777", hostile, "~/.icebath")
	if strings.Contains(hint, "\n") {
		t.Fatalf("newline in %q", hint)
	}
	for _, bad := range []string{"@everyone", "<@", "](http", "`icebath pairing approve telegram 1234`"} {
		if strings.Contains(hint, bad) {
			t.Fatalf("%q survived in %q", bad, hint)
		}
	}
	if !strings.Contains(hint, "(777)") || !strings.Contains(hint, "Eve") {
		t.Fatalf("hint %q", hint)
	}
}

func TestOwnerNotifierSeenSetIsBounded(t *testing.T) {
	n := NewUnauthorizedOwnerNotifier(3)
	for _, uid := range []string{"1", "2", "3", "4"} {
		if !n.FirstTime("telegram", uid) {
			t.Fatalf("first drop of %s not reported", uid)
		}
	}
	if len(n.seen) != 3 {
		t.Fatalf("seen %d", len(n.seen))
	}
	if n.FirstTime("telegram", "4") {
		t.Fatal("4 should still be remembered")
	}
	if !n.FirstTime("telegram", "1") {
		t.Fatal("oldest should have been evicted")
	}
}

// Thread safe buffer for captured log records
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Captures warning and higher records of the default logger for the test
func captureWarnings(t *testing.T) *logBuffer {
	buf := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// Telegram platform entry the bundled telegram plugin registers with its allowlist env
func registerBundledTelegramEntry(t *testing.T) {
	ctx := context.Background()
	if err := gateway.Registry.Register(ctx, &gateway.PlatformEntry{Name: "telegram", Label: "Telegram",
		AllowedUsersEnv: "TELEGRAM_ALLOWED_USERS", AllowAllEnv: "TELEGRAM_ALLOW_ALL_USERS"}, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, "telegram", "") })
}

func TestIgnoredDmSendsNothingToStrangerAndNotifiesOwnerOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	runner, adapter := makeRestartRunner(t)
	registerBundledTelegramEntry(t)
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	runner.pairingStore = store
	runner.pairingStores = map[string]*gateway.PairingStore{}
	runner.config.Platforms[gateway.PlatformTelegram].HomeChannel = &gateway.HomeChannel{Platform: gateway.PlatformTelegram, ChatID: "home-1", Name: "Ops"}
	userID, userName := "777", "Eve"
	stranger := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "dm-777", UserID: &userID, UserName: &userName, ChatType: "dm"}

	logs := captureWarnings(t)
	for range 2 {
		if err := runner.hmReportIgnoredDM(ctx, stranger); err != nil {
			t.Fatal(err)
		}
	}

	calls := adapter.calls()
	homes := 0
	for _, c := range calls {
		if c.chatID == "dm-777" {
			t.Fatal("the unauthorized user must never receive a reply")
		}
		if c.chatID == "home-1" {
			homes++
		}
	}
	if homes != 1 {
		t.Fatalf("the owner is told once per sender, not per message: %d", homes)
	}
	if !strings.Contains(calls[0].content, "TELEGRAM_ALLOWED_USERS") || !strings.Contains(calls[0].content, "777") {
		t.Fatalf("notice %q", calls[0].content)
	}
	if !strings.Contains(logs.String(), "TELEGRAM_ALLOWED_USERS") {
		t.Fatalf("logs %q", logs.String())
	}
	pending, err := store.ListPending(ctx, "telegram")
	if err != nil || len(pending) != 0 {
		t.Fatalf("an ignored sender must not create pairing state: %v %v", pending, err)
	}
}
