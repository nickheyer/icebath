package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Home ICEBATH_HOME named when TestMain started, before any test touched logging
var homeAtTestMain string

// Default slog logger as packages capture it at init, before any setup installs the bridge
var earlySlog = slog.Default()

var secretPattern = regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	dir, err := os.MkdirTemp("", "icebath-logging-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("ICEBATH_HOME", dir)
	homeAtTestMain = dir
	RedactingHandler = func(next slog.Handler) slog.Handler { return &redactDouble{next: next} }
	RedactSensitiveText = func(text string) string { return secretPattern.ReplaceAllString(text, "sk-...") }
	ResolveProfilePolicy = func(string) {}
	code := m.Run()
	resetQueuedHandlers()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// Redaction double honouring a per home opt out in the home's .env or config.yaml
type redactDouble struct{ next slog.Handler }

func (d *redactDouble) Enabled(ctx context.Context, l slog.Level) bool { return d.next.Enabled(ctx, l) }

func (d *redactDouble) Handle(ctx context.Context, r slog.Record) error {
	msg := r.Message
	if redactsFor(home.Dir(ctx)) {
		msg = secretPattern.ReplaceAllString(msg, "sk-...")
	}
	return d.next.Handle(ctx, slog.NewRecord(r.Time, r.Level, msg, r.PC))
}

func (d *redactDouble) WithAttrs(a []slog.Attr) slog.Handler {
	return &redactDouble{next: d.next.WithAttrs(a)}
}

func (d *redactDouble) WithGroup(g string) slog.Handler {
	return &redactDouble{next: d.next.WithGroup(g)}
}

// Redaction policy of a home, on unless its .env or config.yaml opts out
func redactsFor(h string) bool {
	if env, err := os.ReadFile(filepath.Join(h, ".env")); err == nil && strings.Contains(string(env), "ICEBATH_REDACT_SECRETS=false") {
		return false
	}
	if cfg, err := os.ReadFile(filepath.Join(h, "config.yaml")); err == nil && strings.Contains(string(cfg), "redact_secrets: false") {
		return false
	}
	return true
}

// Clears the logging state before and after a test the way the autouse fixture does
func resetLogging(t *testing.T) {
	t.Helper()
	setupMu.Lock()
	loggingInitialized = false
	setupMu.Unlock()
	resetQueuedHandlers()
	prevLevel := root.Level()
	root.SetLevel(LevelNotSet)
	pre := root.Handlers()
	t.Cleanup(func() {
		resetQueuedHandlers()
		for _, h := range root.Handlers() {
			found := false
			for _, p := range pre {
				found = found || p == h
			}
			if !found {
				root.RemoveHandler(h)
				quietly(h.Close)
			}
		}
		root.SetLevel(prevLevel)
		setupMu.Lock()
		loggingInitialized = false
		setupMu.Unlock()
		ClearSessionContext()
	})
}

// Fresh resolved ICEBATH_HOME for one test with the logging state reset
func testHome(t *testing.T) string {
	t.Helper()
	resetLogging(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", dir)
	return dir
}

// Second resolved directory for profile homes outside the launch home
func otherDir(t *testing.T, name string) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(base, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Context whose home override is h, the set_hermes_home_override form
func homeCtx(h string) context.Context { return home.WithOverride(context.Background(), h) }

// Text of path, empty when it does not exist
func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.TrimPrefix(string(b), "\ufeff")
}

func mustSetup(t *testing.T, o Options) string {
	t.Helper()
	dir, err := SetupLogging(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustRoute(t *testing.T, homes ...string) {
	t.Helper()
	ok, err := EnableProfileLogRouting(homes)
	if err != nil || !ok {
		t.Fatalf("EnableProfileLogRouting(%v) = %v, %v", homes, ok, err)
	}
}

var stderrMu sync.Mutex

// Everything fn writes to os.Stderr
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	stderrMu.Lock()
	defer stderrMu.Unlock()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String()
}

// Queued handlers of a type
func queuedRouters() []*profileRoutingFileHandler {
	queueState.mu.Lock()
	defer queueState.mu.Unlock()
	var out []*profileRoutingFileHandler
	for _, h := range queueState.handlers {
		if r, ok := h.(*profileRoutingFileHandler); ok {
			out = append(out, r)
		}
	}
	return out
}

func queuedFileHandlers() []*managedRotatingFileHandler {
	queueState.mu.Lock()
	defer queueState.mu.Unlock()
	var out []*managedRotatingFileHandler
	for _, h := range queueState.handlers {
		if m, ok := h.(*managedRotatingFileHandler); ok {
			out = append(out, m)
		}
	}
	return out
}

// Record of name at level for direct handler calls
func testRecord(name string, level int, msg string) *Record {
	return makeRecord(context.Background(), name, level, msg, time.Now(), 0)
}
