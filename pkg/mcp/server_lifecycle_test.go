package mcp

import (
	"context"
	"encoding/json"
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

func TestServerStdioHangingInitializeIsBounded(t *testing.T) {
	isolateHome(t)
	s := mockStdioTask(t, "leak-guard", map[string]any{"MOCK_MCP_HANG_INIT": "1"})
	s.rendered.Config["connect_timeout"] = 0.2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := s.runStdio(ctx, s.rendered)
	elapsed := time.Since(start)
	if err == nil || !isTimeout(err) || ctx.Err() != nil {
		t.Fatalf("hanging initialize must fail with the connect timeout, got %v (outer ctx %v)", err, ctx.Err())
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("runStdio blocked %s on a hanging initialize", elapsed)
	}
	s.mu.Lock()
	pids := append([]int(nil), s.stdioChildPIDs...)
	s.mu.Unlock()
	if len(pids) != 1 {
		t.Fatalf("child pids %v", pids)
	}
	mcpMu.Lock()
	_, ledgered := stdioPIDs[pids[0]]
	mcpMu.Unlock()
	if ledgered || pidExists(pids[0]) {
		t.Fatalf("timed out child leaked: ledgered=%v alive=%v", ledgered, pidExists(pids[0]))
	}
}

func TestServerStdioReplacesInvalidUTF8(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	s := mockStdioTask(t, "test-encoding", map[string]any{"MOCK_MCP_BAD_UTF8": "1"})
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	defer s.shutdown(context.Background())
	s.mu.Lock()
	list := append([]*Tool(nil), s.tools...)
	s.mu.Unlock()
	if len(list) != 1 || list[0].Description != "ok��!" {
		t.Fatalf("undecodable bytes must become one U+FFFD per maximal subpart: %+v", list)
	}
}

func TestServerRetryAttemptsLogDebugTransitionsWarn(t *testing.T) {
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

func TestServerInitialRetryAttemptsLogDebug(t *testing.T) {
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

func TestServerStdioChildSeesDocumentedEnv(t *testing.T) {
	isolateHome(t)
	setExactEnv(t, map[string]string{"PATH": "/usr/bin:/bin", "TERM": "parent-term", "XDG_CONFIG_HOME": "/parent/config",
		"SET_DIR": "/opt/set", "EMPTY": "", "ACTUAL_LITERAL": "${STILL_UNSET}", "ICEBATH_HOME": os.Getenv("ICEBATH_HOME")})
	userEnv := map[string]any{"OPT_DIR": "${OPT_DIR}", "OPT_ENV": "${env:OPT_ENV}", "EMPTY": "${EMPTY}", "SET_DIR": "${SET_DIR}",
		"PARTIAL": "${OPT_DIR}/bin", "PLAIN": "x", "XDG_CONFIG_HOME": "${OPT_CONFIG_HOME}", "TERM": "${OPT_TERM}", "RESOLVED_LITERAL": "${ACTUAL_LITERAL}"}
	ctx := context.Background()
	r, err := renderServerConfig(ctx, nil, map[string]any{"command": mockBinary(t), "env": userEnv})
	if err == nil {
		r, err = r.Rerender(ctx, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	command, env, _, err := stdioLaunch(ctx, nil, r)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := StartStdio(ctx, StdioParams{Command: command, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(tr, SessionOptions{Name: "env"})
	defer sess.Close()
	if _, err := sess.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := sess.CallTool(ctx, "env", map[string]any{}, nil)
	if err != nil || len(res.Content) != 1 {
		t.Fatalf("env call %v %+v", err, res)
	}
	seen := map[string]string{}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &seen); err != nil {
		t.Fatal(err)
	}
	_, optDir := seen["OPT_DIR"]
	_, empty := seen["EMPTY"]
	if optDir || empty || seen["TERM"] != "parent-term" || seen["RESOLVED_LITERAL"] != "${STILL_UNSET}" ||
		seen["XDG_CONFIG_HOME"] != "/parent/config" || seen["PLAIN"] != "x" {
		t.Fatalf("child env %v", seen)
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
