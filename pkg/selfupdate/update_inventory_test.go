//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Isolates the inventory from this host so only the seams a test sets report runtimes
func pinInventory(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := struct {
		pids     func(context.Context, bool) ([]int, error)
		windows  func(context.Context) ([]gatewaycli.WindowsGatewayService, error)
		procs    func(context.Context) ([]gatewaycli.ProfileGatewayProcess, error)
		image    func() *ImageProvenance
		homes    func() []profileHome
		identity func(bool) (*jsonutil.Object, error)
	}{inventoryServicePIDs, inventoryWindowsServices, inventoryProfileGatewayProcs, inventoryReadImageProvenance, fleetHomes, fleetCodeIdentity}
	t.Cleanup(func() {
		inventoryServicePIDs, inventoryWindowsServices, inventoryProfileGatewayProcs = saved.pids, saved.windows, saved.procs
		inventoryReadImageProvenance, fleetHomes, fleetCodeIdentity = saved.image, saved.homes, saved.identity
	})
	inventoryServicePIDs = func(context.Context, bool) ([]int, error) { return nil, nil }
	inventoryWindowsServices = func(context.Context) ([]gatewaycli.WindowsGatewayService, error) { return nil, nil }
	inventoryProfileGatewayProcs = func(context.Context) ([]gatewaycli.ProfileGatewayProcess, error) { return nil, nil }
	inventoryReadImageProvenance = func() *ImageProvenance { return nil }
	fleetHomes = func() []profileHome { return nil }
	fleetCodeIdentity = func(bool) (*jsonutil.Object, error) {
		identity := jsonutil.NewObject()
		identity.Set("sha", "feedc0de")
		identity.Set("version", "1.2.3")
		return identity, nil
	}
}

func TestInventoryGatewayRowsComeFromSocketsThenStatusThenPidFilesOncePerProcess(t *testing.T) {
	pinInventory(t)
	savedSock, savedLive, savedStatus := fleetSocketIdentity, fleetLiveGatewayPID, fleetReadStatus
	t.Cleanup(func() { fleetSocketIdentity, fleetLiveGatewayPID, fleetReadStatus = savedSock, savedLive, savedStatus })
	identity := func(pairs ...any) *jsonutil.Object {
		obj := jsonutil.NewObject()
		for i := 0; i+1 < len(pairs); i += 2 {
			obj.Set(pairs[i].(string), pairs[i+1])
		}
		return obj
	}
	fleetHomes = func() []profileHome {
		return []profileHome{{"default", "/h/default"}, {"work", "/h/work"}, {"side", "/h/side"}, {"down", "/h/down"}, {"odd", "/h/odd"}}
	}
	fleetSocketIdentity = func(h string) (int, *jsonutil.Object, bool) {
		switch h {
		case "/h/default", "/h/work":
			return 100, identity("launch_supervisor", "systemd", "supervisor", "none", "code_sha", "aaa", "code_version", "9.9"), true
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
	fleetReadStatus = func(path string) *jsonutil.Object { return identity("code_sha", "bbb") }
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
	want := []row{{"default", "systemd", "systemd", 100, "aaa"}, {"side", "launchd", "launchd", 200, "bbb"}, {"odd", "launchd", "launchd", 300, ""},
		{"extra", "manual", "manual", 400, ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows %+v", got)
	}
	if !reflect.DeepEqual(plan.Profiles, []any{"default", "work", "side", "down", "odd"}) || plan.Runtimes[0].CodeVersion != "9.9" {
		t.Fatalf("profiles %v version %v", plan.Profiles, plan.Runtimes[0].CodeVersion)
	}
}

func TestUpdatePlanRoundTripsThroughItsDict(t *testing.T) {
	pinInventory(t)
	inventoryProfileGatewayProcs = func(context.Context) ([]gatewaycli.ProfileGatewayProcess, error) {
		return []gatewaycli.ProfileGatewayProcess{{Profile: "work", PID: 4321}}, nil
	}
	plan := CollectRuntimeInventory(context.Background())
	if plan.ExpectedSHA != "feedc0de" || plan.ExpectedVersion != "1.2.3" || plan.InstallMethod == "" || plan.UpdateMechanism == "" {
		t.Fatalf("plan %+v", plan)
	}
	dict := plan.ToDict()
	if want := []string{"install_method", "updatable_in_place", "update_mechanism", "expected_sha", "expected_version", "profiles", "runtimes"}; !reflect.DeepEqual(dict.Keys(), want) {
		t.Fatalf("plan keys %v", dict.Keys())
	}
	runtimes, _ := dict.Get("runtimes")
	first := runtimes.([]any)[0].(*jsonutil.Object)
	if want := []string{"kind", "profile", "pid", "supervisor", "code_sha", "code_version", "restart_via", "detail"}; !reflect.DeepEqual(first.Keys(), want) {
		t.Fatalf("runtime keys %v", first.Keys())
	}
	dict.Set("unknown_field", "dropped")
	dict.Set("runtimes", append(runtimes.([]any), "junk"))
	back, err := UpdatePlanFromDict(dict)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.ToDict().Value("runtimes").([]any)[0].(*jsonutil.Object).Map(), first.Map()) || back.InstallMethod != plan.InstallMethod ||
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
	pinInventory(t)
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
