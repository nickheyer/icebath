//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFileOpsAtomicWriteNewFileUmaskPermissions(t *testing.T) {
	for _, host := range []bool{false, true} {
		for _, mask := range []int{0o022, 0o002, 0o077} {
			dir := t.TempDir()
			ops := newShellFileOps(remoteTestEnv(t), bashFileOpsEnv(dir, host), dir)
			dest := filepath.Join(dir, "new_file.txt")
			old := syscall.Umask(mask)
			r := ops.WriteFile(dest, "test content\n", nil)
			syscall.Umask(old)
			if r.Error != "" {
				t.Fatalf("host=%v write failed %s", host, r.Error)
			}
			data, _ := os.ReadFile(dest)
			st, _ := os.Stat(dest)
			if string(data) != "test content\n" || int(st.Mode().Perm()) != 0o666&^mask {
				t.Fatalf("host=%v umask %04o gave %04o", host, mask, st.Mode().Perm())
			}
		}
	}
}

func TestFileOpsFifoReturnsNotRegularWithoutBlocking(t *testing.T) {
	for _, native := range []bool{false, true} {
		ops, env, dir := hostFileOps(t, native)
		fifo := filepath.Join(dir, "pipe")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Skip("mkfifo unavailable")
		}
		done := make(chan *ReadResult, 1)
		go func() { done <- ops.readFile(fifo, 1, 2000) }()
		select {
		case r := <-done:
			if !strings.Contains(r.Error, "not a regular file") {
				t.Fatalf("native=%v %+v", native, r)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("native=%v read blocked on a writer-less FIFO", native)
		}
		want := 1
		if native {
			want = 0
		}
		if n := len(envCommands(env)); n != want {
			t.Fatalf("native=%v calls %d", native, n)
		}
	}
}
