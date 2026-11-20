package installidentity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

func TestConcurrentFirstUseReturnsOnePersistedIdentity(t *testing.T) {
	root := t.TempDir()
	values := make([]string, 64)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				value, err := ReadOrCreateInstallID(context.Background(), root)
				if err != nil {
					t.Error(err)
				}
				values[i] = value
			}
		}()
	}
	for i := range 64 {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	distinct := map[string]bool{}
	for _, v := range values {
		distinct[v] = true
	}
	if len(distinct) != 1 || values[0] == "" {
		t.Fatalf("values %v", distinct)
	}
	persisted, err := os.ReadFile(filepath.Join(root, "install_id"))
	if err != nil || strings.TrimSpace(string(persisted)) != values[0] {
		t.Fatalf("persisted %q %v", persisted, err)
	}
}

func TestExistingIdentitySurvivesReadOnlyRootOrRacingPublication(t *testing.T) {
	for _, transientReadFailure := range []bool{false, true} {
		root := t.TempDir()
		value := strings.Repeat("a", 32)
		path := filepath.Join(root, "install_id")
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
		if !transientReadFailure {
			got, err := ReadOrCreateInstallID(context.Background(), root)
			if err != nil || got != value {
				t.Fatalf("got %q %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(root, ".install_id.lock")); !os.IsNotExist(err) {
				t.Fatalf("read-only identity lookup must not acquire a writable lock: %v", err)
			}
			continue
		}
		lockFile, err := os.OpenFile(filepath.Join(root, ".install_id.lock"), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := pyfs.LockFile(lockFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(path); err == nil {
			t.Fatal("publication in progress must fail the first read")
		}
		done := make(chan string, 1)
		go func() {
			got, err := ReadOrCreateInstallID(context.Background(), root)
			if err != nil {
				t.Error(err)
			}
			done <- got
		}()
		time.Sleep(100 * time.Millisecond)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := pyfs.UnlockFile(lockFile); err != nil {
			t.Fatal(err)
		}
		if got := <-done; got != value {
			t.Fatalf("got %q", got)
		}
		lockFile.Close()
	}
}

func TestConcurrentCorruptFileRepairReturnsOneCommittedIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "install_id"), []byte("corrupt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	barrier := filepath.Join(t.TempDir(), "barrier")
	var outs []func() string
	for _, value := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		cmd, out, err := StartRace(root, value, barrier, "")
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, func() string {
			if err := cmd.Wait(); err != nil {
				t.Fatalf("exit %v", err)
			}
			return out.String()
		})
	}
	var returned []string
	for _, out := range outs {
		returned = append(returned, out())
	}
	persisted, err := os.ReadFile(filepath.Join(root, "install_id"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(persisted))
	if returned[0] != want || returned[1] != want {
		t.Fatalf("returned %v persisted %q", returned, want)
	}
}
