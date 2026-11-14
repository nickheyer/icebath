package gatewayrun

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Log records at or above a level, the caplog of a case
type recordedLogs struct {
	mu      sync.Mutex
	level   slog.Level
	records []slog.Record
}

func (h *recordedLogs) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h *recordedLogs) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *recordedLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordedLogs) WithGroup(string) slog.Handler      { return h }

// Messages of the records at the level or above
func (h *recordedLogs) messages(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{}
	for _, r := range h.records {
		if r.Level >= level {
			out = append(out, r.Message)
		}
	}
	return out
}

// Captures the default logger's records at level and above for the rest of the case
func captureLogRecords(t *testing.T, level slog.Level) *recordedLogs {
	h := &recordedLogs{level: level}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func TestAKeyLessInjectFromAMessagingChatKeepsMainsRefusalLine(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(tmp, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := allocGatewayRunner()
	r.SetSessionAuthority(gateway.NewSessionAuthority(r, "default", "test", db, 0))
	logs := captureLogRecords(t, slog.LevelWarn)
	authority, live, err := keylessResolve(ctx, r, tmp, "agent:main:telegram:dm:42", "desk")
	if err != nil || authority != nil || live != nil {
		t.Fatalf("resolved %v %v %v", authority, live, err)
	}
	if got := logs.messages(slog.LevelWarn); !reflect.DeepEqual(got, []string{"inject_message: gateway mode requires an existing session_key or an origin"}) {
		t.Fatalf("warnings %q", got)
	}
}

func TestARefusalWarnsOncePerWindowAndExpiredMarksAreDropped(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	keylessWarnedMu.Lock()
	saved := keylessWarned
	keylessWarned = map[keylessWarnKey]float64{}
	keylessWarnedMu.Unlock()
	t.Cleanup(func() {
		keylessWarnedMu.Lock()
		keylessWarned = saved
		keylessWarnedMu.Unlock()
	})
	logs := captureLogRecords(t, slog.LevelDebug)
	clock := 1000.0
	keylessRefuseAt(clock, "old", "none", "no classic CLI session is attached in its profile")
	keylessRefuseAt(clock, "old", "none", "no classic CLI session is attached in its profile")
	clock += keylessWarnWindowS
	keylessRefuseAt(clock, "new", "several", "2 classic CLI sessions are attached in its profile")
	var heads []string
	for _, msg := range logs.messages(slog.LevelWarn) {
		heads = append(heads, strings.SplitN(msg, " has ", 2)[0])
	}
	if !reflect.DeepEqual(heads, []string{"inject_message from plugin old", "inject_message from plugin new"}) {
		t.Fatalf("warnings %q", heads)
	}
	keylessWarnedMu.Lock()
	defer keylessWarnedMu.Unlock()
	if !reflect.DeepEqual(keylessWarned, map[keylessWarnKey]float64{{"new", "several"}: clock}) {
		t.Fatalf("marks %v", keylessWarned)
	}
}
