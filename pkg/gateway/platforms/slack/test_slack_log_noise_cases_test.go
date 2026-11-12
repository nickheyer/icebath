package slack

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Log record captured with its level and message
type logRecord struct {
	level slog.Level
	msg   string
}

// Default logger swapped for one recording every record from debug up
type logCapture struct {
	mu      sync.Mutex
	records []logRecord
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	var buf bytes.Buffer
	buf.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool { buf.WriteString(" " + a.String()); return true })
	c.mu.Lock()
	c.records = append(c.records, logRecord{r.Level, buf.String()})
	c.mu.Unlock()
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func captureLogs(t *testing.T) *logCapture {
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	for _, r := range c.records {
		parts = append(parts, r.msg)
	}
	return strings.Join(parts, "\n")
}

func TestCatchAllEventAckCatchallAcksUnsubscribedEventQuietly(t *testing.T) {
	a, _, _ := newHarnessAdapter(t, nil)
	if err := a.registerBoltHandlers(context.Background()); err != nil {
		t.Fatal(err)
	}
	var catchAll eventListener
	for i := len(a.app.events) - 1; i >= 0; i-- {
		if a.app.events[i].pattern != nil {
			catchAll = a.app.events[i].listener
			break
		}
	}
	if catchAll == nil {
		t.Fatal("no catch-all listener registered")
	}
	logs := captureLogs(t)
	secret := "confidential launch codes in a reaction item"
	event := map[string]any{"type": "reaction_added", "user": "U123", "item": map[string]any{"type": "message", "text": secret}}
	catchAll(context.Background(), event, map[string]any{"event": event})
	found := false
	for _, r := range logs.records {
		if r.level > slog.LevelDebug {
			t.Fatalf("catch-all logged above debug: %q", r.msg)
		}
		found = found || strings.Contains(r.msg, "reaction_added")
	}
	if !found || strings.Contains(logs.text(), secret) {
		t.Fatalf("records %v", logs.records)
	}
}

func TestInboundLogPrivacyEntryDiagnosticLogIsMetadataOnly(t *testing.T) {
	a, _, _ := newHarnessAdapter(t, nil)
	logs := captureLogs(t)
	secret := "bot payload that must stay out of logs"
	event := map[string]any{"type": "message", "subtype": "bot_message", "user": "U_OTHER_BOT", "bot_id": "B_OTHER",
		"bot_profile": map[string]any{"name": "Peer Bot"}, "ts": "12345.6789", "channel": "C_SHARED", "text": secret}
	a.dedup.IsDuplicate(workspaceEventID("", "12345.6789"))
	if err := a.handleSlackMessage(context.Background(), event, nil); err != nil {
		t.Fatal(err)
	}
	var entry []string
	for _, r := range logs.records {
		if strings.Contains(r.msg, "event received") {
			entry = append(entry, r.msg)
		}
	}
	if len(entry) == 0 || !strings.Contains(entry[0], "bot_id=B_OTHER") || strings.Contains(logs.text(), secret) {
		t.Fatalf("entry lines %v", entry)
	}
}

func TestClarifyLogPrivacyClarifyResolutionLogsIndexNotTextAtInfo(t *testing.T) {
	secret := "migrate the production database tonight"
	clearClarifyState(t, "sk-priv")
	tools.RegisterGatewayClarify("cid-priv", "sk-priv", "Pick", []string{"noop", secret}, false)
	a, _ := approvalAdapter(t)
	attachAuthRunner(a, nil)
	a.clarifyResolved.Set("9.9", false)
	logs := captureLogs(t)
	a.handleClarifyAction(context.Background(), (&ackRecorder{}).ack, map[string]any{"message": map[string]any{"ts": "9.9", "blocks": []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "❓ Pick"}}}},
		"channel": map[string]any{"id": "C1"}, "user": map[string]any{"name": "norbert", "id": "U_NORBERT"}},
		map[string]any{"action_id": "hermes_clarify_choice_1", "value": "cid-priv|1"})
	resolved, debugHas := false, false
	for _, r := range logs.records {
		if r.level >= slog.LevelInfo {
			resolved = resolved || strings.Contains(r.msg, "resolved clarify")
			if strings.Contains(r.msg, secret) {
				t.Fatalf("choice text at info: %q", r.msg)
			}
		}
		if r.level == slog.LevelDebug && strings.Contains(r.msg, secret) {
			debugHas = true
		}
	}
	if !resolved || !debugHas {
		t.Fatalf("records %v", logs.records)
	}
}
