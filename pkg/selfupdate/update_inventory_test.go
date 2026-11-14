package selfupdate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Serve ledger row of a network bound serve with the given fields replaced
func ledgerEntry(over map[string]any) map[string]any {
	entry := map[string]any{"pid": int64(4321), "create_time": 111.0, "purpose": "serve", "install": "inst", "spawner_pid": nil, "spawner_create": nil,
		"registered_at": 222.0, "argv": "icebath serve --host 100.94.65.93 --port 9119", "host": "100.94.65.93", "port": int64(9119), "profile": ""}
	for key, value := range over {
		entry[key] = value
	}
	return entry
}

// Isolates the inventory from this host and feeds it the given ledger rows and spawner verdict
func pinInventory(t *testing.T, entries []map[string]any, spawnerDead, spawnerKnown bool) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := struct {
		pids     func(context.Context, bool) ([]int, error)
		windows  func(context.Context) ([]gatewaycli.WindowsGatewayService, error)
		procs    func(context.Context) ([]gatewaycli.ProfileGatewayProcess, error)
		ledger   func() []map[string]any
		spawner  func(map[string]any) (bool, bool)
		jobs     func(context.Context) []gateway.LaunchdBackendJob
		parents  func(context.Context, int) []int
		systemd  func(context.Context, int) (string, string, bool)
		image    func() *ImageProvenance
		homes    func() []profileHome
		identity func(bool) (*pyjson.Object, error)
		liveness func(int64, *float64) (bool, bool)
	}{inventoryServicePIDs, inventoryWindowsServices, inventoryProfileGatewayProcs, inventoryLedgerEntries, inventorySpawnerIsDead, inventoryLaunchdJobs,
		inventoryProcessAncestors, inventorySystemdOwnerForPid, inventoryReadImageProvenance, fleetHomes, fleetCodeIdentity, servePidAliveMatches}
	t.Cleanup(func() {
		inventoryServicePIDs, inventoryWindowsServices, inventoryProfileGatewayProcs, inventoryLedgerEntries = saved.pids, saved.windows, saved.procs, saved.ledger
		inventorySpawnerIsDead, inventoryLaunchdJobs, inventoryProcessAncestors, inventorySystemdOwnerForPid = saved.spawner, saved.jobs, saved.parents, saved.systemd
		inventoryReadImageProvenance, fleetHomes, fleetCodeIdentity, servePidAliveMatches = saved.image, saved.homes, saved.identity, saved.liveness
	})
	inventoryServicePIDs = func(context.Context, bool) ([]int, error) { return nil, nil }
	inventoryWindowsServices = func(context.Context) ([]gatewaycli.WindowsGatewayService, error) { return nil, nil }
	inventoryProfileGatewayProcs = func(context.Context) ([]gatewaycli.ProfileGatewayProcess, error) { return nil, nil }
	inventoryLedgerEntries = func() []map[string]any { return entries }
	inventorySpawnerIsDead = func(map[string]any) (bool, bool) { return spawnerDead, spawnerKnown }
	inventoryLaunchdJobs = func(context.Context) []gateway.LaunchdBackendJob { return nil }
	inventoryProcessAncestors = func(context.Context, int) []int { return []int{} }
	inventorySystemdOwnerForPid = func(context.Context, int) (string, string, bool) { return "", "", false }
	inventoryReadImageProvenance = func() *ImageProvenance { return nil }
	fleetHomes = func() []profileHome { return nil }
	fleetCodeIdentity = func(bool) (*pyjson.Object, error) {
		identity := pyjson.NewObject()
		identity.Set("sha", "feedc0de")
		identity.Set("version", "1.2.3")
		return identity, nil
	}
}

// First serve runtime of the collected inventory, failing the test when there is none
func firstServe(t *testing.T) *RuntimeRecord {
	t.Helper()
	for _, runtime := range CollectRuntimeInventory(context.Background()).Runtimes {
		if runtime.Kind == "serve" {
			return runtime
		}
	}
	t.Fatal("no serve runtime in the inventory")
	return nil
}

func detailValue(r *RuntimeRecord, key string) any {
	value, _ := r.Detail.Get(key)
	return value
}

