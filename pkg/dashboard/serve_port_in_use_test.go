package dashboard

import (
	"context"
	"net"
	"syscall"
	"testing"
)

func TestProbeDetectsHeldSocket(t *testing.T) {
	holder, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if !portBindConflict("127.0.0.1", holder.Addr().(*net.TCPAddr).Port) {
		t.Fatal("a held port must conflict")
	}
}

func TestProbeFreePortIsClean(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	if portBindConflict("127.0.0.1", port) {
		t.Fatal("a free port must not conflict")
	}
}

func TestProbeSkipsEphemeralPortZero(t *testing.T) {
	if portBindConflict("127.0.0.1", 0) {
		t.Fatal("port zero never conflicts")
	}
}

func TestAddrInUseErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		errno syscall.Errno
		want  bool
	}{{syscall.EADDRINUSE, true}, {98, true}, {10048, true}, {syscall.EACCES, false}} {
		if got := isAddrInUseError(tc.errno); got != tc.want {
			t.Fatalf("errno %d: got %v, want %v", tc.errno, got, tc.want)
		}
	}
}

func TestGatewayListenerCompressesOnlyForRemotePeers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	noop := func(context.Context, *Scope, Receive, Send) error { return nil }
	for _, tc := range []struct {
		host         string
		authRequired bool
		deflate      bool
	}{{"127.0.0.1", false, false}, {"0.0.0.0", true, true}, {"127.0.0.1", true, true}} {
		cfg, _ := BuildListenerServer(context.Background(), noop, tc.host, 0, ListenerOptions{AuthRequired: tc.authRequired})
		if cfg.WSPerMessageDeflate != tc.deflate {
			t.Fatalf("%s auth=%v: deflate %v, want %v", tc.host, tc.authRequired, cfg.WSPerMessageDeflate, tc.deflate)
		}
	}
}
