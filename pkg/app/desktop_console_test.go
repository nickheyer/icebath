package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/logging"
)

// Records every log record the desktop logger emits at or above its level
type desktopCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *desktopCapture) Handle(_ context.Context, rec *logging.Record) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, rec.Msg)
	return true
}
func (c *desktopCapture) Level() int   { return 0 }
func (c *desktopCapture) Close() error { return nil }

// Messages captured so far
func (c *desktopCapture) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.msgs...)
}

// Captures the desktop logger at a level for the test on a faked Windows host
func captureDesktopLog(t *testing.T, level int) *desktopCapture {
	t.Helper()
	swapGUISteps(t)
	gui.platform = func() string { return "win32" }
	logger := desktopLog()
	prev := logger.Level()
	logger.SetLevel(level)
	c := &desktopCapture{}
	logger.AddHandler(c)
	t.Cleanup(func() { logger.RemoveHandler(c); logger.SetLevel(prev) })
	return c
}

// Runs a helper child with its stdio drained into the desktop log and returns its exit status
func runDrained(t *testing.T, action string, extra ...string) int {
	t.Helper()
	argv, env := helperChild(action, extra...)
	streams, err := desktopConsoleOutput(false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runGuiCommand(guiRun{Argv: argv, Env: env, StdoutFile: streams.Stdout, StderrFile: streams.Stderr, Timeout: 15e9})
	streams.Close()
	if err != nil {
		t.Fatal(err)
	}
	return res.Returncode
}

func TestPackagedConsoleOutputDrainsBothStreams(t *testing.T) {
	c := captureDesktopLog(t, logging.LevelInfo)
	var out bytes.Buffer
	desktopLaunchNotice(&out, "Starting Icebath", false)
	if out.Len() != 0 {
		t.Fatalf("packaged startup attempted a synchronous console write %q", out.String())
	}
	if code := runDrained(t, "flood"); code != 7 {
		t.Fatalf("exit %d", code)
	}
	xs, diagnostic := 0, false
	for _, m := range c.messages() {
		xs += strings.Count(m, "x")
		diagnostic = diagnostic || strings.Contains(m, "diagnostic")
	}
	if xs != 131072 || !diagnostic {
		t.Fatalf("x count %d diagnostic %v", xs, diagnostic)
	}
}

func TestStderrSurvivesLoggingThreshold(t *testing.T) {
	for _, level := range []int{logging.LevelWarning, logging.LevelError} {
		c := captureDesktopLog(t, level)
		if code := runDrained(t, "ordinary"); code != 7 {
			t.Fatalf("exit %d", code)
		}
		fatal, ordinary := false, false
		for _, m := range c.messages() {
			fatal = fatal || strings.Contains(m, "fatal-diagnostic")
			ordinary = ordinary || strings.Contains(m, "ordinary-output")
		}
		if !fatal || ordinary {
			t.Fatalf("level %d fatal %v ordinary %v", level, fatal, ordinary)
		}
	}
}

func TestCompleteRecordsReachRedactedLogs(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("ICEBATH_HOME", tmp)
			logging.ResetQueuedHandlers()
			t.Cleanup(logging.ResetQueuedHandlers)
			captureDesktopLog(t, logging.LevelInfo)
			secret := "synthetic-boundary-credential"
			var payload bytes.Buffer
			payload.WriteString(strings.Repeat(" ", 8192-len("API_KEY=")) + "API_KEY=" + secret + "\n")
			payload.WriteString(strings.Repeat("u", 8191) + "€\n")
			payload.WriteString("oversized-start API_KEY=" + secret + strings.Repeat("z", 262144))
			payload.WriteString("oversized-end\nrecovered\ntail €")
			payloadPath := filepath.Join(tmp, "child-output.bin")
			if err := os.WriteFile(payloadPath, payload.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			logDir, err := logging.SetupLogging(context.Background(), logging.Options{Home: tmp, LogLevel: "INFO", Mode: "gui", Force: true})
			if err != nil {
				t.Fatal(err)
			}
			if code := runDrained(t, "payload", "ICEBATH_DESKTOP_TEST_MARKER", payloadPath, "ICEBATH_DESKTOP_TEST_STREAM", stream); code != 0 {
				t.Fatalf("exit %d", code)
			}
			logging.FlushLogQueue()
			raw, err := os.ReadFile(filepath.Join(logDir, "agent.log"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(raw)
			if strings.Contains(text, secret) || !strings.Contains(text, strings.Repeat("u", 8191)+"€") ||
				strings.Contains(text, "�") || strings.Contains(text, "oversized-start") || strings.Contains(text, "oversized-end") ||
				!strings.Contains(text, "recovered") || !strings.Contains(text, "tail €") || !strings.Contains(strings.ToLower(text), "omitted") {
				t.Fatalf("agent.log %q", text[max(0, len(text)-600):])
			}
			guiRaw, err := os.ReadFile(filepath.Join(logDir, "gui.log"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(guiRaw), "recovered") || strings.Contains(string(guiRaw), secret) {
				t.Fatalf("gui.log %q", guiRaw)
			}
		})
	}
}

func TestSourceLaunchKeepsInteractiveStreams(t *testing.T) {
	streams, err := desktopConsoleOutput(true)
	if err != nil || streams.Stdout != nil || streams.Stderr != nil {
		t.Fatalf("streams %+v err %v", streams, err)
	}
}
