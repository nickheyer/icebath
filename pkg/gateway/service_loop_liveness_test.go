package gateway

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Writes a heartbeat for pid aged ageS seconds with optional extra payload keys
func writeAgedHeartbeat(t *testing.T, h string, pid int, ageS float64, extra map[string]any) string {
	t.Helper()
	var ex *pyjson.Object
	if len(extra) > 0 {
		ex = pyjson.NewObject()
		for k, v := range extra {
			ex.Set(k, v)
		}
	}
	path := WriteLoopHeartbeat(context.Background(), &pid, nil, h, ex)
	if path == "" {
		path = GetLoopHeartbeatPath(context.Background(), h)
	}
	if ageS != 0 {
		stamp := time.Now().Add(-time.Duration(ageS * float64(time.Second)))
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Serves a tick witness at the address that answers 1 when answer is set, else stays silent
func serveWitness(t *testing.T, network, address string, answer bool) net.Listener {
	t.Helper()
	ln, err := net.Listen(network, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if answer {
				_, _ = c.Write([]byte("1"))
				c.Close()
				continue
			}
			go func() { time.Sleep(time.Second); c.Close() }()
		}
	}()
	return ln
}

// Home directly under the temp root, short enough for a unix socket path like the hermes fixture
func shortHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hwg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// Points the tick socket at a short path inside dir
func shortTickSocket(t *testing.T, dir string) string {
	sock := filepath.Join(dir, "t.sock")
	old := lifecycleTickSocketPath
	t.Cleanup(func() { lifecycleTickSocketPath = old })
	lifecycleTickSocketPath = func(context.Context, string, *int) string { return sock }
	return sock
}

func fp(v float64) *float64 { return &v }

func TestProbeGatewayLoopLivenessHeartbeatRules(t *testing.T) {
	ctx := context.Background()
	h := shortHome(t)
	shortTickSocket(t, h)
	opts := LoopLivenessOptions{Home: h, TickTimeoutS: 0.05}
	if got := ProbeGatewayLoopLiveness(ctx, 4242, opts); got != GatewayLoopUnknown {
		t.Fatalf("missing heartbeat = %s", got)
	}
	writeAgedHeartbeat(t, h, 4242, 5, nil)
	if got := ProbeGatewayLoopLiveness(ctx, 4242, opts); got != GatewayLoopAlive {
		t.Fatalf("fresh heartbeat = %s", got)
	}
	writeAgedHeartbeat(t, h, 4242, 60, nil)
	inside := opts
	inside.StaleAfterS = fp(90)
	if got := ProbeGatewayLoopLiveness(ctx, 4242, inside); got != GatewayLoopAlive {
		t.Fatalf("inside budget = %s", got)
	}
	writeAgedHeartbeat(t, h, 4242, 600, nil)
	if got := ProbeGatewayLoopLiveness(ctx, 4242, opts); got != GatewayLoopWedged {
		t.Fatalf("stale heartbeat = %s", got)
	}
	if got := ProbeGatewayLoopLiveness(ctx, 0, opts); got != GatewayLoopUnknown {
		t.Fatalf("nonpositive pid = %s", got)
	}
	writeAgedHeartbeat(t, h, 1111, 600, nil)
	if got := ProbeGatewayLoopLiveness(ctx, 4242, opts); got != GatewayLoopUnknown {
		t.Fatalf("pid mismatch = %s", got)
	}
	path := GetLoopHeartbeatPath(ctx, h)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ProbeGatewayLoopLiveness(ctx, 4242, opts); got != GatewayLoopUnknown {
		t.Fatalf("corrupt heartbeat = %s", got)
	}
	old := lifecycleHeartbeatPath
	t.Cleanup(func() { lifecycleHeartbeatPath = old })
	lifecycleHeartbeatPath = func(context.Context, string) string { return filepath.Join(h, "missing", "dir", "hb") }
	if got := ProbeGatewayLoopLiveness(ctx, 4242, opts); got != GatewayLoopUnknown {
		t.Fatalf("unreadable path = %s", got)
	}
}

func TestLoopTickWitnessContract(t *testing.T) {
	ctx := context.Background()
	opts := func(h string) LoopLivenessOptions {
		return LoopLivenessOptions{Home: h, TickTimeoutS: 0.05, TickGapS: fp(0)}
	}
	t.Run("answering witness beats a stale file", func(t *testing.T) {
		h := shortHome(t)
		serveWitness(t, "unix", shortTickSocket(t, h), true)
		writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_socket": true})
		if got := ProbeGatewayLoopLiveness(ctx, 4242, opts(h)); got != GatewayLoopAlive {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("true wedge requires sustained silence", func(t *testing.T) {
		h := shortHome(t)
		serveWitness(t, "unix", shortTickSocket(t, h), false)
		writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_socket": true})
		if got := ProbeGatewayLoopLiveness(ctx, 4242, opts(h)); got != GatewayLoopWedged {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("single silent probe is not destructive", func(t *testing.T) {
		h := shortHome(t)
		serveWitness(t, "unix", shortTickSocket(t, h), false)
		writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_socket": true})
		old := lifecycleProbeTickSocketOnce
		t.Cleanup(func() { lifecycleProbeTickSocketOnce = old })
		n := 0
		lifecycleProbeTickSocketOnce = func(context.Context, int, string, float64) tickWitness {
			n++
			if n == 1 {
				return tickSilent
			}
			return tickAnswered
		}
		if got := ProbeGatewayLoopLiveness(ctx, 4242, opts(h)); got != GatewayLoopAlive {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("witness vanishing mid window is unknown", func(t *testing.T) {
		h := shortHome(t)
		shortTickSocket(t, h)
		writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_socket": true})
		old := lifecycleProbeTickSocketOnce
		t.Cleanup(func() { lifecycleProbeTickSocketOnce = old })
		n := 0
		lifecycleProbeTickSocketOnce = func(context.Context, int, string, float64) tickWitness {
			n++
			if n == 1 {
				return tickSilent
			}
			return tickAbsent
		}
		if got := ProbeGatewayLoopLiveness(ctx, 4242, opts(h)); got != GatewayLoopUnknown {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("armed witness unreachable and unarmed witness are unknown", func(t *testing.T) {
		h := shortHome(t)
		shortTickSocket(t, h)
		writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_socket": true})
		if got := ProbeGatewayLoopLiveness(ctx, 4242, opts(h)); got != GatewayLoopUnknown {
			t.Fatalf("armed unreachable got %s", got)
		}
		writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_socket": false})
		if got := ProbeGatewayLoopLiveness(ctx, 4242, opts(h)); got != GatewayLoopUnknown {
			t.Fatalf("unarmed got %s", got)
		}
	})
	t.Run("fresh file with a silent node is unknown", func(t *testing.T) {
		h := shortHome(t)
		serveWitness(t, "unix", shortTickSocket(t, h), false)
		writeAgedHeartbeat(t, h, 4242, 5, nil)
		if got := ProbeGatewayLoopLiveness(ctx, 4242, opts(h)); got != GatewayLoopUnknown {
			t.Fatalf("got %s", got)
		}
	})
}

func TestLoopTickTCPWitness(t *testing.T) {
	ctx := context.Background()
	h := shortHome(t)
	shortTickSocket(t, h)
	o := LoopLivenessOptions{Home: h, TickTimeoutS: 0.05, TickGapS: fp(0)}
	answering := serveWitness(t, "tcp", "127.0.0.1:0", true)
	writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_tcp_port": int64(answering.Addr().(*net.TCPAddr).Port)})
	if got := ProbeGatewayLoopLiveness(ctx, 4242, o); got != GatewayLoopAlive {
		t.Fatalf("answering tcp got %s", got)
	}
	silent := serveWitness(t, "tcp", "127.0.0.1:0", false)
	port := int64(silent.Addr().(*net.TCPAddr).Port)
	writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_tcp_port": port})
	if got := ProbeGatewayLoopLiveness(ctx, 4242, o); got != GatewayLoopWedged {
		t.Fatalf("silent tcp got %s", got)
	}
	writeAgedHeartbeat(t, h, 4242, 5, map[string]any{"loop_tick_tcp_port": port})
	if got := ProbeGatewayLoopLiveness(ctx, 4242, o); got != GatewayLoopUnknown {
		t.Fatalf("fresh silent tcp got %s", got)
	}
	writeAgedHeartbeat(t, h, 4242, 600, map[string]any{"loop_tick_tcp_port": "garbage"})
	if got := ProbeGatewayLoopLiveness(ctx, 4242, o); got != GatewayLoopWedged {
		t.Fatalf("garbage port must fall back to the socket contract, got %s", got)
	}
}

func TestEscalateWedgedGateway(t *testing.T) {
	lifecycleHarness(t)
	var signals []string
	term := func(fail func(force bool) error) {
		lifecycleTerminatePid = func(pid int, force bool, _ any) error {
			if force {
				signals = append(signals, "kill")
			} else {
				signals = append(signals, "term")
			}
			if fail != nil {
				return fail(force)
			}
			return nil
		}
	}
	term(nil)
	lifecycleWaitForPIDExit = func(int, float64, func()) bool { return true }
	if !escalateWedgedGateway(4242, 5, 5) || len(signals) != 1 || signals[0] != "term" {
		t.Fatalf("sigterm grace: %v", signals)
	}
	signals = nil
	var waits []float64
	lifecycleWaitForPIDExit = func(_ int, timeout float64, _ func()) bool { waits = append(waits, timeout); return len(waits) > 1 }
	if !escalateWedgedGateway(4242, 5, 5) || len(signals) != 2 || signals[1] != "kill" {
		t.Fatalf("sigkill escalation: %v", signals)
	}
	waits = nil
	lifecycleWaitForPIDExit = func(_ int, timeout float64, _ func()) bool { waits = append(waits, timeout); return false }
	if escalateWedgedGateway(4242, escalateDefaultTermGraceS, escalateDefaultKillWaitS) {
		t.Fatal("never exited yet reported gone")
	}
	sum := 0.0
	for _, w := range waits {
		sum += w
	}
	if sum >= 30 {
		t.Fatalf("wait budget %v", sum)
	}
	term(func(bool) error { return errProcessGone })
	lifecycleWaitForPIDExit = func(int, float64, func()) bool { return true }
	if !escalateWedgedGateway(4242, 5, 5) {
		t.Fatal("gone process is success")
	}
	signals = nil
	term(func(force bool) error {
		if force {
			return os.ErrPermission
		}
		return nil
	})
	lifecycleWaitForPIDExit = func(int, float64, func()) bool { return false }
	if escalateWedgedGateway(4242, 5, 5) || len(signals) != 2 {
		t.Fatalf("permission error path: %v", signals)
	}
}
