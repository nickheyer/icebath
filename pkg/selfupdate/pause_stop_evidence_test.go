//go:build !windows

package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func seSkipRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
}

func seClaims() []string {
	found, _ := filepath.Glob(updaterecord.PauseRecordPath() + ".*.claim")
	sort.Strings(found)
	return found
}

func seToken(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := updaterecord.ReadPauseRecord(path)
	if err != nil || body == nil {
		t.Fatalf("pause record %s unreadable: %v", path, err)
	}
	return body["token"].(map[string]any)
}

// Pauses this process, has it accept its stop past a refused checkpoint, then ages and orphans the record
func seAcceptedThenOrphaned(t *testing.T, homeWritable, consume, markSent bool) (string, map[string]any) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	profile := filepath.Join(root, "profiles", "p")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profile)
	pm.SetCommandArgv(func(argv []string) []string { return argv })
	ctx := context.Background()
	pid := os.Getpid()
	marker := filepath.Join(profile, ".gateway-planned-stop.json")
	token, err := updaterecord.RecordPause(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"p": pid},
		"identities": map[string]any{strconv.Itoa(pid): updaterecord.PauseIdentity(pid)["ct"]}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := updaterecord.MarkStopRequested(ctx, token, []int{pid}, map[int]string{pid: marker}); err != nil {
		t.Fatal(err)
	}
	if !gateway.WritePlannedStopMarker(pid) {
		t.Fatal("premise: the request was written")
	}
	_ = os.Chmod(root, 0o555)
	if !homeWritable {
		_ = os.Chmod(profile, 0o555)
	}
	func() {
		defer func() { _ = os.Chmod(profile, 0o755); _ = os.Chmod(root, 0o755) }()
		if markSent {
			_ = updaterecord.MarkStopSent(ctx, token, pid)
		}
		if consume {
			if ok, err := gateway.ConsumePlannedStopMarkerForSelf(); !ok || err != nil {
				t.Fatalf("premise: the stop was accepted (%v, %v)", ok, err)
			}
		}
	}()
	saved := seToken(t, updaterecord.PauseRecordPath())
	if sent := saved["stop_sent"]; !reflect.DeepEqual(sent, []any{}) {
		t.Fatalf("premise: no checkpoint landed in the record, stop_sent %v", sent)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	body := loaded.(*pyjson.Object)
	body.Set("written_at", time.Now().UTC().Add(-120*time.Second).Format("2006-01-02T15:04:05.000000-07:00"))
	text, err := pyjson.Dumps(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return marker, saved
}

// Runs a launch's recovery and returns the profiles still owed by the obligation, nil once retired
func seOwedAfterRecovery(t *testing.T, pauseID any) map[string]any {
	t.Helper()
	if err := updaterecord.RecoverPauses(context.Background(), []string{"status"}); err != nil {
		t.Fatal(err)
	}
	path := updaterecord.PauseRecordPath()
	retired := strings.TrimSuffix(path, ".json") + ".retired"
	ids := []any{}
	if raw, err := os.ReadFile(retired); err == nil {
		loaded, _ := pyjson.Loads(string(raw))
		ids, _ = pyjson.Plain(loaded).(map[string]any)["ids"].([]any)
	}
	var owed []map[string]any
	for _, src := range append([]string{path}, seClaims()...) {
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if token := seToken(t, src); pyrepr.Equal(token["pause_id"], pauseID) {
			owed = append(owed, token["profiles"].(map[string]any))
		}
	}
	retiredHere := false
	for _, id := range ids {
		retiredHere = retiredHere || pyrepr.Equal(id, pauseID)
	}
	if (len(owed) > 0) == retiredHere {
		t.Fatalf("an obligation is either owed or retired, never both or neither: owed %v retired %v", owed, ids)
	}
	if len(owed) == 0 {
		return nil
	}
	return owed[0]
}

func seSameProfiles(t *testing.T, got map[string]any) bool {
	t.Helper()
	return len(got) == 1 && pyrepr.Equal(got["p"], os.Getpid())
}

func TestPauseAnUnreadableAcceptedStopReceiptKeepsTheDrainingGatewayOwed(t *testing.T) {
	seSkipRoot(t)
	for _, receipt := range []string{"refused", "malformed", "missing"} {
		t.Run(receipt, func(t *testing.T) {
			marker, saved := seAcceptedThenOrphaned(t, true, true, false)
			if err := updaterecord.WritePause(context.Background(), saved, updaterecord.PauseUnowned(), ""); err != nil {
				t.Fatal(err)
			}
			accepted := updaterecord.PauseAcceptedPath(marker)
			if _, err := os.Stat(accepted); err != nil {
				t.Fatalf("premise: the consumer left its receipt: %v", err)
			}
			switch receipt {
			case "refused":
				_ = os.Chmod(accepted, 0)
			case "malformed":
				_ = os.WriteFile(accepted, []byte("{trunc"), 0o644)
			default:
				_ = os.Remove(accepted)
			}
			owed := func() map[string]any {
				defer func() { _ = os.Chmod(accepted, 0o644) }()
				return seOwedAfterRecovery(t, saved["pause_id"])
			}()
			if receipt == "missing" {
				if owed != nil {
					t.Fatalf("a request nobody accepted must still expire: %v", owed)
				}
				return
			}
			if !seSameProfiles(t, owed) {
				t.Fatalf("a %s receipt retired a draining gateway's restart debt: %v", receipt, owed)
			}
			claims := seClaims()
			if len(claims) == 0 {
				t.Fatal("no claim carries the owed set")
			}
			for _, sent := range seToken(t, claims[0])["stop_sent"].([]any) {
				if pyrepr.Equal(sent, strconv.Itoa(os.Getpid())) {
					t.Fatal("unknown evidence was resolved into sent")
				}
			}
			if owed := seOwedAfterRecovery(t, saved["pause_id"]); !seSameProfiles(t, owed) {
				t.Fatalf("a readable judgement again retired the debt: %v", owed)
			}
		})
	}
}

func TestPauseAnAcceptedStopWhoseReceiptCannotBeWrittenStaysOwed(t *testing.T) {
	seSkipRoot(t)
	marker, saved := seAcceptedThenOrphaned(t, false, true, false)
	if err := updaterecord.WritePause(context.Background(), saved, updaterecord.PauseUnowned(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(updaterecord.PauseAcceptedPath(marker)); err == nil {
		t.Fatal("premise: the receipt could not be created")
	}
	if owed := seOwedAfterRecovery(t, saved["pause_id"]); !seSameProfiles(t, owed) {
		t.Fatalf("an accepted stop lost its restart debt: %v", owed)
	}
}

func TestPauseAnAcceptedStopTheRecordCouldNotCheckpointOutlivesTheRequestTTL(t *testing.T) {
	seSkipRoot(t)
	for _, consumed := range []bool{true, false} {
		t.Run(strconv.FormatBool(consumed), func(t *testing.T) {
			_, saved := seAcceptedThenOrphaned(t, true, consumed, true)
			copied := map[string]any{}
			for k, v := range saved {
				copied[k] = v
			}
			dropped, err := updaterecord.DropNeverStopped(copied)
			if err != nil {
				t.Fatal(err)
			}
			owed := dropped["profiles"].(map[string]any)
			if consumed && !seSameProfiles(t, owed) || !consumed && len(owed) != 0 {
				t.Fatalf("a gateway draining an accepted stop lost its restart debt: %v", owed)
			}
		})
	}
}
