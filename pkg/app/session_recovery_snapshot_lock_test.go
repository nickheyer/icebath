package app

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestSnapshotBlocksConnectionsOpenedDuringTheCopy(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	tmp := t.TempDir()
	source := filepath.Join(tmp, "racy-state.db")
	snapshotDir := filepath.Join(tmp, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recoveryMakeSource(t, source)
	insideCopy, releaseCopy := make(chan struct{}), make(chan struct{})
	connectAttempted, connectionOpened := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var errs []string
	fail := func(text string) { mu.Lock(); errs = append(errs, text); mu.Unlock() }
	var insideOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCopy) }) }
	swap(t, &recoveryCopy2, func(src, dst string) error {
		err := pyfs.Copy2(src, dst)
		if strings.HasSuffix(src, "racy-state.db") {
			insideOnce.Do(func() { close(insideCopy) })
			select {
			case <-releaseCopy:
			case <-time.After(30 * time.Second):
			}
		}
		return err
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, _, err := recoveryCopySourceBundle(source, snapshotDir); err != nil {
			fail("copy failed: " + err.Error())
		}
	}()
	defer func() {
		release()
		wg.Wait()
	}()
	select {
	case <-insideCopy:
	case <-time.After(30 * time.Second):
		t.Fatal("copy never reached the patched operation")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(connectAttempted)
		conn, err := state.ConnectTracked(source, state.WithTimeout(30))
		if err != nil {
			fail("connect failed: " + err.Error())
			return
		}
		close(connectionOpened)
		conn.Close()
	}()
	<-connectAttempted
	select {
	case <-connectionOpened:
		t.Fatal("ConnectTracked completed while raw copy descriptors were open, the guard is not holding the lifecycle lock across the copy")
	case <-time.After(time.Second):
	}
	release()
	select {
	case <-connectionOpened:
	case <-time.After(30 * time.Second):
		t.Fatal("ConnectTracked never completed after the copy released the lock")
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatal(errs[0])
	}
}
