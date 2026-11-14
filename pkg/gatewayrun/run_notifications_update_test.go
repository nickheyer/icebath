package gatewayrun

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Telegram adapter recording every send and running an optional hook on each
type updateTestAdapter struct {
	*gateway.BasePlatformAdapter
	mu      sync.Mutex
	sent    []string
	chats   []string
	onSend  func(text string)
	sendErr error
}

func (a *updateTestAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *updateTestAdapter) Disconnect(context.Context) error            { return nil }
func (a *updateTestAdapter) Send(_ context.Context, chatID, content, _ string, _ map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	a.sent = append(a.sent, content)
	a.chats = append(a.chats, chatID)
	hook, err := a.onSend, a.sendErr
	a.mu.Unlock()
	if hook != nil {
		hook(content)
	}
	if err != nil {
		return gateway.SendResult{}, err
	}
	return gateway.SendResult{Success: true, MessageID: "1"}, nil
}
func (a *updateTestAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Recorded sends in order
func (a *updateTestAdapter) sends() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.sent...)
}

// Telegram test adapter with an enabled config
func newUpdateTestAdapter() *updateTestAdapter {
	return newUpdateTestAdapterFor(gateway.PlatformTelegram)
}

// Test adapter of a platform with an enabled config
func newUpdateTestAdapterFor(platform gateway.Platform) *updateTestAdapter {
	a := &updateTestAdapter{}
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, platform, a)
	return a
}