func TestInventoryIncludesManualServeFromLedger(t *testing.T) {
	pinInventory(t, []map[string]any{ledgerEntry(nil)}, false, false)
	row := firstServe(t)
	if row.PID == nil || *row.PID != 4321 || row.Supervisor != "manual-serve" || row.RestartVia != "respawn-argv" ||
		detailValue(row, "host") != "100.94.65.93" || detailValue(row, "port") != int64(9119) || row.Profile != "default" {
		t.Fatalf("row %+v detail %v", row, row.Detail.Vals)
	}
}

func TestInventoryClassifiesDesktopOwnedServe(t *testing.T) {
	pinInventory(t, []map[string]any{ledgerEntry(map[string]any{"spawner_pid": int64(999), "spawner_create": 1.0})}, false, true)
	if row := firstServe(t); row.Supervisor != "desktop" || row.RestartVia != "desktop" {
		t.Fatalf("row %+v", row)
	}
}

const sshServeArgv = "icebath serve --isolated --host 127.0.0.1 --port 0 --ssh-session-token-file /h/.icebath/desktop-ssh/a/b.token --ssh-owner-nonce 0123456789abcdef"

func TestInventoryClassifiesRemoteDesktopSshServeAsItsClients(t *testing.T) {
	pinInventory(t, []map[string]any{ledgerEntry(map[string]any{"argv": sshServeArgv, "host": "127.0.0.1", "port": int64(57474), "isolated": true})}, false, false)
	row := firstServe(t)
	if row.Supervisor == "manual-serve" || row.Supervisor == "desktop" || row.RestartVia == "respawn-argv" {
		t.Fatalf("row %+v", row)
	}
	if row.Supervisor != "desktop-ssh" || !ClientOwnedServeSupervisors[row.Supervisor] {
		t.Fatalf("supervisor %q", row.Supervisor)
	}
	if DeferManualServe(context.Background(), row.ToDict(), false) {
		t.Fatal("a manual restart reminder was filed for a client owned serve")
	}
}

func TestHandStartedIsolatedServeStaysManual(t *testing.T) {
	pinInventory(t, []map[string]any{ledgerEntry(map[string]any{"argv": "icebath serve --isolated --host 127.0.0.1 --port 9119", "isolated": true})}, false, false)
	if row := firstServe(t); row.Supervisor != "manual-serve" {
		t.Fatalf("row %+v", row)
	}
}

func TestInventoryRecordsTheServeProcessIncarnation(t *testing.T) {
	pinInventory(t, []map[string]any{ledgerEntry(map[string]any{"create_time": 1712345678.5})}, false, false)
	if row := firstServe(t); detailValue(row, "create_time") != 1712345678.5 {
		t.Fatalf("detail %v", row.Detail.Vals)
	}
}

func TestInventoryClassifiesLaunchdJobOwnedServe(t *testing.T) {
	pinInventory(t, []map[string]any{ledgerEntry(map[string]any{"spawner_pid": int64(999), "spawner_create": 1.0})}, true, true)
	inventoryLaunchdJobs = func(context.Context) []gateway.LaunchdBackendJob {
		return []gateway.LaunchdBackendJob{{Domain: "gui/501", Label: "ai.icebath.dashboard", Argv: []string{"icebath", "serve", "--host", "100.94.65.93", "--port", "9119"}}}
	}
	row := firstServe(t)
	if row.Supervisor != "launchd" || row.RestartVia != "launchd" || detailValue(row, "launchd_domain") != "gui/501" || detailValue(row, "launchd_label") != "ai.icebath.dashboard" {
		t.Fatalf("row %+v detail %v", row, row.Detail.Vals)
	}
}

func TestInventoryClassifiesSystemdUnitOwnedServeAndSkipsRowsOutsideTheServeKinds(t *testing.T) {
	entries := []map[string]any{ledgerEntry(map[string]any{"purpose": "dashboard", "profile": "ops"}), ledgerEntry(map[string]any{"pid": int64(5), "purpose": "gateway"}),
		ledgerEntry(map[string]any{"pid": "77"}), ledgerEntry(nil)}
	pinInventory(t, entries, true, true)
	inventorySystemdOwnerForPid = func(ctx context.Context, pid int) (string, string, bool) {
		return "icebath-dashboard.service", "", pid == 4321
	}
	plan := CollectRuntimeInventory(context.Background())
	if len(plan.Runtimes) != 1 {
		t.Fatalf("runtimes %d", len(plan.Runtimes))
	}
	row := plan.Runtimes[0]
	if row.Kind != "dashboard" || row.Profile != "ops" || row.Supervisor != "systemd" || row.RestartVia != "systemd" ||
		detailValue(row, "systemd_unit") != "icebath-dashboard.service" || detailValue(row, "systemd_scope") != nil {
		t.Fatalf("row %+v detail %v", row, row.Detail.Vals)
	}
	if want := []string{"argv", "host", "port", "create_time", "systemd_unit", "systemd_scope"}; !reflect.DeepEqual(row.Detail.Keys, want) {
		t.Fatalf("detail keys %v", row.Detail.Keys)
	}
	if plan.ExpectedSHA != "feedc0de" || plan.ExpectedVersion != "1.2.3" || plan.InstallMethod == "" || plan.UpdateMechanism == "" {
		t.Fatalf("plan %+v", plan)
	}
}

