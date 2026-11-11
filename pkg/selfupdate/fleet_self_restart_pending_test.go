package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

var (
	pendingOldSHA  = strings.Repeat("a", 40)
	pendingHeadSHA = strings.Repeat("b", 40)
)

// Default home plus named profiles, each with a state file whose pid the identity resolver takes as live
func pendingFleetHomes(t *testing.T, records map[string]map[string]any) {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(filepath.Join(root, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", t.TempDir())
	byHome := map[string]int{}
	for profile, record := range records {
		dir := root
		if profile != "default" {
			dir = filepath.Join(root, "profiles", profile)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(record)
		if err := os.WriteFile(filepath.Join(dir, "gateway_state.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		byHome[dir] = record["pid"].(int)
	}
	vsSwap(t, &fleetCodeIdentity, func(bool) (*pyjson.Object, error) {
		return pyjson.ObjectOf("sha", pendingHeadSHA, "version", "1.0"), nil
	})
	vsSwap(t, &fleetSocketIdentity, func(string) (int, *pyjson.Object, bool) { return 0, nil, false })
	vsSwap(t, &fleetLiveGatewayPID, func(h string) (int, bool, error) {
		pid, found := byHome[h]
		return pid, found, nil
	})
}

func fleetStates(rows []*pyjson.Object) map[string]string {
	states := map[string]string{}
	for _, row := range rows {
		profile, _ := rowValue(row, "profile").(string)
		state, _ := rowValue(row, "state").(string)
		states[profile] = state
	}
	return states
}

func TestAncestorWithAcceptedSelfRestartIsPendingWhileOtherStaleRowsStillFail(t *testing.T) {
	ancestor, sibling := os.Getpid(), os.Getppid()
	pendingFleetHomes(t, map[string]map[string]any{
		"default": {"pid": ancestor, "gateway_state": "running", "code_sha": pendingOldSHA},
		"ops":     {"pid": sibling, "gateway_state": "running", "code_sha": pendingOldSHA},
	})

	fleet := CollectFleetVersions([]int{ancestor, sibling}, []int{ancestor})
	if want := map[string]string{"default": updaterecord.RestartPendingState, "ops": "stale"}; !reflect.DeepEqual(fleetStates(fleet), want) {
		t.Fatalf("states = %v", fleetStates(fleet))
	}
	var out bytes.Buffer
	if !printFleetVersionMatrixTo(&out, fleet) {
		t.Fatal("the stale sibling did not fail the matrix")
	}
	if !strings.Contains(out.String(), "restart pending") || !strings.Contains(out.String(), "1 gateway(s) still running the old code") {
		t.Fatalf("matrix = %q", out.String())
	}

	var onlyAncestor []*pyjson.Object
	for _, row := range fleet {
		if rowValue(row, "profile") == "default" {
			onlyAncestor = append(onlyAncestor, row)
		}
	}
	if printFleetVersionMatrixTo(io.Discard, onlyAncestor) {
		t.Fatal("a pending ancestor alone failed the matrix")
	}
	plain := CollectFleetVersions([]int{ancestor, sibling}, nil)
	if want := map[string]string{"default": "stale", "ops": "stale"}; !reflect.DeepEqual(fleetStates(plain), want) {
		t.Fatalf("plain states = %v", fleetStates(plain))
	}
}

func TestRestartPhaseRecordsAcceptedSelfRestartAndVerifyExitsClean(t *testing.T) {
	ancestor := os.Getpid()
	vsSwap(t, &fleetIsPidAncestor, func(pid int) bool { return pid == ancestor })
	vsSwap(t, &fleetRequestSelfRestart, func(int) bool { return true })
	pending := map[int]bool{}
	accepted, err := drainOrSignalGatewayForUpdate(context.Background(), io.Discard, ancestor, 5.0, "default", pending)
	if err != nil || !accepted || !reflect.DeepEqual(pending, map[int]bool{ancestor: true}) {
		t.Fatalf("accepted=%v pending=%v err=%v", accepted, pending, err)
	}

	pendingFleetHomes(t, map[string]map[string]any{"default": {"pid": ancestor, "gateway_state": "running", "code_sha": pendingOldSHA}})
	quietVerification(t)
	vsSwap(t, &fleetSleep, func(time.Duration) {})
	cleared := 0
	vsSwap(t, &fleetClearMarker, func(context.Context) { cleared++ })
	vsSwap(t, &fleetAutoMigrate, func(context.Context, io.Writer) error { return nil })
	followups := []string{}
	record := fleetRecordFollowup
	vsSwap(t, &fleetRecordFollowup, func(ctx context.Context, step, reason, retry string) {
		followups = append(followups, step)
		record(ctx, step, reason, retry)
	})
	restart := newGatewayRestartOutcome()
	restart.PreRestartGatewayPIDs = []int{ancestor}
	restart.RestartedServices = []string{"icebath-gateway"}
	restart.SelfRestartPendingPIDs = pending
	ctx := context.Background()
	var out bytes.Buffer
	if err := verifyFleetAfterUpdate(ctx, &out, restart, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "restart pending") || strings.Contains(out.String(), "Update not complete") || cleared != 1 {
		t.Fatalf("out=%q cleared=%d", out.String(), cleared)
	}

	out.Reset()
	restart.SelfRestartPendingPIDs = map[int]bool{}
	if err := verifyFleetAfterUpdate(ctx, &out, restart, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if !restart.Incomplete || !reflect.DeepEqual(followups, []string{"gateway_restart"}) || cleared != 1 {
		t.Fatalf("incomplete=%v followups=%v cleared=%d", restart.Incomplete, followups, cleared)
	}
}
