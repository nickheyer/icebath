package updaterecord

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func psHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func TestTheRecordMutexExcludesOtherThreadsOfThisProcess(t *testing.T) {
	psHome(t)
	held, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, done, err := PauseMutex(context.Background(), PauseMutexWaitSeconds*time.Second)
		if err != nil {
			t.Error(err)
			close(held)
			return
		}
		close(held)
		<-release
		done()
	}()
	<-held
	_, done, err := PauseMutex(context.Background(), 300*time.Millisecond)
	if err == nil {
		done()
		t.Fatal("rode on the other goroutine's hold")
	}
	if !IsPauseRecordError(err, "RecordBusy") {
		t.Fatalf("err %v", err)
	}
	close(release)
	wg.Wait()
	ctx, outer, err := PauseMutex(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, inner, err := PauseMutex(ctx, time.Second)
	if err != nil {
		t.Fatalf("re-entry refused: %v", err)
	}
	inner()
	outer()
}

func TestPauseRecordReadAbsentMalformedAndPathShape(t *testing.T) {
	h := psHome(t)
	path := PauseRecordPath()
	if filepath.Dir(path) != h || !strings.HasPrefix(filepath.Base(path), PauseRecordStem+".") || len(pauseInstallKey("")) != 12 {
		t.Fatalf("path %s", path)
	}
	if body, err := ReadPauseRecord(""); body != nil || err != nil {
		t.Fatalf("absent record %v %v", body, err)
	}
	for _, text := range []string{"{torn", `{"token": []}`, "[]"} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPauseRecord(path); !IsPauseRecordError(err, "RecordUnknown") || !strings.Contains(err.Error(), "is malformed; fix or remove it to resume") {
			t.Fatalf("%q: %v", text, err)
		}
	}
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf"+`{"token": {"pause_id": "p"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if body, err := ReadPauseRecord(path); err != nil || body["token"].(map[string]any)["pause_id"] != "p" {
		t.Fatalf("body %v %v", body, err)
	}
}

func psMarker(pid, stopper int64) *pyjson.Object {
	m := pyjson.NewObject()
	m.Set("target_pid", pid)
	m.Set("stopper_pid", stopper)
	return m
}

func TestPauseMarkStopConsumedCheckpointsOnlyTheMatchingRequest(t *testing.T) {
	h := psHome(t)
	markerPath := filepath.Join(h, "gateway-planned-stop.json")
	token := map[string]any{"pause_id": "p1", "stopper_pid": int64(77), "stop_requested": []any{"41"},
		"stop_sent": []any{"9"}, "stop_markers": map[string]any{"41": markerPath}}
	body := map[string]any{"schema": int64(1), "owner": PauseIdentity(0), "install_root": PauseInstallRoot(), "token": token}
	if err := pauseAtomicWrite(PauseRecordPath(), body); err != nil {
		t.Fatal(err)
	}
	claim := PauseRecordPath() + ".123.claim"
	foreign := map[string]any{"install_root": "/elsewhere", "token": map[string]any{"pause_id": "p2", "stopper_pid": int64(77),
		"stop_requested": []any{"41"}, "stop_markers": map[string]any{"41": markerPath}}}
	if err := pauseAtomicWrite(claim, foreign); err != nil {
		t.Fatal(err)
	}
	same := func(a, b string) (bool, error) { return filepath.Clean(a) == filepath.Clean(b), nil }
	if err := MarkStopConsumed(markerPath, psMarker(41, 78), same); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadPauseRecord(""); got["token"].(map[string]any)["stop_sent"].([]any)[0] != "9" || len(got["token"].(map[string]any)["stop_sent"].([]any)) != 1 {
		t.Fatalf("another stopper checkpointed: %v", got["token"])
	}
	if err := MarkStopConsumed(markerPath, psMarker(41, 77), same); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadPauseRecord("")
	if sent := got["token"].(map[string]any)["stop_sent"]; !reflect.DeepEqual(sent, []any{"41", "9"}) {
		t.Fatalf("stop_sent %v", sent)
	}
	if other, _ := ReadPauseRecord(claim); other["token"].(map[string]any)["stop_sent"] != nil {
		t.Fatal("another install's claim was checkpointed")
	}
	if err := MarkStopConsumed(markerPath, pyjson.NewObject(), same); err == nil || err.Error() != "'target_pid'" {
		t.Fatalf("missing target pid: %v", err)
	}
}

func TestPauseMarkStopAcceptedBesideTheMarkerElseStampedInPlace(t *testing.T) {
	h := psHome(t)
	markerPath := filepath.Join(h, "gateway-planned-stop.json")
	if err := MarkStopAccepted(markerPath, psMarker(41, 77)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(PauseAcceptedPath(markerPath))
	if err != nil || !strings.Contains(string(raw), `"target_pid": 41`) {
		t.Fatalf("accepted receipt %q %v", raw, err)
	}
	if os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(h, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	inPlace := filepath.Join(locked, "gateway-planned-stop.json")
	if err := os.WriteFile(inPlace, []byte(`{"target_pid": 41, "stopper_pid": 77, "padding": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := MarkStopAccepted(inPlace, psMarker(41, 77)); err != nil {
		t.Fatal(err)
	}
	stamped, _ := os.ReadFile(inPlace)
	if string(stamped) != `{"target_pid": 41, "stopper_pid": 77, "accepted": true}` {
		t.Fatalf("stamped %q", stamped)
	}
}