func TestInventoryGatewayRowsComeFromSocketsThenStatusThenPidFilesOncePerProcess(t *testing.T) {
	pinInventory(t, nil, false, false)
	savedSock, savedLive, savedStatus := fleetSocketIdentity, fleetLiveGatewayPID, fleetReadStatus
	t.Cleanup(func() { fleetSocketIdentity, fleetLiveGatewayPID, fleetReadStatus = savedSock, savedLive, savedStatus })
	identity := func(pairs ...any) *pyjson.Object {
		obj := pyjson.NewObject()
		for i := 0; i+1 < len(pairs); i += 2 {
			obj.Set(pairs[i].(string), pairs[i+1])
		}
		return obj
	}
	fleetHomes = func() []profileHome {
		return []profileHome{{"default", "/h/default"}, {"work", "/h/work"}, {"side", "/h/side"}, {"down", "/h/down"}, {"odd", "/h/odd"}}
	}
	fleetSocketIdentity = func(h string) (int, *pyjson.Object, bool) {
		switch h {
		case "/h/default", "/h/work":
			return 100, identity("launch_supervisor", "desktop", "supervisor", "none", "code_sha", "aaa", "code_version", "9.9"), true
		case "/h/odd":
			return 300, identity("supervisor", "none"), true
		}
		return 0, nil, false
	}
	fleetLiveGatewayPID = func(h string) (int, bool, error) {
		if h == "/h/side" {
			return 200, true, nil
		}
		return 0, false, nil
	}
	fleetReadStatus = func(path string) *pyjson.Object { return identity("code_sha", "bbb") }
	inventoryServicePIDs = func(context.Context, bool) ([]int, error) { return []int{200, 300}, nil }
	inventoryWindowsServices = func(context.Context) ([]gatewaycli.WindowsGatewayService, error) {
		return []gatewaycli.WindowsGatewayService{{GatewayPID: 300}}, errors.New("SCM service enumeration failed")
	}
	savedSystemd, savedMac := inventorySupportsSystemd, inventoryIsMacOS
	t.Cleanup(func() { inventorySupportsSystemd, inventoryIsMacOS = savedSystemd, savedMac })
	inventorySupportsSystemd = func(context.Context) bool { return false }
	inventoryIsMacOS = func() bool { return true }
	inventoryProfileGatewayProcs = func(context.Context) ([]gatewaycli.ProfileGatewayProcess, error) {
		return []gatewaycli.ProfileGatewayProcess{{Profile: "work", PID: 100}, {Profile: "extra", PID: 400}}, nil
	}
	plan := CollectRuntimeInventory(context.Background())
	type row struct {
		profile, supervisor, via string
		pid                      int
		sha                      string
	}
	var got []row
	for _, r := range plan.Runtimes {
		sha := ""
		if r.CodeSHA != nil {
			sha = *r.CodeSHA
		}
		got = append(got, row{r.Profile, r.Supervisor, r.RestartVia, *r.PID, sha})
	}
	want := []row{{"default", "desktop", "desktop", 100, "aaa"}, {"side", "launchd", "launchd", 200, "bbb"}, {"odd", "launchd", "launchd", 300, ""},
		{"extra", "manual", "manual", 400, ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows %+v", got)
	}
	if !reflect.DeepEqual(plan.Profiles, []any{"default", "work", "side", "down", "odd"}) || plan.Runtimes[0].CodeVersion != "9.9" {
		t.Fatalf("profiles %v version %v", plan.Profiles, plan.Runtimes[0].CodeVersion)
	}
}

func TestUpdatePlanRoundTripsThroughItsDict(t *testing.T) {
	pinInventory(t, []map[string]any{ledgerEntry(nil)}, false, false)
	plan := CollectRuntimeInventory(context.Background())
	dict := plan.ToDict()
	if want := []string{"install_method", "updatable_in_place", "update_mechanism", "expected_sha", "expected_version", "profiles", "runtimes"}; !reflect.DeepEqual(dict.Keys, want) {
		t.Fatalf("plan keys %v", dict.Keys)
	}
	runtimes, _ := dict.Get("runtimes")
	first := runtimes.([]any)[0].(*pyjson.Object)
	if want := []string{"kind", "profile", "pid", "supervisor", "code_sha", "code_version", "restart_via", "detail"}; !reflect.DeepEqual(first.Keys, want) {
		t.Fatalf("runtime keys %v", first.Keys)
	}
	dict.Set("unknown_field", "dropped")
	dict.Set("runtimes", append(runtimes.([]any), "junk"))
	back, err := UpdatePlanFromDict(dict)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.ToDict().Vals["runtimes"].([]any)[0].(*pyjson.Object).Vals, first.Vals) || back.InstallMethod != plan.InstallMethod ||
		back.UpdateMechanism != plan.UpdateMechanism || len(back.Runtimes) != 1 {
		t.Fatalf("round trip %+v", back)
	}
	if _, err := UpdatePlanFromDict(map[string]any{"runtimes": []any{map[string]any{"profile": "x"}}}); err == nil {
		t.Fatal("a runtime row without a kind accepted")
	}
	empty, err := UpdatePlanFromDict(map[string]any{})
	if err != nil || empty.InstallMethod != "unknown" || !empty.UpdatableInPlace || empty.UpdateMechanism != "icebath update" {
		t.Fatalf("defaults %+v %v", empty, err)
	}
	if DescribeRestartMechanism("systemd", "x") != "systemctl restart (drain-first SIGUSR1 when supported)" ||
		DescribeRestartMechanism("manual", "default") != "icebath gateway restart" || DescribeRestartMechanism("manual", "work") != "icebath -p work gateway restart" {
		t.Fatal("mechanism descriptions")
	}
}

