//go:build unix

package gatewaycli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSeedSuperviseSkeletonCreatesExpectedLayout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	svc := filepath.Join(t.TempDir(), "gateway-foo")
	if err := os.Mkdir(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := seedSuperviseSkeleton(svc); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"event", "supervise", "supervise/event"} {
		if st, err := os.Stat(filepath.Join(svc, rel)); err != nil || !st.IsDir() {
			t.Fatalf("missing %s/", rel)
		}
	}
	if st, _ := os.Stat(filepath.Join(svc, "supervise")); st.Mode().Perm() != 0o755 {
		t.Fatalf("supervise mode %o", st.Mode().Perm())
	}
	st, err := os.Stat(filepath.Join(svc, "supervise", "control"))
	if err != nil || st.Mode()&os.ModeNamedPipe == 0 || st.Mode().Perm() != 0o660 {
		t.Fatalf("control fifo %v %v", st, err)
	}
	for _, rel := range []string{"event", "supervise/event"} {
		var raw syscall.Stat_t
		if err := syscall.Stat(filepath.Join(svc, rel), &raw); err != nil {
			t.Fatal(err)
		}
		if mode := raw.Mode & 0o7777; mode != 0o3730 {
			t.Fatalf("%s mode %o want 3730", rel, mode)
		}
	}
}

func TestS6LogRunNeverInvokesChownWithSymlinkedLogDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink and /bin/sh required")
	}
	tmp := t.TempDir()
	h := filepath.Join(tmp, "icebath")
	leaf := filepath.Join(h, "logs", "gateways", "coder")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(tmp, "victim")
	os.Mkdir(victim, 0o755)
	os.WriteFile(filepath.Join(victim, "marker"), []byte("keep"), 0o644)
	os.WriteFile(filepath.Join(victim, "lock"), []byte("keep-lock"), 0o644)
	before, _ := os.Stat(victim)
	bin := filepath.Join(tmp, "bin")
	os.Mkdir(bin, 0o755)
	recorder := filepath.Join(tmp, "chown_calls.txt")
	scripts := map[string]string{
		"chown":        "#!/bin/sh\nprintf \"%s\\n\" \"$*\" >> \"" + recorder + "\"\nexit 0\n",
		"id":           "#!/bin/sh\nif [ \"$1\" = \"-u\" ]; then echo 0; exit 0; fi\nexit 1\n",
		"s6-setuidgid": "#!/bin/sh\nshift\nICEBATH_TEST_DROPPED=1 exec \"$@\"\n",
		"rm":           "#!/bin/sh\nif [ -n \"$ICEBATH_TEST_DROPPED\" ]; then\n  exit 0\nfi\nexec /bin/rm \"$@\"\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(tmp, "log_run_setup.sh")
	os.WriteFile(script, []byte(logRunSetupFragment(renderS6LogRun("coder"))), 0o755)
	stop := make(chan struct{})
	done := make(chan struct{})
	clear := func() {
		if st, err := os.Lstat(leaf); err == nil {
			if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
				os.Remove(leaf)
			} else {
				os.Remove(leaf)
			}
		}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			clear()
			if os.Symlink(victim, leaf) == nil {
				time.Sleep(time.Millisecond)
				clear()
				os.Mkdir(leaf, 0o755)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	env := append(os.Environ(), "ICEBATH_HOME="+h, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for i := 0; i < 40; i++ {
		cmd := exec.Command("/bin/sh", script)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			close(stop)
			<-done
			t.Fatalf("setup failed %v %s", err, out)
		}
		if st, err := os.Stat(filepath.Join(victim, "lock")); err != nil || !st.Mode().IsRegular() {
			close(stop)
			<-done
			t.Fatal("symlinked leaf let root unlink victim/lock")
		}
	}
	close(stop)
	<-done
	if raw, err := os.ReadFile(recorder); err == nil && strings.TrimSpace(string(raw)) != "" {
		t.Fatalf("chown invoked: %s", raw)
	}
	after, _ := os.Stat(victim)
	if after.Sys().(*syscall.Stat_t).Uid != before.Sys().(*syscall.Stat_t).Uid {
		t.Fatal("victim owner changed")
	}
	if m, _ := os.ReadFile(filepath.Join(victim, "marker")); string(m) != "keep" {
		t.Fatal("marker changed")
	}
	if l, _ := os.ReadFile(filepath.Join(victim, "lock")); string(l) != "keep-lock" {
		t.Fatal("lock changed")
	}
}