// Bare runner serving one telegram adapter, with the home set to a fresh directory
func updateRunner(t *testing.T) (*GatewayRunner, *updateTestAdapter, string) {
	t.Helper()
	hermesHome := filepath.Join(t.TempDir(), "hermes")
	if err := os.MkdirAll(hermesHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", hermesHome)
	r := allocGatewayRunner()
	r.sessions = map[string]*SessionState{}
	adapter := newUpdateTestAdapter()
	r.adapters = adapterDict(gateway.PlatformTelegram, adapter)
	return r, adapter, hermesHome
}

// Writes a JSON marker file
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

var updatePendingMarker = map[string]any{"platform": "telegram", "chat_id": "111", "user_id": "222", "session_key": "agent:main:telegram:dm:111"}

func TestWatchUpdateProgressStreamsOutputToAdapter(t *testing.T) {
	r, adapter, hermesHome := updateRunner(t)
	writeJSONFile(t, filepath.Join(hermesHome, ".update_pending.json"), updatePendingMarker)
	if err := os.WriteFile(filepath.Join(hermesHome, ".update_output.txt"), []byte("→ Fetching updates...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	streamed := make(chan struct{})
	var once sync.Once
	adapter.onSend = func(text string) {
		if strings.Contains(text, "Fetching updates") {
			if _, err := os.Stat(filepath.Join(hermesHome, ".update_exit_code")); err == nil {
				t.Errorf("exit code existed before the stream")
			}
			once.Do(func() { close(streamed) })
		}
	}
	watcher := spawnGatewayTask(context.Background(), "watch", func(ctx context.Context) (any, error) {
		return nil, r.watchUpdateProgress(ctx, 10*time.Millisecond, 20*time.Millisecond, 15*time.Second)
	})
	defer watcher.Cancel()
	select {
	case <-streamed:
	case <-time.After(5 * time.Second):
		t.Fatal("output never streamed")
	}
	if watcher.isDone() {
		t.Fatal("watcher finished before the child")
	}
	for _, text := range adapter.sends() {
		if strings.Contains(strings.ToLower(text), "update finished") {
			t.Fatalf("finished notice sent early %q", text)
		}
	}
	output, err := os.OpenFile(filepath.Join(hermesHome, ".update_output.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = output.WriteString("✓ Code updated!\n")
	_ = output.Close()
	if err := os.WriteFile(filepath.Join(hermesHome, ".update_exit_code"), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := watcher.wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	sent := adapter.sends()
	if len(sent) < 2 || !strings.Contains(sent[len(sent)-2], "Code updated!") || !strings.Contains(strings.ToLower(sent[len(sent)-1]), "update finished") {
		t.Fatalf("sent %q", sent)
	}
}

func TestWatchUpdateProgressDetectsAndForwardsPrompt(t *testing.T) {
	r, adapter, hermesHome := updateRunner(t)
	writeJSONFile(t, filepath.Join(hermesHome, ".update_pending.json"), updatePendingMarker)
	if err := os.WriteFile(filepath.Join(hermesHome, ".update_output.txt"), []byte("output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		writeJSONFile(t, filepath.Join(hermesHome, ".update_prompt.json"), map[string]any{"prompt": "Restore local changes? [Y/n]", "default": "y", "id": "test1"})
		time.Sleep(200 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(hermesHome, ".update_response"), []byte("y"), 0o644)
		_ = os.Remove(filepath.Join(hermesHome, ".update_prompt.json"))
		time.Sleep(200 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(hermesHome, ".update_exit_code"), []byte("0"), 0o644)
	}()
	if err := r.watchUpdateProgress(context.Background(), 100*time.Millisecond, 200*time.Millisecond, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	<-done
	found := false
	for _, text := range adapter.sends() {
		found = found || strings.Contains(text, "Restore local changes")
	}
	if !found {
		t.Fatalf("prompt not forwarded, sent %q", adapter.sends())
	}
}

func TestWatchUpdateProgressPromptIsRecoveredAfterWatcherRestart(t *testing.T) {
	runner1, adapter1, hermesHome := updateRunner(t)
	writeJSONFile(t, filepath.Join(hermesHome, ".update_pending.json"), updatePendingMarker)
	if err := os.WriteFile(filepath.Join(hermesHome, ".update_output.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(hermesHome, ".update_prompt.json")
	writeJSONFile(t, promptPath, map[string]any{"prompt": "Restore local changes? [Y/n]", "default": "y", "id": "restart-recover"})
	watch1 := spawnGatewayTask(context.Background(), "watch1", func(ctx context.Context) (any, error) {
		return nil, runner1.watchUpdateProgress(ctx, 50*time.Millisecond, 100*time.Millisecond, 10*time.Second)
	})
	for range 40 {
		if len(adapter1.sends()) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(adapter1.sends()); n != 1 {
		t.Fatalf("first watcher sent %d", n)
	}
	if _, err := os.Stat(promptPath); err != nil {
		t.Fatal("prompt marker removed before an answer")
	}
	watch1.Cancel()
	if _, err := watch1.wait(context.Background()); err == nil {
		t.Fatal("cancelled watcher returned no cancellation")
	}
	runner2 := allocGatewayRunner()
	runner2.sessions = map[string]*SessionState{}
	adapter2 := newUpdateTestAdapter()
	runner2.adapters = adapterDict(gateway.PlatformTelegram, adapter2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(hermesHome, ".update_response"), []byte("y"), 0o644)
		time.Sleep(200 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(hermesHome, ".update_exit_code"), []byte("0"), 0o644)
	}()
	if err := runner2.watchUpdateProgress(context.Background(), 50*time.Millisecond, 100*time.Millisecond, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	<-done
	prompts := 0
	for _, text := range adapter2.sends() {
		if strings.Contains(text, "Restore local changes") {
			prompts++
		}
	}
	if prompts != 1 {
		t.Fatalf("prompt sends %d in %q", prompts, adapter2.sends())
	}
}

// Writes a text file under the home
func writeHomeFile(t *testing.T, hermesHome, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(hermesHome, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Whether a path exists
func homeFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Runner with only the given adapters and the home set to a fresh directory
func notifyRunner(t *testing.T, adapters map[gateway.Platform]gateway.PlatformAdapter) (*GatewayRunner, string) {
	t.Helper()
	r, _, hermesHome := updateRunner(t)
	r.adapters = adapterDict()
	for platform, adapter := range adapters {
		r.adapters.Set(platform, adapter)
	}
	return r, hermesHome
}

// Sends the update notification and fails the test on a cleanup error
func sendUpdateNotificationOK(t *testing.T, r *GatewayRunner) bool {
	t.Helper()
	notified, err := r.sendUpdateNotification(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return notified
}

func TestSendUpdateNotificationDefersNotificationWhileUpdateStillRunning(t *testing.T) {
	adapter := newUpdateTestAdapter()
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformTelegram: adapter})
	pending := filepath.Join(hermesHome, ".update_pending.json")
	writeJSONFile(t, pending, map[string]any{"platform": "telegram", "chat_id": "67890", "user_id": "12345"})
	writeHomeFile(t, hermesHome, ".update_output.txt", []byte("still running"))
	if sendUpdateNotificationOK(t, r) {
		t.Fatal("expected deferral")
	}
	if len(adapter.sends()) != 0 || !homeFileExists(pending) {
		t.Fatalf("sends %q pending kept %v", adapter.sends(), homeFileExists(pending))
	}
}

func TestSendUpdateNotificationRecoversFromClaimedPendingFile(t *testing.T) {
	adapter := newUpdateTestAdapter()
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformTelegram: adapter})
	claimed := filepath.Join(hermesHome, ".update_pending.claimed.json")
	writeJSONFile(t, claimed, map[string]any{"platform": "telegram", "chat_id": "67890", "user_id": "12345"})
	writeHomeFile(t, hermesHome, ".update_output.txt", []byte("done"))
	writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	if !sendUpdateNotificationOK(t, r) {
		t.Fatal("expected delivery")
	}
	if len(adapter.sends()) != 1 || homeFileExists(claimed) {
		t.Fatalf("sends %q claimed kept %v", adapter.sends(), homeFileExists(claimed))
	}
}

func TestSendUpdateNotificationSendsNotificationWithOutput(t *testing.T) {
	adapter := newUpdateTestAdapter()
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformTelegram: adapter})
	writeJSONFile(t, filepath.Join(hermesHome, ".update_pending.json"), map[string]any{"platform": "telegram", "chat_id": "67890", "user_id": "12345", "timestamp": "2026-03-04T21:00:00"})
	writeHomeFile(t, hermesHome, ".update_output.txt", []byte("→ Found 3 new commit(s)\n✓ Code updated!\n✓ Update complete!"))
	writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	sendUpdateNotificationOK(t, r)
	sent := adapter.sends()
	if len(sent) != 1 || adapter.chats[0] != "67890" {
		t.Fatalf("sends %q chats %q", sent, adapter.chats)
	}
	if !strings.Contains(sent[0], "Update complete") && !strings.Contains(strings.ToLower(sent[0]), "update finished") {
		t.Fatalf("text %q", sent[0])
	}
}

func TestSendUpdateNotificationDropsStaleMarkerWhenThePlatformNeverConnects(t *testing.T) {
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{})
	var logs strings.Builder
	var logsMu sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&logs, &logsMu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	pending := filepath.Join(hermesHome, ".update_pending.json")
	stamp := time.Now().Add(-2 * time.Hour).Format("2006-01-02T15:04:05.000000")
	writeJSONFile(t, pending, map[string]any{"platform": "telegram", "chat_id": "67890", "user_id": "12345", "timestamp": stamp})
	writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	if !sendUpdateNotificationOK(t, r) {
		t.Fatal("expected a definitive drop")
	}
	for _, name := range []string{".update_pending.json", ".update_pending.claimed.json", ".update_output.txt", ".update_exit_code"} {
		if homeFileExists(filepath.Join(hermesHome, name)) {
			t.Fatalf("%s kept", name)
		}
	}
	logsMu.Lock()
	defer logsMu.Unlock()
	if !strings.Contains(logs.String(), "adapter never connected") {
		t.Fatalf("logs %s", logs.String())
	}
}

func TestSendUpdateNotificationKeepsWaitingForARecentMarker(t *testing.T) {
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{})
	pending := filepath.Join(hermesHome, ".update_pending.json")
	stamp := time.Now().Add(-5 * time.Minute).Format("2006-01-02T15:04:05.000000")
	writeJSONFile(t, pending, map[string]any{"platform": "telegram", "chat_id": "67890", "user_id": "12345", "timestamp": stamp})
	writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	if sendUpdateNotificationOK(t, r) {
		t.Fatal("expected deferral")
	}
	if !homeFileExists(pending) {
		t.Fatal("marker kept so a later poll can still deliver it")
	}
}

func TestSendUpdateNotificationCleansUpOnError(t *testing.T) {
	adapter := newUpdateTestAdapter()
	adapter.sendErr = errors.New("network error")
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformTelegram: adapter})
	pending := filepath.Join(hermesHome, ".update_pending.json")
	writeJSONFile(t, pending, map[string]any{"platform": "telegram", "chat_id": "111", "user_id": "222"})
	output := writeHomeFile(t, hermesHome, ".update_output.txt", []byte("✓ Done"))
	exitCode := writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	sendUpdateNotificationOK(t, r)
	if homeFileExists(pending) || homeFileExists(output) || homeFileExists(exitCode) {
		t.Fatal("markers kept after a failed send")
	}
}

func TestSendUpdateNotificationNoAdapterForPlatformPreservesMarkers(t *testing.T) {
	adapter := newUpdateTestAdapter()
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformTelegram: adapter})
	pending := filepath.Join(hermesHome, ".update_pending.json")
	writeJSONFile(t, pending, map[string]any{"platform": "discord", "chat_id": "111", "user_id": "222"})
	output := writeHomeFile(t, hermesHome, ".update_output.txt", []byte("Done"))
	exitCode := writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	if sendUpdateNotificationOK(t, r) {
		t.Fatal("expected deferral")
	}
	if len(adapter.sends()) != 0 || !homeFileExists(pending) || !homeFileExists(output) || !homeFileExists(exitCode) {
		t.Fatal("markers dropped or a send happened")
	}
	if homeFileExists(filepath.Join(hermesHome, ".update_pending.claimed.json")) {
		t.Fatal("claim not restored")
	}
}

func TestSendUpdateNotificationDeferredNotificationDeliversAfterReconnect(t *testing.T) {
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{})
	pending := filepath.Join(hermesHome, ".update_pending.json")
	writeJSONFile(t, pending, map[string]any{"platform": "discord", "chat_id": "111", "user_id": "222"})
	output := writeHomeFile(t, hermesHome, ".update_output.txt", []byte("✓ Update complete!"))
	exitCode := writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	if sendUpdateNotificationOK(t, r) || !homeFileExists(pending) {
		t.Fatal("first pass did not defer")
	}
	adapter := newUpdateTestAdapterFor(gateway.PlatformDiscord)
	r.adapters = adapterDict(gateway.PlatformDiscord, adapter)
	if !sendUpdateNotificationOK(t, r) {
		t.Fatal("second pass did not deliver")
	}
	if sent := adapter.sends(); len(sent) != 1 || !strings.Contains(sent[0], "Update complete") {
		t.Fatalf("sends %q", sent)
	}
	for _, path := range []string{pending, output, exitCode, filepath.Join(hermesHome, ".update_pending.claimed.json")} {
		if homeFileExists(path) {
			t.Fatalf("%s kept", path)
		}
	}
}

func TestSendUpdateNotificationCompletionNotificationToleratesInvalidUtf8Output(t *testing.T) {
	adapter := newUpdateTestAdapterFor(gateway.PlatformDiscord)
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformDiscord: adapter})
	pending := filepath.Join(hermesHome, ".update_pending.json")
	writeJSONFile(t, pending, map[string]any{"platform": "discord", "chat_id": "111", "user_id": "222"})
	output := writeHomeFile(t, hermesHome, ".update_output.txt", []byte("ok before\ninvalid byte: \x96\ncontinued after\n"))
	exitCode := writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	if !sendUpdateNotificationOK(t, r) {
		t.Fatal("expected delivery")
	}
	sent := adapter.sends()
	if len(sent) != 1 {
		t.Fatalf("sends %q", sent)
	}
	for _, want := range []string{"ok before", "invalid byte", "continued after", "Icebath update finished"} {
		if !strings.Contains(sent[0], want) {
			t.Fatalf("missing %q in %q", want, sent[0])
		}
	}
	if homeFileExists(pending) || homeFileExists(output) || homeFileExists(exitCode) {
		t.Fatal("markers kept")
	}
}

