package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Records whose message holds every needle, filtered to one level
func (h *captureHandler) atLevel(level slog.Level, needles ...string) (match, other int) {
	for _, r := range h.all() {
		hit := true
		for _, n := range needles {
			hit = hit && strings.Contains(r.msg, n)
		}
		if !hit {
			continue
		}
		if r.level == level {
			match++
		} else {
			other++
		}
	}
	return match, other
}

func TestRetryAttemptsLogDebugTransitionsWarn(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxReconnectRetries = 2
	s, _ := runnerTask(t, "noisy", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call == 1 {
			fakeConnected(s, false)
		}
		return "", &mcpConnectionError{msg: "backend down"}
	})
	logs := captureLogs(t)
	_ = s.start(context.Background(), s.rendered)
	waitUntil(t, "flapping server never parked", s.parked)
	s.shutdown(context.Background())
	debug, louder := logs.atLevel(slog.LevelDebug, "connection lost (attempt")
	if debug == 0 || louder != 0 {
		t.Fatalf("per attempt retry logs must all be DEBUG, got %d debug and %d louder: %s", debug, louder, logs.text())
	}
	if warn, _ := logs.atLevel(slog.LevelWarn, "parking"); warn != 1 {
		t.Fatalf("expected exactly one degraded to parked warning, got %d: %s", warn, logs.text())
	}
}

func TestInitialRetryAttemptsLogDebug(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	s, _ := runnerTask(t, "startup", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		return "", &mcpConnectionError{msg: "dns blip"}
	})
	logs := captureLogs(t)
	_ = s.start(context.Background(), s.rendered)
	waitUntil(t, "initial failures never parked", s.parked)
	s.shutdown(context.Background())
	debug, louder := logs.atLevel(slog.LevelDebug, "initial connection failed (attempt")
	if debug == 0 || louder != 0 {
		t.Fatalf("initial attempt logs must all be DEBUG, got %d debug and %d louder: %s", debug, louder, logs.text())
	}
	if warn, _ := logs.atLevel(slog.LevelWarn, "connecting → parked"); warn != 1 {
		t.Fatalf("expected exactly one connecting to parked warning, got %d: %s", warn, logs.text())
	}
}

func TestLifecycleScopedShutdownReleasesStderrLog(t *testing.T) {
	base := isolateHome(t)
	resetLedgers(t)
	t.Cleanup(func() { closeStderrLogs("") })
	first, second := filepath.Join(base, "first"), filepath.Join(base, "second")
	firstLog := getStderrLog(home.WithOverride(context.Background(), first))
	secondLog := getStderrLog(home.WithOverride(context.Background(), second))
	scope := home.Key(first)
	ShutdownMCPServers(context.Background(), &scope, nil, time.Second)
	if _, err := firstLog.WriteString("x"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("scoped shutdown left the first profile log open: %v", err)
	}
	if _, err := secondLog.WriteString("other profile remains usable\n"); err != nil {
		t.Fatalf("scoped shutdown closed another profile log: %v", err)
	}
	reopened := getStderrLog(home.WithOverride(context.Background(), first))
	if reopened == firstLog {
		t.Fatal("reload returned the released handle")
	}
	if _, err := reopened.WriteString("reload can reopen the log\n"); err != nil {
		t.Fatal(err)
	}
	ShutdownMCPServers(context.Background(), nil, nil, time.Second)
	for _, fh := range []*os.File{secondLog, reopened} {
		if _, err := fh.WriteString("x"); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("wildcard shutdown left a log open: %v", err)
		}
	}
}
