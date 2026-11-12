package whatsapp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/messagetools"
)

// Runs a helper role of the test binary, a port holder, a sleeper or a client
func runHelperRole(role string) int {
	parts := strings.Split(role, ":")
	switch parts[0] {
	case "holder":
		if parts[1] == "ignore" {
			signal.Ignore(syscall.SIGTERM)
		}
		ln, err := net.Listen("tcp4", "127.0.0.1:"+parts[2])
		if err != nil {
			fmt.Println("bind failed", err)
			return 3
		}
		fmt.Println("ready")
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		time.Sleep(60 * time.Second)
	case "sleep":
		secs, _ := strconv.ParseFloat(parts[1], 64)
		time.Sleep(time.Duration(secs * float64(time.Second)))
	case "client":
		c, err := net.Dial("tcp4", "127.0.0.1:"+parts[1])
		if err == nil {
			time.Sleep(200 * time.Millisecond)
			c.Close()
		}
	}
	return 0
}

// Test binary started in a helper role with optional extra argv and an optional node named executable
func spawnHelper(t *testing.T, role string, nodeNamedDir string, extra ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if nodeNamedDir != "" {
		link := filepath.Join(nodeNamedDir, "node")
		if err := os.Symlink(exe, link); err != nil {
			t.Fatal(err)
		}
		exe = link
	}
	cmd := exec.Command(exe, extra...)
	cmd.Env = append(os.Environ(), "ICEBATH_WA_TEST_ROLE="+role)
	return cmd
}

// Port holder process, bound and ready, ignoring SIGTERM when asked and named node when a dir is given
func spawnHolder(t *testing.T, ignoreTerm bool, port int, nodeNamedDir string) *procBridge {
	t.Helper()
	mode := "polite"
	if ignoreTerm {
		mode = "ignore"
	}
	cmd := spawnHelper(t, fmt.Sprintf("holder:%s:%d", mode, port), nodeNamedDir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("holder not ready: %q", line)
	}
	p := wrapProc(cmd)
	t.Cleanup(func() { p.stop() })
	return p
}

// Started helper process wrapped as a bridge handle
func startProc(t *testing.T, cmd *exec.Cmd) *procBridge {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := wrapProc(cmd)
	t.Cleanup(func() { p.stop() })
	return p
}

// A real child process seen through the bridge handle, the Popen of the Python cases
type procBridge struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

// Handle that reaps the process in the background and records its Popen style return code
func wrapProc(cmd *exec.Cmd) *procBridge {
	p := &procBridge{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if ok && ws.Signaled() {
			p.code = -int(ws.Signal())
		} else {
			p.code = cmd.ProcessState.ExitCode()
		}
		close(p.done)
	}()
	return p
}

func (p *procBridge) Pid() int { return p.cmd.Process.Pid }
func (p *procBridge) Poll() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}
func (p *procBridge) Terminate() { _ = p.cmd.Process.Signal(syscall.SIGTERM) }
func (p *procBridge) Kill()      { _ = p.cmd.Process.Kill() }
func (p *procBridge) Wait(timeout time.Duration) (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	case <-time.After(timeout):
		return 0, false
	}
}

// Kills the process when it still runs and waits for it
func (p *procBridge) stop() {
	if _, done := p.Poll(); !done {
		p.Kill()
	}
	<-p.done
}

// Bridge handle whose poll answers come from a function, the MagicMock process of the Python cases
type fakeProc struct {
	mu         sync.Mutex
	pid        int
	poll       func() (int, bool)
	terminates int
	kills      int
}

func (f *fakeProc) Pid() int { return f.pid }
func (f *fakeProc) Poll() (int, bool) {
	if f.poll == nil {
		return 0, false
	}
	return f.poll()
}
func (f *fakeProc) Terminate() { f.mu.Lock(); f.terminates++; f.mu.Unlock() }
func (f *fakeProc) Kill()      { f.mu.Lock(); f.kills++; f.mu.Unlock() }
func (f *fakeProc) Wait(time.Duration) (int, bool) {
	return f.Poll()
}

// Fake process exited with the code
func exitedProc(code int) *fakeProc {
	return &fakeProc{pid: 4190000, poll: func() (int, bool) { return code, true }}
}

// Free loopback port
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Waits until a bind probe agrees the port is held or free
func waitPortHeld(t *testing.T, port int, held bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for portIsFree(port) == held && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if portIsFree(port) == held {
		t.Fatalf("port %d never became held=%v", port, held)
	}
}

// Shortens the SIGTERM grace and the port free wait like the Python fast grace fixture
func fastGrace(t *testing.T) {
	t.Helper()
	grace, wait := sigtermGrace, bridgePortFreeTimeout
	sigtermGrace, bridgePortFreeTimeout = 1200*time.Millisecond, 1500*time.Millisecond
	t.Cleanup(func() { sigtermGrace, bridgePortFreeTimeout = grace, wait })
}

// Replaces the adapter sleep with an immediate return, the patched asyncio.sleep
func noSleep(t *testing.T) {
	t.Helper()
	saved := whatsappSleep
	whatsappSleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { whatsappSleep = saved })
}

// Replaces the bridge spawn for the test
func fakeSpawn(t *testing.T, fn func(args bridgeArgs, logFH *os.File, env map[string]string) bridgeProc) {
	t.Helper()
	saved := spawnBridgeProcess
	spawnBridgeProcess = func(ctx context.Context, args bridgeArgs, logFH *os.File, env map[string]string) bridgeProc {
		return fn(args, logFH, env)
	}
	t.Cleanup(func() { spawnBridgeProcess = saved })
}

// Adapter built without the constructor, the __new__ adapter of the Python cases
func newBareAdapter(t *testing.T, script, session string, port int) *WhatsAppAdapter {
	t.Helper()
	if !strings.HasPrefix(os.Getenv("ICEBATH_HOME"), os.TempDir()) {
		hermeticHome(t)
	}
	cfg := &gateway.PlatformConfig{Extra: map[string]any{}}
	abs, _ := filepath.Abs(session)
	a := &WhatsAppAdapter{bridgePort: port, bridgeScript: script, sessionPath: abs, profileHome: os.Getenv("ICEBATH_HOME")}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformWhatsApp, a)
	a.WhatsAppBehaviorMixin = NewWhatsAppBehaviorMixin(a.BasePlatformAdapter.Name(), cfg)
	a.DMPolicy, a.GroupPolicy = "pairing", "pairing"
	a.AllowFrom, a.GroupAllowFrom = map[string]bool{}, map[string]bool{}
	flushStatus(t)
	return a
}

// Hermetic profile home for one case, logging to stderr so no log file writer outlives the case
func hermeticHome(t *testing.T) string {
	t.Helper()
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	h := isolatedWhatsAppHome(t)
	flushStatus(t)
	return h
}

// Drains the asynchronous runtime status writer before the temp dirs made so far are removed
func flushStatus(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { gateway.FlushRuntimeStatus(5 * time.Second) })
}

// Error unless the condition holds within the timeout
func eventually(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return errors.New("condition never held")
}
