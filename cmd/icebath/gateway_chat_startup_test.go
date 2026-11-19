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
	if code := launchGatewayChat(&chatArgs{SafeMode: true, Query: "x", OutputFormat: "text"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "pass --model explicitly") {
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
	errOut.Reset()
	carried := &chatArgs{SafeMode: true, Model: "explicit", Provider: "custom", Query: "x", OutputFormat: "text", Worktree: true,
		NoRestoreCWD: true, RunBudget: 30, Verbose: true, Source: "my-integration"}
	if code := launchGatewayChat(carried); code != 1 || !strings.Contains(errOut.String(), "no local fallback") {
		t.Fatalf("worktree, budget, verbose and source launch must reach the gateway: exit %d stderr %q", code, errOut.String())
	}
}
