package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	mockOnce  sync.Once
	mockPath  string
	mockErr   error
	pkgDir, _ = os.Getwd()
)

// Builds the mock MCP server once per test binary
func mockBinary(t *testing.T) string {
	t.Helper()
	mockOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mcp-mock-")
		if err != nil {
			mockErr = err
			return
		}
		mockPath = filepath.Join(dir, "mockserver")
		build := exec.Command("go", "build", "-o", mockPath, "./testdata/mockserver")
		build.Dir = pkgDir
		out, err := build.CombinedOutput()
		if err != nil {
			mockErr = fmt.Errorf("building the mock server: %v: %s", err, out)
		}
	})
	if mockErr != nil {
		t.Fatal(mockErr)
	}
	return mockPath
}

// One captured log record
type logRecord struct {
	level slog.Level
	msg   string
}

// Log handler keeping every record for assertions
type captureHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, logRecord{level: r.Level, msg: r.Message})
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// Records captured so far
func (h *captureHandler) all() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]logRecord(nil), h.records...)
}

// Every captured message joined by newlines
func (h *captureHandler) text() string {
	var parts []string
	for _, r := range h.all() {
		parts = append(parts, r.msg)
	}
	return strings.Join(parts, "\n")
}

// Count of captured messages containing every needle
func (h *captureHandler) count(needles ...string) int {
	n := 0
	for _, r := range h.all() {
		ok := true
		for _, needle := range needles {
			if !strings.Contains(r.msg, needle) {
				ok = false
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// Routes the default logger into a capture handler for the test
func captureLogs(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// Isolated home with the MCP ledgers and caches emptied for the test
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	restoreEnviron(t)
	t.Setenv("ICEBATH_HOME", dir)
	resetLedgers(t)
	return dir
}

// Puts the process environment back to its pre test contents, dropping variables a saved secret published
func restoreEnviron(t *testing.T) {
	t.Helper()
	before := os.Environ()
	t.Cleanup(func() {
		keep := map[string]bool{}
		for _, kv := range before {
			k, v, _ := strings.Cut(kv, "=")
			keep[k] = true
			os.Setenv(k, v)
		}
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); !keep[k] {
				os.Unsetenv(k)
			}
		}
	})
}

// Empties every connection ledger and the cost tally, restoring emptiness after the test
func resetLedgers(t *testing.T) {
	t.Helper()
	clear := func() {
		mcpMu.Lock()
		for k := range mcpServers {
			delete(mcpServers, k)
		}
		for _, m := range []map[ServerKey]string{serverScopeKeys, serverConnectErrors, lazyServerFingerprints, serverTrustLevels} {
			for k := range m {
				delete(m, k)
			}
		}
		for k := range serverToolScopes {
			delete(serverToolScopes, k)
		}
		for k := range lazyServerConfigs {
			delete(lazyServerConfigs, k)
		}
		for k := range lazyServerToolNames {
			delete(lazyServerToolNames, k)
		}
		for k := range serverCostPolicies {
			delete(serverCostPolicies, k)
		}
		for k := range serverConnecting {
			delete(serverConnecting, k)
		}
		for k := range parallelSafeServers {
			delete(parallelSafeServers, k)
		}
		stdioPIDs, orphanStdioPIDs, orphanStdioPIDServers = map[int]string{}, map[int]bool{}, map[int]string{}
		stdioPGIDs, stdioStartTimes = map[int]int{}, map[int]int64{}
		mcpMu.Unlock()
		costsMu.Lock()
		spent = map[tallyKey]float64{}
		askLocks = map[tallyKey]*sync.Mutex{}
		unscopedMovedTo = ""
		costsMu.Unlock()
		whitespaceWarnedMu.Lock()
		whitespaceWarned = map[[2]string]bool{}
		whitespaceWarnedMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// Pointer to a float literal
func fptr(f float64) *float64 { return &f }

// Waits until the running loop has run every callback queued before this call
func flushLoop(t *testing.T) {
	t.Helper()
	l := runningLoop()
	if l == nil {
		return
	}
	done := make(chan struct{})
	if l.callSoon(func() { close(done) }) != nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop never ran its queued callbacks")
	}
}