func TestSendUpdateNotificationFailedUpdateNoticeSaysStillRunningAndTrimsLog(t *testing.T) {
	adapter := newUpdateTestAdapterFor(gateway.PlatformDiscord)
	r, hermesHome := notifyRunner(t, map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformDiscord: adapter})
	writeJSONFile(t, filepath.Join(hermesHome, ".update_pending.json"), map[string]any{"platform": "discord", "chat_id": "111", "user_id": "222"})
	writeHomeFile(t, hermesHome, ".update_output.txt", []byte(strings.Repeat("x", 3000)+"\nERROR: pip failed\n"))
	writeHomeFile(t, hermesHome, ".update_exit_code", []byte("1"))
	sendUpdateNotificationOK(t, r)
	sent := adapter.sends()
	if len(sent) != 1 || !strings.Contains(sent[0], "ERROR: pip failed") || len([]rune(sent[0])) >= 1200 {
		t.Fatalf("sends %q", sent)
	}
}

func TestWatchUpdateProgressInvalidUtf8UpdateOutputDoesNotCrashWatcher(t *testing.T) {
	r, adapter, hermesHome := updateRunner(t)
	writeJSONFile(t, filepath.Join(hermesHome, ".update_pending.json"), map[string]any{"platform": "telegram", "chat_id": "67890", "user_id": "12345"})
	writeHomeFile(t, hermesHome, ".update_output.txt", []byte("ok before\n\xe2\x9c invalid-continuation: \x96\ncontinued after\n"))
	writeHomeFile(t, hermesHome, ".update_exit_code", []byte("0"))
	if err := r.watchUpdateProgress(context.Background(), 10*time.Millisecond, 10*time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	sent := strings.Join(adapter.sends(), "\n")
	if !strings.Contains(sent, "ok before") || !strings.Contains(sent, "continued after") {
		t.Fatalf("sent %q", sent)
	}
}

// Writer serialising concurrent log writes into a builder
type lockedWriter struct {
	b  *strings.Builder
	mu *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
