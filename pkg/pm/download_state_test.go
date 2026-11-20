package pm

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCollectPartialsRespectsOwnershipAndRecentResume(t *testing.T) {
	for _, state := range []string{"held", "recent-part", "recent-ranges", "unused"} {
		t.Run(state, func(t *testing.T) {
			tmp := t.TempDir()
			partials := filepath.Join(tmp, "partials")
			part := filepath.Join(partials, "example.part")
			side := filepath.Join(partials, "example.ranges")
			writeFile(t, part, []byte("partial"), 0o666)
			writeFile(t, side, []byte("[]"), 0o666)
			old := time.Now().Add(-10 * 24 * time.Hour)
			for _, path := range []string{part, side} {
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(state, "recent-") {
				recent := part
				if state == "recent-ranges" {
					recent = side
				}
				now := time.Now()
				if err := os.Chtimes(recent, now, now); err != nil {
					t.Fatal(err)
				}
			}
			release := func() {}
			if state == "held" {
				held, acquired, err := PartialLock(partials, "example", nil, true)
				if err != nil || !acquired {
					t.Fatalf("lock acquired %v error %v", acquired, err)
				}
				release = held
			}
			if err := CollectPartials(partials, GCGraceSeconds); err != nil {
				t.Fatal(err)
			}
			if dltExists(part) != (state != "unused") || dltExists(side) != (state != "unused") {
				t.Fatalf("part kept %v ranges kept %v", dltExists(part), dltExists(side))
			}
			release()
			if state == "held" {
				lock := filepath.Join(partials, ".locks", "example")
				if !dltIsFile(lock) {
					t.Fatal("lock inode removed on release")
				}
				if err := CollectPartials(partials, GCGraceSeconds); err != nil {
					t.Fatal(err)
				}
				if dltExists(part) || dltExists(side) {
					t.Fatal("released expired partial survived collection")
				}
				if !dltIsFile(lock) {
					t.Fatal("collection removed the lock inode")
				}
			}
		})
	}
}

func TestPauseInterruptsAnOwnerWait(t *testing.T) {
	tmp := t.TempDir()
	destination := filepath.Join(tmp, "already-present")
	writeFile(t, destination, []byte("complete"), 0o666)
	download := NewDownload([]Source{{URL: "https://example.invalid/model", Dest: destination}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")})
	waiting, finished := make(chan struct{}), make(chan struct{})
	var waitingOnce sync.Once
	originalKey := downloadKey
	dltSwap(t, &downloadKey, func(d *Download, rawURL string) string {
		waitingOnce.Do(func() { close(waiting) })
		return originalKey(d, rawURL)
	})
	release, acquired, err := PartialLock(download.PartialsDir, originalKey(download, download.Sources[0].URL), nil, true)
	if err != nil || !acquired {
		t.Fatalf("lock acquired %v error %v", acquired, err)
	}
	var runErr error
	go func() {
		defer close(finished)
		_, runErr = download.Run(nil)
	}()
	reached := dltWaitClosed(waiting, 5*time.Second)
	download.Pause()
	stoppedWhileLocked := dltWaitClosed(finished, 5*time.Second)
	release()
	dltWaitClosed(finished, 5*time.Second)
	if !reached {
		t.Fatal("download never reached the ownership wait")
	}
	if !stoppedWhileLocked {
		t.Fatal("pause did not interrupt the ownership wait")
	}
	if !IsDownloadKind(runErr, DownloadWasPaused) {
		t.Fatalf("want a pause, got %v", runErr)
	}
	if string(readFile(t, destination)) != "complete" {
		t.Fatal("destination changed")
	}
}

func TestCollectPartialsIgnoresNamesWithoutAPathlibSuffix(t *testing.T) {
	tmp := t.TempDir()
	partials := filepath.Join(tmp, "partials")
	old := time.Now().Add(-10 * 24 * time.Hour)
	for _, name := range []string{".part", ".ranges", "example.part", "example.ranges"} {
		path := filepath.Join(partials, name)
		writeFile(t, path, []byte("partial"), 0o666)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := CollectPartials(partials, GCGraceSeconds); err != nil {
		t.Fatal(err)
	}
	if !dltExists(filepath.Join(partials, ".part")) || !dltExists(filepath.Join(partials, ".ranges")) {
		t.Fatal("a dot only name was taken for a partial")
	}
	if dltExists(filepath.Join(partials, "example.part")) || dltExists(filepath.Join(partials, "example.ranges")) {
		t.Fatal("expired partial survived collection")
	}
}
