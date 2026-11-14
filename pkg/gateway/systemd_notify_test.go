//go:build linux

package gateway

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
)

// Bound datagram receiver at name, closed with the test
func notifyReceiver(t *testing.T, name string) *net.UnixConn {
	t.Helper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestNotifySupportsSystemdAbstractSocket(t *testing.T) {
	receiver := notifyReceiver(t, "@icebath-test-notify-"+filepath.Base(t.TempDir()))
	t.Setenv("NOTIFY_SOCKET", receiver.LocalAddr().String())
	if !SystemdNotify("WATCHDOG=1") {
		t.Fatal("notify returned false")
	}
	receiver.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4096)
	n, err := receiver.Read(buf)
	if err != nil || string(buf[:n]) != "WATCHDOG=1" {
		t.Fatalf("received %q, %v", buf[:n], err)
	}
}

func TestNotifyUsesNonblockingDatagramSend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	notifyReceiver(t, path)
	t.Setenv("NOTIFY_SOCKET", path)
	if !SystemdNotify("READY=1") {
		t.Fatal("first send failed")
	}
	payload := strings.Repeat("x", 4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100000 && SystemdNotify(payload); i++ {
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked on a full receiver buffer")
	}
}

func TestWatchdogSendsReadyHeartbeatAndStopping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	receiver := notifyReceiver(t, path)
	t.Setenv("NOTIFY_SOCKET", path)
	t.Setenv("WATCHDOG_USEC", "20000")
	watchdog := NewSystemdWatchdog(true, 1.0)
	ctx := pyctx.WithRunningLoop(context.Background())
	if !watchdog.Start(ctx) {
		t.Fatal("start returned false")
	}
	if !watchdog.Ready("Gateway running") {
		t.Fatal("ready returned false")
	}
	time.Sleep(40 * time.Millisecond)
	watchdog.Stop(ctx)
	var calls []string
	buf := make([]byte, 4096)
	for {
		receiver.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := receiver.Read(buf)
		if err != nil {
			break
		}
		calls = append(calls, string(buf[:n]))
	}
	ready, beat := false, false
	for _, message := range calls {
		ready = ready || strings.HasPrefix(message, "READY=1")
		beat = beat || message == "WATCHDOG=1"
	}
	if !ready || !beat || len(calls) == 0 || calls[len(calls)-1] != "STOPPING=1" {
		t.Fatalf("calls %q", calls)
	}
	if watchdog.Unhealthy() {
		t.Fatal("watchdog unhealthy")
	}
}
