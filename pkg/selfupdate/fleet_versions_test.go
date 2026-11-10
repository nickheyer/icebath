package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func fvRestoreSeams(t *testing.T) {
	identity, socket, homes, read, live, owner, out := fleetCodeIdentity, fleetSocketIdentity, fleetHomes,
		fleetReadStatus, fleetStatusPidIsLive, fleetLiveGatewayPID, fleetStdout
	t.Cleanup(func() {
		fleetCodeIdentity, fleetSocketIdentity, fleetHomes = identity, socket, homes
		fleetReadStatus, fleetStatusPidIsLive, fleetLiveGatewayPID, fleetStdout = read, live, owner, out
	})
}

func fvIdentity(sha string) func(bool) (*pyjson.Object, error) {
	return func(bool) (*pyjson.Object, error) {
		return pyjson.ObjectOf("sha", sha, "short_sha", sha[:8], "version", "1.0", "source", "git"), nil
	}
}

func fvHome(t *testing.T, name string) string {
	h := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func fvFleetWith(t *testing.T, record *pyjson.Object, expectedSHA string) []*pyjson.Object {
	fvRestoreSeams(t)
	h := fvHome(t, "fleet_home")
	gatewayRecord := pyjson.ObjectOf("gateway_state", "running", "kind", "icebath-gateway",
		"argv", []any{"icebath", "gateway", "run"})
	gatewayRecord.Update(record)
	text, err := pyjson.Dumps(gatewayRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "gateway_state.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	fleetCodeIdentity = fvIdentity(expectedSHA)
	fleetSocketIdentity = func(string) (int, *pyjson.Object, bool) { return 0, nil, false }
	pid := gatewayRecord.Vals["pid"].(int)
	fleetLiveGatewayPID = func(string) (int, bool, error) { return pid, true, nil }
	return CollectFleetVersions(nil, nil)
}

func TestFleetLiveNonGatewayStateWriterIsUnknown(t *testing.T) {
	fvRestoreSeams(t)
	fvHome(t, "fleet_home")
	fleetCodeIdentity = fvIdentity(strings.Repeat("a", 40))
	if _, err := gateway.WriteRuntimeStatus(context.Background(), gateway.RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "running"}}, false, nil); err != nil {
		t.Fatal(err)
	}
	fleet := CollectFleetVersions(nil, nil)
	if len(fleet) != 1 || fleet[0].Vals["pid"] != os.Getpid() || fleet[0].Vals["state"] != "unknown" ||
		fleet[0].Vals["code_sha"] != nil || fleet[0].Vals["code_version"] != nil {
		t.Fatalf("fleet %v", fvRows(fleet))
	}
}

func TestFleetCurrentGateway(t *testing.T) {
	sha := strings.Repeat("a", 40)
	fleet := fvFleetWith(t, pyjson.ObjectOf("pid", 4242, "code_sha", sha, "code_version", "1.0"), sha)
	if len(fleet) != 1 || fleet[0].Vals["state"] != "current" || fleet[0].Vals["pid"] != 4242 {
		t.Fatalf("fleet %v", fvRows(fleet))
	}
}

func TestFleetCurrentMultiplexerReportsItsServedProfiles(t *testing.T) {
	sha := strings.Repeat("a", 40)
	fleet := fvFleetWith(t, pyjson.ObjectOf("pid", 4242, "code_sha", sha, "code_version", "1.0",
		"served_profiles", []any{"default", "coder"}), sha)
	if len(fleet) != 1 || !reflect.DeepEqual(fleet[0].Vals["served_profiles"], []any{"default", "coder"}) {
		t.Fatalf("fleet %v", fvRows(fleet))
	}
}

func TestFleetStaleGateway(t *testing.T) {
	fleet := fvFleetWith(t, pyjson.ObjectOf("pid", 4242, "code_sha", strings.Repeat("b", 40), "code_version", "0.9"),
		strings.Repeat("a", 40))
	if len(fleet) != 1 || fleet[0].Vals["state"] != "stale" {
		t.Fatalf("fleet %v", fvRows(fleet))
	}
}

