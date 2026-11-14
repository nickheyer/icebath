package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
)

// WARNING sample record rendered by the formatter the Go log files write through
func logsPythonLogLine(t *testing.T, loggerName string) string {
	t.Helper()
	f, err := logging.LogFileFormatter()
	if err != nil {
		t.Fatal(err)
	}
	line, err := f.Format(context.Background(), &logging.Record{Name: loggerName, Levelno: 30, Levelname: "WARNING", Msg: "sample", Created: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// Line the MCP stderr tee writes for one child stderr line
func logsMCPOutputLine(t *testing.T) string {
	t.Helper()
	var log strings.Builder
	tee, err := mcp.NewStderrTee(&log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tee.Sink.Write([]byte("server says hello\n")); err != nil {
		t.Fatal(err)
	}
	tee.Close(5 * time.Second)
	return log.String()
}

// Run banner the update output wrapper writes to the root home update.log
func logsUpdateLogLine(t *testing.T) string {
	t.Helper()
	t.Setenv("ICEBATH_UPDATE_POST_SWAP", "")
	state := gateway.InstallHangupProtection(io.Discard, io.Discard, true)
	gateway.FinalizeUpdateOutput(state)
	if !state.Installed {
		t.Fatal("update output wrapper did not open update.log")
	}
	raw, err := os.ReadFile(filepath.Join(home.Root(), "logs", "update.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimLeft(string(raw), " \t\n\r\v\f")
}

// Line of the desktop update hand-off shell log function
func logsHandoffLogLine(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("bash", "-c", `log() { echo "$(date +%Y-%m-%dT%H:%M:%S%z) $1"; }; log "update| → Checking if desktop app needs rebuilding..."`).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(out), "\n")
}

// One line per log file entry from that file's real writer
func logsFileSamples(t *testing.T) map[string]string {
	return map[string]string{
		"agent":   logsPythonLogLine(t, "run_agent"),
		"errors":  logsPythonLogLine(t, "run_agent"),
		"gateway": logsPythonLogLine(t, "gateway.run"),
		"gui":     logsPythonLogLine(t, "hermes_cli.web_server"),
		"desktop": "2026-09-28 13:18:46,062 [icebath] [boot] ready",
		"mcp":     logsMCPOutputLine(t),
		"update":  logsUpdateLogLine(t),
		"handoff": logsHandoffLogLine(t),
	}
}

func TestEveryLogFileWritesAStampHermesLogsSinceCanRead(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	samples := logsFileSamples(t)
	if len(samples) != len(logging.LogFiles) {
		t.Fatalf("samples %v for log files %v", samples, logging.LogFiles)
	}
	for name := range logging.LogFiles {
		if _, ok := samples[name]; !ok {
			t.Fatalf("add a real sample line for log file %q", name)
		}
	}
	for name, line := range samples {
		if _, ok := parseLogLineTimestamp(line); !ok {
			t.Fatalf("%s %q", name, line)
		}
	}
	if _, ok := parseLogLineTimestamp(gateway.StampLine("raw gateway stderr")); !ok {
		t.Fatal("stamped gateway stderr line carries no readable stamp")
	}
}
