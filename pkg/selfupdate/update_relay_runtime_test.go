package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/state"
)

// Post update maintenance over stubbed steps, the relay runtime step answering with result
func runMaintenanceWithRelayRuntime(t *testing.T, result error) (calls int, followups [][2]string, out string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	prevRoot, prevSkills, prevProfiles, prevMigration := fleetProjectRoot, maintSyncSkills, maintSyncProfiles, maintConfigMigration
	prevNotices, prevSQLite, prevRelay := maintNoticesAndSelfHeals, maintSqliteRuntimeStatus, maintProvisionRelayRuntime
	t.Cleanup(func() {
		fleetProjectRoot, maintSyncSkills, maintSyncProfiles, maintConfigMigration = prevRoot, prevSkills, prevProfiles, prevMigration
		maintNoticesAndSelfHeals, maintSqliteRuntimeStatus, maintProvisionRelayRuntime = prevNotices, prevSQLite, prevRelay
	})
	fleetProjectRoot = func() string { return root }
	maintSyncSkills = func(context.Context, bool) (map[string]any, error) { return map[string]any{}, nil }
	maintSyncProfiles = func(context.Context, io.Writer) error { return nil }
	maintConfigMigration = func(context.Context, io.Writer, bool, bool, *string) error { return nil }
	maintNoticesAndSelfHeals = func(context.Context, io.Writer) error { return nil }
	maintSqliteRuntimeStatus = func(context.Context) (bool, *state.SQLiteRuntimeInfo) { return true, nil }
	maintProvisionRelayRuntime = func(_ context.Context, w io.Writer) error {
		calls++
		io.WriteString(w, "relay runtime step\n")
		return result
	}
	var buf bytes.Buffer
	message := "✓ Update complete"
	if _, err := RunPostUpdateMaintenance(context.Background(), PostUpdateMaintenance{CompletionMessage: &message, Followups: &followups, Stdout: &buf, Stderr: &buf}); err != nil {
		t.Fatal(err)
	}
	return calls, followups, buf.String()
}

func TestPostUpdateMaintenanceProvisionsTheRelayRuntimeAndOwesAFailure(t *testing.T) {
	calls, followups, out := runMaintenanceWithRelayRuntime(t, nil)
	if calls != 1 || len(followups) != 0 || !bytes.Contains([]byte(out), []byte("relay runtime step")) {
		t.Fatalf("calls %d followups %v out %q", calls, followups, out)
	}
	failure := errors.New("NeMo Relay runtime was not provisioned: download failed")
	calls, followups, out = runMaintenanceWithRelayRuntime(t, failure)
	if calls != 1 || !reflect.DeepEqual(followups, [][2]string{{"relay_runtime", failure.Error()}}) ||
		!strings.Contains(out, "  ⚠ "+failure.Error()+"; the next icebath start provisions it in the background\n") {
		t.Fatalf("calls %d followups %v out %q", calls, followups, out)
	}
	t.Setenv(pm.EnvDisableLazyInstalls, "1")
	_, _, out = runMaintenanceWithRelayRuntime(t, failure)
	if !strings.Contains(out, "; run `icebath pm install` to provision it\n") {
		t.Fatalf("lazy installs off out %q", out)
	}
}
