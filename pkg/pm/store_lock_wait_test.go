package pm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Writer safe to read while another goroutine writes into it
type slwBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *slwBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *slwBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestInstallLockReportsWhatItWaitsOn(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "store"))
	if err := os.Mkdir(store.Root, 0o777); err != nil {
		t.Fatal(err)
	}
	stderr := &slwBuffer{}
	saved := installLockWaitOutput
	installLockWaitOutput = stderr
	t.Cleanup(func() { installLockWaitOutput = saved })
	holder, err := os.OpenFile(filepath.Join(store.Root, ".install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := LockFD(holder, false, 0); err != nil || !held {
		t.Fatalf("holder could not take the lock: %v %v", held, err)
	}
	entered := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		release, err := store.InstallLock()
		if err != nil {
			t.Error(err)
			return
		}
		close(entered)
		release()
	}()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(stderr.String(), "waiting for") && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(stderr.String(), filepath.Join(store.Root, ".install.lock")) {
		t.Fatalf("message does not name the lock: %q", stderr.String())
	}
	select {
	case <-entered:
		t.Fatal("the waiter entered while the lock was held")
	default:
	}
	holder.Close()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the message must not replace the wait")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
	}
}
