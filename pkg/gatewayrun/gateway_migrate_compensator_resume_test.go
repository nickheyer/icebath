package gatewayrun

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type strandedHost struct {
	root          string
	ops           [][2]string
	startSucceeds bool
	gatewayPid    int
}

// Writes the pid file and runtime record a live gateway writes, served profiles when given
func writeLiveGateway(t *testing.T, h string, pid int, served []any) {
	record := pyjson.ObjectOf("pid", int64(pid), "hermes_home", h, "gateway_state", "running")
	if served != nil {
		record.Set("served_profiles", served)
	}
	for name, v := range map[string]any{"gateway.pid": pyjson.ObjectOf("pid", int64(pid), "hermes_home", h), "gateway_state.json": record} {
		text, err := pyjson.Dumps(v)
		if err != nil {
			t.Fatal(err)
		}
		fleetWrite(t, filepath.Join(h, name), text)
	}
}

// A host mid migration, manifest on disk, both secondaries' units already gone and nothing serving
func newStrandedHost(t *testing.T) *strandedHost {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "home", ".icebath")
	for _, sub := range []string{"profiles/coder", "profiles/ops"} {
		fleetWrite(t, filepath.Join(root, sub, "SOUL.md"), "x\n")
	}
	fleetWrite(t, filepath.Join(root, "config.yaml"), "gateway:\n  multiplex_profiles: true\n")
	fleetWrite(t, filepath.Join(root, ".env"), "TELEGRAM_BOT_TOKEN=111111:shared\n")
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, "locks"))
	for _, name := range []string{"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "GATEWAY_MULTIPLEX_PROFILES"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	secondary := func(name string, pid int64) *pyjson.Object {
		return pyjson.ObjectOf("profile", name, "home", filepath.Join(root, "profiles", name), "pid", pid,
			"services", []any{pyjson.ObjectOf("kind", "systemd", "system", false)})
	}
	manifest, _ := pyjson.Dumps(pyjson.ObjectOf("version", int64(1), "flag_was", false,
		"default", pyjson.ObjectOf("profile", "default", "home", root, "pid", nil, "services", []any{}),
		"secondaries", []any{secondary("coder", 4101), secondary("ops", 4102)}))
	fleetWrite(t, filepath.Join(root, MigrationManifestName), manifest)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	standIn := exec.Command(exe, migrateGatewayStandInEntry, "icebath_cli.main", "gateway", "run")
	if err := standIn.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		standIn.Process.Kill()
		standIn.Wait()
	})
	s := &strandedHost{root: root, startSucceeds: true, gatewayPid: standIn.Process.Pid}
	swapMigrateSeam(t, &migrateServiceOp, func(_ context.Context, _ io.Writer, _ string, _ bool, verb, h, _ string) error {
		name := migrateProfileName(h)
		s.ops = append(s.ops, [2]string{name, verb})
		if (verb == "start" || verb == "restart") && name == "default" && s.startSucceeds {
			writeLiveGateway(t, root, s.gatewayPid, []any{"default", "coder", "ops"})
		}
		return nil
	})
	swapMigrateSeam(t, &migrateInstalledServices, func(context.Context, string) ([]ServiceUnit, error) { return []ServiceUnit{}, nil })
	swapMigrateSeam(t, &migrateHostSupportsMigration, func() string { return "" })
	swapMigrateSeam(t, &migratePreflightApply, func(context.Context, *MigrationPlan, *ServiceUnit, string) (string, error) { return "", nil })
	swapMigrateSeam(t, &compensatorWait, time.Second)
	swapMigrateSeam(t, &MigrateS6.Running, func() bool { return false })
	return s
}

func TestTheCompensatorRestoresOneGatewayAndNeverRebuildsTheFleet(t *testing.T) {
	s := newStrandedHost(t)
	var out bytes.Buffer
	ok, err := RollbackMigration(context.Background(), &out, s.root)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v\n%s", ok, err, out.String())
	}
	for _, op := range s.ops {
		if (op[1] == "install" || op[1] == "start" || op[1] == "restart") && op[0] != "default" {
			t.Fatalf("a secondary gateway was rebuilt: %v", s.ops)
		}
	}
	text := out.String()
	if strings.Contains(text, "Restored the per-profile gateways") || !strings.Contains(text, "coder, ops") || !strings.Contains(text, "NOT reinstalled") {
		t.Fatal(text)
	}
	if _, err := os.Stat(filepath.Join(s.root, MigrationManifestName)); err == nil {
		t.Fatal("manifest kept")
	}
}

func TestTheCompensatorReportsFailureWhenNoGatewayComesUp(t *testing.T) {
	s := newStrandedHost(t)
	s.startSucceeds = false
	var out bytes.Buffer
	ok, err := RollbackMigration(context.Background(), &out, s.root)
	if err != nil || ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	text := out.String()
	if !strings.Contains(text, "no gateway confirmed serving this host") || !strings.Contains(text, "Compensation incomplete") {
		t.Fatal(text)
	}
	if _, err := os.Stat(filepath.Join(s.root, MigrationManifestName)); err != nil {
		t.Fatal("the manifest must survive for the next resume")
	}
}

func TestAPreflightBlockerCanNoLongerTrapAHostThatIsMidMigration(t *testing.T) {
	s := newStrandedHost(t)
	for _, name := range []string{"coder", "ops"} {
		fleetWrite(t, filepath.Join(s.root, "profiles", name, ".env"), "TELEGRAM_BOT_TOKEN=111111:shared\n")
	}
	ctx := context.Background()
	plan, err := BuildMigrationPlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Blocked() || plan.Manifest == nil {
		t.Fatal("the premise: blocked AND mid-migration")
	}
	var out bytes.Buffer
	err = CmdMigrate(ctx, &out, map[string]any{"multiplex": true, "dry_run": false, "yes": true})
	exit, isExit := err.(*gateway.SystemExitError)
	if !isExit || exit.Code != 0 {
		t.Fatalf("the blocked gate must not refuse a host that is already mid-migration: %v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"resuming the migration recorded in", "Preflight findings", filepath.Join(s.root, MigrationManifestName), "serves 3 profiles"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q\n%s", want, text)
		}
	}
	if _, err := os.Stat(filepath.Join(s.root, MigrationManifestName)); err == nil {
		t.Fatal("manifest kept")
	}
}
