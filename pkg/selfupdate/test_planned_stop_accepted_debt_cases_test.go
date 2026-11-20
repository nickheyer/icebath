//go:build !windows

package selfupdate

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Accepted stop the record could not checkpoint, aged past its TTL and orphaned, the acceptance kept beside the request
func paAcceptedUnrecorded(t *testing.T, homeWritable bool) (string, map[string]any) {
	t.Helper()
	marker, saved := seAcceptedThenOrphaned(t, homeWritable, true, false)
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	accepted, _ := loaded.(*pyjson.Object).Get("accepted")
	if homeWritable && accepted != nil || !homeWritable && accepted != true {
		t.Fatalf("premise: receipt vs stamped request, accepted %v", accepted)
	}
	if err := updaterecord.WritePause(context.Background(), saved, updaterecord.PauseUnowned(), ""); err != nil {
		t.Fatal(err)
	}
	return marker, saved
}

// Profiles the obligation still owes after a launch's recovery, nil once nothing carries it
func paOwed(t *testing.T, pauseID any) map[string]any {
	t.Helper()
	if err := updaterecord.RecoverPauses(context.Background(), []string{"status"}); err != nil {
		t.Fatal(err)
	}
	for _, src := range append([]string{updaterecord.PauseRecordPath()}, seClaims()...) {
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if token := seToken(t, src); pyrepr.Equal(token["pause_id"], pauseID) {
			return token["profiles"].(map[string]any)
		}
	}
	return nil
}

func paExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestAStampedAcceptedRequestOutlivesItsTtlWhileThePauseIsOwed(t *testing.T) {
	seSkipRoot(t)
	for _, secondLook := range []string{"second_signal", "watcher_probe"} {
		t.Run(secondLook, func(t *testing.T) {
			marker, saved := paAcceptedUnrecorded(t, false)
			if secondLook == "second_signal" {
				if ok, err := gateway.ConsumePlannedStopMarkerForSelf(); ok || err != nil {
					t.Fatalf("an expired request matches nobody: %v %v", ok, err)
				}
			} else if ok, err := gateway.PlannedStopMarkerTargetsSelf(); ok || err != nil {
				t.Fatalf("the watcher probe matched an expired request: %v %v", ok, err)
			}
			if !paExists(marker) {
				t.Fatalf("the %s deleted the stamped accepted request", secondLook)
			}
			if owed := paOwed(t, saved["pause_id"]); !seSameProfiles(t, owed) {
				t.Fatalf("an accepted stop lost its restart debt: %v", owed)
			}
		})
	}
}

func TestAcceptedStopEvidenceIsRemovedOnceItsPauseIsSettledAndNeverBefore(t *testing.T) {
	seSkipRoot(t)
	for _, homeWritable := range []bool{true, false} {
		t.Run(map[bool]string{true: "receipt", false: "stamped_request"}[homeWritable], func(t *testing.T) {
			marker, saved := paAcceptedUnrecorded(t, homeWritable)
			evidence := marker
			if homeWritable {
				evidence = updaterecord.PauseAcceptedPath(marker)
			}
			if _, err := gateway.ConsumePlannedStopMarkerForSelf(); err != nil {
				t.Fatal(err)
			}
			if !paExists(evidence) {
				t.Fatal("accepted-stop evidence removed while its pause is still owed")
			}
			if err := updaterecord.DischargePause(context.Background(), saved, ""); err != nil {
				t.Fatal(err)
			}
			if paExists(updaterecord.PauseRecordPath()) {
				t.Fatal("premise: the pause is settled")
			}
			if _, err := gateway.ConsumePlannedStopMarkerForSelf(); err != nil {
				t.Fatal(err)
			}
			if paExists(updaterecord.PauseAcceptedPath(marker)) {
				t.Fatal("a settled receipt was left behind")
			}
			if paExists(marker) {
				t.Fatal("a settled stamped request was left behind")
			}
		})
	}
}

func TestWithNoPauseOnDiskTheConsumerNeverWaitsOnThePauseLock(t *testing.T) {
	seSkipRoot(t)
	profile := t.TempDir() + "/root/profiles/p"
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profile)
	held, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, done, err := updaterecord.PauseMutex(context.Background(), updaterecord.PauseMutexWaitSeconds*time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer done()
		close(held)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}()
	var elapsed time.Duration
	marker := gateway.PlannedStopMarkerPath()
	func() {
		defer func() { close(release); wg.Wait() }()
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			t.Fatal("premise: another goroutine holds the pause lock")
		}
		if !gateway.WritePlannedStopMarker(context.Background(), os.Getpid()) {
			t.Fatal("premise: the request was written")
		}
		started := time.Now()
		ok, err := gateway.ConsumePlannedStopMarkerForSelf()
		elapsed = time.Since(started)
		if !ok || err != nil {
			t.Fatalf("consume = %v, %v", ok, err)
		}
	}()
	if elapsed >= time.Second {
		t.Fatalf("the consumer waited %.2fs on a pause lock no pause needed", elapsed.Seconds())
	}
	if paExists(marker) {
		t.Fatal("the consumed request was left on disk")
	}
	if paExists(updaterecord.PauseAcceptedPath(marker)) {
		t.Fatal("an .accepted receipt was written with no pause")
	}
}
