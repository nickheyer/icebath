package lockfile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Child process mode, holding the lock named by the environment until stdin closes
func TestMain(m *testing.M) {
	if path := os.Getenv("LOCKFILE_HOLD"); path != "" {
		l, err := Try(path)
		if err != nil {
			os.Exit(3)
		}
		os.Stdout.Write([]byte("held\n"))
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		l.Release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Starts a child that holds the lock, returning a closer that lets it go
func hold(t *testing.T, path string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "LOCKFILE_HOLD="+path)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil || string(buf) != "held\n" {
		cmd.Process.Kill()
		t.Fatalf("child did not take the lock: %v %q", err, buf)
	}
	return func() {
		in.Close()
		cmd.Wait()
	}
}

func TestTryAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	release := hold(t, path)
	if _, err := Try(path); !errors.Is(err, ErrHeld) {
		t.Fatalf("Try while held: %v, want ErrHeld", err)
	}
	held, err := Held(path)
	if err != nil || !held {
		t.Fatalf("Held while held: %v %v", held, err)
	}
	release()
	l, err := Try(path)
	if err != nil {
		t.Fatalf("Try after release: %v", err)
	}
	held, err = Held(path)
	if err != nil || !held {
		t.Fatalf("Held while held through another descriptor of this process: %v %v", held, err)
	}
	l.Release()
	held, err = Held(path)
	if err != nil || held {
		t.Fatalf("Held after release: %v %v", held, err)
	}
}

func TestHeldMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.lock")
	held, err := Held(path)
	if err != nil || held {
		t.Fatalf("Held of a missing file: %v %v", held, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Held created the file")
	}
}

func TestAcquireBlocksUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	release := hold(t, path)
	done := make(chan *Lock, 1)
	go func() {
		l, err := Acquire(path)
		if err != nil {
			t.Error(err)
		}
		done <- l
	}()
	select {
	case <-done:
		t.Fatal("Acquire returned while another process held the lock")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case l := <-done:
		l.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not return after release")
	}
}
