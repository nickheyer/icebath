//go:build windows

package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func wptHelperEnv(mode string, extra map[string]string) map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	env["ICEBATH_PTY_TEST_HELPER"] = mode
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func wptSpawn(t *testing.T, argv []string, o PtySpawnOptions) *WinPtyBridge {
	t.Helper()
	tb, err := SpawnWinPtyBridge(context.Background(), nil, argv, o)
	if err != nil {
		t.Fatal(err)
	}
	return tb.(*WinPtyBridge)
}

func wptReadUntil(b *WinPtyBridge, needle []byte, timeout time.Duration) []byte {
	deadline := time.Now().Add(timeout)
	var buf []byte
	for time.Now().Before(deadline) {
		chunk, _ := b.Read(200 * time.Millisecond)
		if chunk == nil {
			break
		}
		buf = append(buf, chunk...)
		if bytes.Contains(buf, needle) {
			break
		}
	}
	return buf
}

func TestWinPtySpawnReturnsBridgeWithPid(t *testing.T) {
	b := wptSpawn(t, []string{"cmd.exe", "/c", "exit 0"}, PtySpawnOptions{Env: wptHelperEnv("", nil)})
	defer b.Close()
	if b.Pid() <= 0 {
		t.Fatalf("pid %d", b.Pid())
	}
}

func TestWinPtySpawnRaisesOnMissingArgv0(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "definitely-not-a-real-binary.exe")
	if _, err := SpawnWinPtyBridge(context.Background(), nil, []string{bogus}, PtySpawnOptions{Env: wptHelperEnv("", nil)}); err == nil {
		t.Fatal("missing binary spawned")
	}
}

func TestWinPtyWriteSendsToChildStdin(t *testing.T) {
	b := wptSpawn(t, []string{os.Args[0]}, PtySpawnOptions{Env: wptHelperEnv("readline", nil)})
	defer b.Close()
	if ok, err := b.Write(context.Background(), []byte("hello-pty\r\n"), PtyWriteTimeout, nil); !ok || err != nil {
		t.Fatalf("write %v %v", ok, err)
	}
	if out := wptReadUntil(b, []byte("GOT:hello-pty"), 10*time.Second); !bytes.Contains(out, []byte("GOT:hello-pty")) {
		t.Fatalf("output %q", out)
	}
}

func TestWinPtyPasteToAProgramThatIsNotReadingLandsAtOnce(t *testing.T) {
	b := wptSpawn(t, []string{os.Args[0]}, PtySpawnOptions{Env: wptHelperEnv("sleep-print", map[string]string{"ICEBATH_PTY_TEST_MS": "1500", "ICEBATH_PTY_TEST_TEXT": "SURVI" + "VED"})})
	defer b.Close()
	var paste bytes.Buffer
	for n := 0; n < 16384; n++ {
		fmt.Fprintf(&paste, "%05d %s\r", n, strings.Repeat("x", 57))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ok, err := b.Write(ctx, paste.Bytes(), PtyNoTimeout, nil); !ok || err != nil {
		t.Fatalf("paste %v %v", ok, err)
	}
	if out := wptReadUntil(b, []byte("SURVIVED"), 15*time.Second); !bytes.Contains(out, []byte("SURVIVED")) {
		t.Fatalf("output tail %q", out[max(0, len(out)-200):])
	}
}

func TestWinPtyIdleReadReturnsWithinItsTimeout(t *testing.T) {
	b := wptSpawn(t, []string{os.Args[0]}, PtySpawnOptions{Env: wptHelperEnv("print-sleep", map[string]string{"ICEBATH_PTY_TEST_MS": "30000", "ICEBATH_PTY_TEST_TEXT": "REA" + "DY"})})
	defer b.Close()
	if !bytes.Contains(wptReadUntil(b, []byte("READY"), 10*time.Second), []byte("READY")) {
		t.Fatal("never ready")
	}
	idle, slowest := false, time.Duration(0)
	deadline := time.Now().Add(5 * time.Second)
	for !idle && time.Now().Before(deadline) {
		started := time.Now()
		got, _ := b.Read(500 * time.Millisecond)
		idle = got != nil && len(got) == 0
		slowest = max(slowest, time.Since(started))
	}
	if !idle || slowest >= 2500*time.Millisecond {
		t.Fatalf("idle %v slowest %v", idle, slowest)
	}
}

func TestWinPtyReadReturnsNoneAfterChildExits(t *testing.T) {
	b := wptSpawn(t, []string{"cmd.exe", "/c", "echo done"}, PtySpawnOptions{Env: wptHelperEnv("", nil)})
	defer b.Close()
	wptReadUntil(b, []byte("done"), 10*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for b.IsAlive() && time.Now().Before(deadline) {
		b.Read(100 * time.Millisecond)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := b.Read(100 * time.Millisecond); got == nil {
			return
		}
	}
	t.Fatal("read did not return nil after child EOF")
}

func TestWinPtyResizeDoesNotRaiseOnLiveChild(t *testing.T) {
	b := wptSpawn(t, []string{os.Args[0]}, PtySpawnOptions{Cols: 80, Rows: 24, Env: wptHelperEnv("sleep-print", map[string]string{"ICEBATH_PTY_TEST_MS": "1000"})})
	defer b.Close()
	b.Resize(123, 45)
	if !b.IsAlive() {
		t.Fatal("child died on resize")
	}
}

func TestWinPtyResizeAfterCloseIsSilent(t *testing.T) {
	b := wptSpawn(t, []string{"cmd.exe", "/c", "exit 0"}, PtySpawnOptions{Env: wptHelperEnv("", nil)})
	b.Close()
	b.Resize(100, 40)
}

func TestWinPtyCloseTerminatesLongRunningChild(t *testing.T) {
	b := wptSpawn(t, []string{os.Args[0]}, PtySpawnOptions{Env: wptHelperEnv("print-sleep", map[string]string{"ICEBATH_PTY_TEST_MS": "30000"})})
	pid := b.Pid()
	if !b.IsAlive() {
		t.Fatalf("child pid %d not alive before close", pid)
	}
	b.Close()
	deadline := time.Now().Add(5 * time.Second)
	for b.child.IsAlive() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if b.IsAlive() || b.child.IsAlive() {
		t.Fatalf("pid %d alive after close", pid)
	}
}

func TestWinPtyCwdIsRespected(t *testing.T) {
	dir := t.TempDir()
	b := wptSpawn(t, []string{os.Args[0]}, PtySpawnOptions{Cwd: dir, Env: wptHelperEnv("cwd", nil)})
	defer b.Close()
	needle := []byte(strings.ToLower(dir))
	out := wptReadUntil(b, needle, 10*time.Second)
	if !bytes.Contains(bytes.ToLower(out), needle) {
		t.Fatalf("cwd %s not echoed, got %q", dir, out)
	}
}

func TestWinPtyEnvIsForwarded(t *testing.T) {
	b := wptSpawn(t, []string{os.Args[0]}, PtySpawnOptions{Env: wptHelperEnv("env", map[string]string{"ICEBATH_PTY_TEST_TEXT": "forwarded-value-123"})})
	defer b.Close()
	if out := wptReadUntil(b, []byte("forwarded-value-123"), 10*time.Second); !bytes.Contains(out, []byte("forwarded-value-123")) {
		t.Fatalf("output %q", out)
	}
}