func TestFleetUnstampedGatewayIsUnknown(t *testing.T) {
	fleet := fvFleetWith(t, pyjson.ObjectOf("pid", 4242), strings.Repeat("a", 40))
	if len(fleet) != 1 || fleet[0].Vals["state"] != "unknown" {
		t.Fatalf("fleet %v", fvRows(fleet))
	}
}

func TestFleetDeadPidExcluded(t *testing.T) {
	fvRestoreSeams(t)
	h := fvHome(t, "fleet_home2")
	body := `{"pid": 999999, "code_sha": "` + strings.Repeat("a", 40) + `"}`
	if err := os.WriteFile(filepath.Join(h, "gateway_state.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	fleetStatusPidIsLive = func(*pyjson.Object) bool { return false }
	fleetLiveGatewayPID = func(string) (int, bool, error) { return 0, false, nil }
	if fleet := CollectFleetVersions(nil, nil); len(fleet) != 0 {
		t.Fatalf("fleet %v", fvRows(fleet))
	}
}

func TestFleetLiveRuntimeRecordWithoutVerifiedGatewayIsUnknown(t *testing.T) {
	fvRestoreSeams(t)
	h := fvHome(t, "fleet_home")
	fleetCodeIdentity = func(bool) (*pyjson.Object, error) { return pyjson.ObjectOf("sha", strings.Repeat("a", 40)), nil }
	record := pyjson.ObjectOf("pid", 4242, "gateway_state", "running", "kind", "icebath-gateway",
		"argv", []any{"icebath", "gateway", "run"}, "code_sha", strings.Repeat("a", 40), "code_version", "1.0")
	fleetHomes = func() []profileHome { return []profileHome{{"default", h}} }
	fleetSocketIdentity = func(string) (int, *pyjson.Object, bool) { return 0, nil, false }
	fleetReadStatus = func(string) *pyjson.Object { return record }
	fleetStatusPidIsLive = func(*pyjson.Object) bool { return true }
	fleetLiveGatewayPID = func(string) (int, bool, error) { return 0, false, nil }
	fleet := CollectFleetVersions(nil, nil)
	if len(fleet) != 1 || fleet[0].Vals["state"] != "unknown" || fleet[0].Vals["code_sha"] != nil || fleet[0].Vals["code_version"] != nil {
		t.Fatalf("fleet %v", fvRows(fleet))
	}
}

func TestFleetMatrixReturnsTrueOnlyOnStale(t *testing.T) {
	fvRestoreSeams(t)
	fleetStdout = &bytes.Buffer{}
	current := pyjson.ObjectOf("profile", "default", "pid", 1, "code_sha", strings.Repeat("a", 40), "state", "current")
	stale := pyjson.ObjectOf("profile", "work", "pid", 2, "code_sha", strings.Repeat("b", 40), "state", "stale")
	if PrintFleetVersionMatrix(nil) || PrintFleetVersionMatrix([]*pyjson.Object{current}) ||
		!PrintFleetVersionMatrix([]*pyjson.Object{current, stale}) {
		t.Fatal("only a stale row fails the matrix")
	}
}

func TestFleetUnknownDoesNotFailUpdate(t *testing.T) {
	fvRestoreSeams(t)
	out := &bytes.Buffer{}
	fleetStdout = out
	row := pyjson.ObjectOf("profile", "default", "pid", 1, "code_sha", nil, "state", "unknown")
	if PrintFleetVersionMatrix([]*pyjson.Object{row}) {
		t.Fatal("unknown row failed the update")
	}
	if !strings.Contains(out.String(), "? default (pid 1) — version unknown") {
		t.Fatalf("matrix %q", out.String())
	}
}

func fvRows(fleet []*pyjson.Object) []any {
	rows := make([]any, len(fleet))
	for i, row := range fleet {
		rows[i] = pyjson.Plain(row)
	}
	return rows
}