func TestInstallShapeFollowsImageProvenance(t *testing.T) {
	pinInventory(t, nil, false, false)
	savedMethod, savedManaged := inventoryDetectInstallMethod, inventoryManagedSystem
	t.Cleanup(func() { inventoryDetectInstallMethod, inventoryManagedSystem = savedMethod, savedManaged })
	inventoryDetectInstallMethod = func(context.Context) (string, error) { return "git", nil }
	inventoryManagedSystem = func(context.Context) string { return "" }
	plan := CollectRuntimeInventory(context.Background())
	if plan.InstallMethod != "git" || !plan.UpdatableInPlace || plan.UpdateMechanism != "icebath update" {
		t.Fatalf("git plan %+v", plan)
	}
	reason := "marker_not_object"
	inventoryReadImageProvenance = func() *ImageProvenance { return &ImageProvenance{Manager: "unknown", Valid: false, Error: &reason} }
	if plan = CollectRuntimeInventory(context.Background()); plan.InstallMethod != "git" || plan.UpdatableInPlace {
		t.Fatalf("invalid marker plan %+v", plan)
	}
	inventoryReadImageProvenance = func() *ImageProvenance { return &ImageProvenance{Manager: "docker", Valid: true} }
	if plan = CollectRuntimeInventory(context.Background()); plan.InstallMethod != "docker" || plan.UpdatableInPlace {
		t.Fatalf("image plan %+v", plan)
	}
	inventoryReadImageProvenance = func() *ImageProvenance { return nil }
	inventoryManagedSystem = func(context.Context) string { return "nixos" }
	if plan = CollectRuntimeInventory(context.Background()); plan.InstallMethod != "nixos" || plan.UpdatableInPlace {
		t.Fatalf("managed plan %+v", plan)
	}
	inventoryDetectInstallMethod = func(context.Context) (string, error) { return "", errors.New("unreadable stamp") }
	if plan = CollectRuntimeInventory(context.Background()); plan.InstallMethod != "unknown" || !plan.UpdatableInPlace || plan.UpdateMechanism != "icebath update" {
		t.Fatalf("failed probe plan %+v", plan)
	}
}
