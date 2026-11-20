package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type messageCapture struct {
	mu   *sync.Mutex
	msgs *[]string
}

func (h messageCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h messageCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}
func (h messageCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h messageCapture) WithGroup(string) slog.Handler      { return h }

// Collects every log message for the test and returns a reader of the attempt telemetry payloads
func captureAttemptRecords(t *testing.T) func() []*pyjson.Object {
	t.Helper()
	var mu sync.Mutex
	var msgs []string
	prev := slog.Default()
	slog.SetDefault(slog.New(messageCapture{&mu, &msgs}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []*pyjson.Object {
		mu.Lock()
		defer mu.Unlock()
		var out []*pyjson.Object
		for _, m := range msgs {
			if _, rest, ok := strings.Cut(m, "context compression attempt telemetry: "); ok {
				v, err := pyjson.Loads(rest)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, v.(*pyjson.Object))
			}
		}
		return out
	}
}

func TestGenericCallerVerdictsDeferToTheRecordedFailureClass(t *testing.T) {
	cases := []struct {
		recorded         any
		attemptID        string
		caller, expected string
	}{
		{nil, "this-attempt", "no_progress", "no_progress"},
		{"summary_auth_failure", "this-attempt", "summary_generation_aborted", "summary_auth_failure"},
		{"no_compressible_window", "this-attempt", "attempt_superseded", "attempt_superseded"},
		{"no_compressible_window", "previous-attempt", "no_progress", "no_progress"},
	}
	for _, k := range cases {
		records := captureAttemptRecords(t)
		c := newCompressorWith(t, 100_000, nil)
		c.Last = pyjson.ObjectOf("failure_class", k.recorded, "attempt_id", k.attemptID)
		a := &AIAgent{contextCompressor: c, sessionID: "session-telemetry-test"}
		a.compressionAttemptID = "this-attempt"
		emitAbortedAttemptTelemetry(context.Background(), a, time.Now(), k.caller, nil)
		got := records()
		if len(got) != 1 {
			t.Fatalf("%v: %d records", k, len(got))
		}
		if class, _ := got[0].Get("failure_class"); class != k.expected {
			t.Fatalf("%v: failure class %v", k, class)
		}
	}
}

func TestBlockedRecordNeverCarriesTheCooldownSeconds(t *testing.T) {
	records := captureAttemptRecords(t)
	a := &AIAgent{sessionID: "s"}
	a.compressionAttemptID = "a"
	emitBlockedAttemptTelemetry(context.Background(), a, time.Now(), nil, nil, "structural_backoff:917")
	got := records()
	if len(got) != 1 {
		t.Fatalf("%d records", len(got))
	}
	text, _ := pyjson.Dumps(got[0])
	if class, _ := got[0].Get("failure_class"); class != "blocked:structural_backoff" || strings.Contains(text, "917") {
		t.Fatalf("record %s", text)
	}
}
