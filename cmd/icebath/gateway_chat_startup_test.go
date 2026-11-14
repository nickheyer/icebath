package main

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/cliui"
)

func TestInvalidOptionsAreReportedBeforeReadingProfile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	t.Cleanup(app.SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out, StdinTTY: func() bool { return false },
		StdoutTTY: func() bool { return false }}, errOut))
	if code := launchGatewayChat(&chatArgs{Worktree: true, OutputFormat: "text"}); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "--worktree") {
		t.Fatalf("stderr %q", errOut.String())
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://127.0.0.1:"+strconv.Itoa(port)+"/api/ws")
	errOut.Reset()
	if code := launchGatewayChat(&chatArgs{SafeMode: true, Model: "explicit", Provider: "custom", Query: "x", OutputFormat: "text"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "no local fallback") {
		t.Fatalf("stderr %q", errOut.String())
	}
}
