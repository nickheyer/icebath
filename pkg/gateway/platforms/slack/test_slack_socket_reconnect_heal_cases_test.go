package slack

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter with an app and app token pointed at the loopback Slack, set up by before and its socket task started
func attachedSocketAdapter(t *testing.T, before ...func(*SlackAdapter)) (*SlackAdapter, *fakeSlackAPI) {
	t.Helper()
	f := newFakeSlackAPI(t)
	a := NewSlackAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Token: "xoxb-fake"})
	a.app = &slackApp{client: newWebClient("xoxb-fake", "")}
	a.appToken = "xapp-fake"
	a.SetRunning(true)
	for _, setup := range before {
		setup(a)
	}
	if err := a.startSocketModeHandler(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.NextConn(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.SetRunning(false); a.stopSocketModeHandler() })
	return a, f
}

// Asserts teardown left no live socket task, no open socket and no retry against the workspace
func assertSocketTornDown(t *testing.T, f *fakeSlackAPI, task *socketTask) {
	t.Helper()
	opens, conns := len(f.CallsTo("apps.connections.open")), len(f.Conns())
	time.Sleep(30 * time.Millisecond)
	if !task.isDone() {
		t.Fatal("the old socket task outlived teardown")
	}
	for _, c := range f.Conns() {
		select {
		case <-c.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("a Socket Mode connection outlived teardown")
		}
	}
	if n := len(f.CallsTo("apps.connections.open")); n != opens {
		t.Fatalf("the old socket task retried %d time(s) after teardown", n-opens)
	}
	if n := len(f.Conns()); n != conns {
		t.Fatalf("the old socket task redialed %d time(s) after teardown", n-conns)
	}
}

// test_slack_socket_reconnect_heal.py TestSocketModeTeardown.test_socket_task_stops_before_session_is_closed
func TestSocketModeTeardownSocketTaskStopsBeforeSessionIsClosed(t *testing.T) {
	a, f := attachedSocketAdapter(t)
	task := a.socketModeTask
	a.stopSocketModeHandler()
	assertSocketTornDown(t, f, task)
}

// test_slack_socket_reconnect_heal.py TestSocketModeTeardown.test_client_tasks_are_dead_before_the_session_closes
func TestSocketModeTeardownClientTasksAreDeadBeforeTheSessionCloses(t *testing.T) {
	a, f := attachedSocketAdapter(t)
	conn := f.Conns()[0]
	if err := conn.Disconnect("refresh_requested"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.NextConn(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	task := a.socketModeTask
	a.stopSocketModeHandler()
	a.closeWorkspaceClients()
	assertSocketTornDown(t, f, task)
}

// test_slack_socket_reconnect_heal.py TestSocketModeRestart.test_watchdog_restarts_when_transport_disconnected
func TestSocketModeRestartWatchdogRestartsWhenTransportDisconnected(t *testing.T) {
	a, f := attachedSocketAdapter(t)
	conn := f.Conns()[0]
	barrier, err := conn.SendEvent("T_FAKE", map[string]any{"type": "icebath_barrier"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Ack(barrier, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	a.handler.setConnected(false)
	a.socketWatchdogInterval = 10 * time.Millisecond
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	done := make(chan error, 1)
	go func() { done <- a.socketWatchdogLoop(context.Background()) }()
	if _, err := f.NextConn(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	a.SetRunning(false)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog kept running")
	}
	slog.SetDefault(prev)
	var reasons []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if i := strings.Index(line, "Socket Mode unhealthy ("); i >= 0 {
			reasons = append(reasons, strings.SplitN(line[i+len("Socket Mode unhealthy ("):], ")", 2)[0])
		}
	}
	if len(reasons) != 1 || reasons[0] != "transport disconnected" {
		t.Fatalf("reasons = %v", reasons)
	}
}
