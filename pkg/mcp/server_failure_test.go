package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

type brokenPipeError struct{}

func (brokenPipeError) Error() string { return "" }

// Transport whose every send fails with a task group wrapped broken pipe
type groupFailTransport struct{ in chan *Message }

func (g groupFailTransport) Send(context.Context, *Message) error {
	return errors.Join(context.Canceled, brokenPipeError{})
}
func (g groupFailTransport) Receive() <-chan *Message { return g.in }
func (g groupFailTransport) Err() error               { return nil }
func (g groupFailTransport) Close() error             { return nil }
func (g groupFailTransport) Label() string            { return "group-fail" }

func TestFailureKeepaliveLogsRootCause(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	keepaliveFloor = 10 * time.Millisecond
	logs := captureLogs(t)
	s := newServerTask("pipey", &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
	s.config = map[string]any{"keepalive_interval": 0.01}
	s.setSession(NewSession(groupFailTransport{make(chan *Message)}, SessionOptions{}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reason, err := s.waitForLifecycleEvent(ctx, nil)
	if err != nil || reason != "reconnect" {
		t.Fatalf("reason %q err %v", reason, err)
	}
	var keepalive []string
	for _, r := range logs.all() {
		if strings.Contains(r.msg, "keepalive failed") {
			keepalive = append(keepalive, r.msg)
		}
	}
	if len(keepalive) == 0 || !strings.Contains(strings.Join(keepalive, "\n"), "brokenPipeError") || strings.Contains(strings.Join(keepalive, "\n"), "joinError") {
		t.Fatalf("keepalive logs %v", keepalive)
	}
}

func TestFailurePermanentStdioFailureParksWithoutRetryLadder(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	logs := captureLogs(t)
	s, calls := runnerTask(t, "missing-cmd", map[string]any{"command": "nope"}, func(s *serverTask, ctx context.Context, call int) (string, error) {
		return "", &os.PathError{Op: "exec", Path: "nonexistent-mcp-command", Err: os.ErrNotExist}
	})
	_ = s.start(context.Background(), s.rendered)
	waitUntil(t, "permanent failure never parked", s.parked)
	if calls.Load() != 1 {
		t.Fatalf("permanent failure burned %d attempts", calls.Load())
	}
	s.shutdown(context.Background())
	var park []string
	for _, r := range logs.all() {
		if r.level == slog.LevelWarn && strings.Contains(r.msg, "permanent error") {
			park = append(park, r.msg)
		}
	}
	if len(park) != 1 || !strings.Contains(park[0], "FileNotFoundError") {
		t.Fatalf("park warnings %v\n%s", park, logs.text())
	}
}

func TestFailureInitialAuthFailureParksAndRevivesAfterRelogin(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	writeServersConfig(t, "  figma:\n    command: x\n")
	parkedRetryInterval = 50 * time.Millisecond
	logs := captureLogs(t)
	var authenticated atomic.Bool
	s, calls := runnerTask(t, "figma", map[string]any{"command": "x"}, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if !authenticated.Load() {
			return "", errors.Join(context.Canceled, &HTTPStatusError{Status: 401})
		}
		fakeConnected(s, true)
		<-s.reconnect.Wait()
		return s.finishLifecycleWait(), nil
	})
	_ = s.start(context.Background(), s.rendered)
	waitUntil(t, "auth failure never parked", s.parked)
	if calls.Load() != 1 || taskDone(s) {
		t.Fatalf("calls %d, run exited %v", calls.Load(), taskDone(s))
	}
	authenticated.Store(true)
	waitUntil(t, "parked server never recovered after re-authentication", func() bool { return s.ready.IsSet() && !s.parked() && calls.Load() > 1 })
	s.shutdown(context.Background())
	if n := logs.count("failed initial authentication", "icebath mcp login figma"); n != 1 {
		t.Fatalf("auth warnings %d\n%s", n, logs.text())
	}
}
