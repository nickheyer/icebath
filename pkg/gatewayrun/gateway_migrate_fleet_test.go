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

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gatewayapi"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Hidden first argument of a re-executed test binary whose command line reads as a running gateway
const migrateGatewayStandInEntry = "__gatewayrun_gateway_standin__"

type fleetUnitKey struct {
	name, kind string
	system     bool
}

// default plus coder and ops, both secondaries running a live standalone gateway with a user systemd unit
type migrateFleet struct {
	t              *testing.T
	ctx            context.Context
	tmp, root      string
	services       map[string][]ServiceUnit
	pids           map[string]int
	ops            [][2]string
	refusedAtStart map[string]bool
	enabled        map[fleetUnitKey]bool
	gatewayPid     int
	systemUnitDir  string
	out            *bytes.Buffer
}

func swapMigrateSeam[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func fleetWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func unit(kind string, system bool) ServiceUnit { return ServiceUnit{kind, system} }

func newMigrateFleet(t *testing.T) *migrateFleet {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "icebath")
	for _, sub := range []string{"profiles/coder", "profiles/ops"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fleetWrite(t, filepath.Join(root, "config.yaml"), "model:\n  default: x\n")
	fleetWrite(t, filepath.Join(root, ".env"), "TELEGRAM_BOT_TOKEN=111111:default-token\n")
	fleetWrite(t, filepath.Join(root, "profiles/coder/.env"), "TELEGRAM_BOT_TOKEN=222222:coder-token\n")
	fleetWrite(t, filepath.Join(root, "profiles/ops/.env"), "DISCORD_BOT_TOKEN=ops-discord-333333\n")
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, "locks"))
	if err := os.MkdirAll(filepath.Join(tmp, "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GATEWAY_MULTIPLEX_PROFILES", "TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "API_SERVER_KEY", "WEBHOOK_ENABLED"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
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
	f := &migrateFleet{t: t, ctx: context.Background(), tmp: tmp, root: root,
		services:       map[string][]ServiceUnit{"coder": {unit("systemd", false)}, "ops": {unit("systemd", false)}},
		pids:           map[string]int{"coder": 4101, "ops": 4102},
		refusedAtStart: map[string]bool{},
		enabled:        map[fleetUnitKey]bool{{"coder", "systemd", false}: true, {"ops", "systemd", false}: true},
		gatewayPid:     standIn.Process.Pid, systemUnitDir: gateway.SystemUnitDir(), out: &bytes.Buffer{}}
	swapMigrateSeam(t, &migrateSystemdUnitPath, func(ctx context.Context, system bool) (string, error) {
		name, err := gateway.GetServiceName(ctx)
		if err != nil {
			return "", err
		}
		if system {
			return filepath.Join(f.systemUnitDir, name+".service"), nil
		}
		return filepath.Join(tmp, "user-units", name+".service"), nil
	})
	swapMigrateSeam(t, &migrateInstalledServices, func(_ context.Context, h string) ([]ServiceUnit, error) {
		return append([]ServiceUnit{}, f.services[migrateProfileName(h)]...), nil
	})
	swapMigrateSeam(t, &migrateLiveGatewayPid, func(h string) int { return f.pids[migrateProfileName(h)] })
	swapMigrateSeam(t, &migrateServiceOp, f.serviceOp)
	swapMigrateSeam(t, &migrateStopGatewayProcess, func(h string) { delete(f.pids, migrateProfileName(h)) })
	swapMigrateSeam(t, &migrateSpawnDetachedGateway, func(_ context.Context, h string) (bool, error) { return f.fakeSpawn(h), nil })
	swapMigrateSeam(t, &migrateWaitForLiveGateway, func(h string, _ time.Duration) int { return pidOfStartedGateway(h) })
	swapMigrateSeam(t, &migrateHostSupportsMigration, func() string { return "" })
	swapMigrateSeam(t, &migratePreflightApply, func(context.Context, *MigrationPlan, *ServiceUnit, string) (string, error) { return "", nil })
	swapMigrateSeam(t, &guardsPidUID, func(_ context.Context, pid int) *int {
		for _, p := range f.pids {
			if p == pid {
				info, err := os.Stat(root)
				if err != nil {
					t.Fatal(err)
				}
				uid, _ := statUID(info)
				return &uid
			}
		}
		return nil
	})
	swapMigrateSeam(t, &MigrateS6.Running, func() bool { return false })
	return f
}

// The fake service layer, recording each op and starting the default by writing what a booted multiplexer writes
func (f *migrateFleet) serviceOp(ctx context.Context, _ io.Writer, kind string, system bool, verb, h, _ string) error {
	name := migrateProfileName(h)
	f.ops = append(f.ops, [2]string{name, verb})
	if verb == "start" && name != "default" {
		served, _ := gateway.NamedProfileServedByRunningMultiplexerCtx(ctx, &name)
		f.refusedAtStart[name] = served
	}
	switch {
	case verb == "uninstall":
		remaining := []ServiceUnit{}
		for _, u := range f.services[name] {
			if u != unit(kind, system) {
				remaining = append(remaining, u)
			}
		}
		if len(remaining) > 0 {
			f.services[name] = remaining
		} else {
			delete(f.services, name)
			delete(f.enabled, fleetUnitKey{name, kind, system})
		}
	case verb == "install":
		f.services[name] = []ServiceUnit{unit(kind, system)}
		f.enabled[fleetUnitKey{name, kind, system}] = true
	case verb == "enable":
		f.enabled[fleetUnitKey{name, kind, system}] = true
	case (verb == "start" || verb == "restart") && name == "default":
		f.writePidRecord()
		runtimePath := filepath.Join(f.root, "gateway_state.json")
		runtime := pyjson.NewObject()
		if raw, err := os.ReadFile(runtimePath); err == nil {
			if v, err := pyjson.Loads(string(raw)); err == nil {
				runtime = v.(*pyjson.Object)
			}
		}
		runtime.Set("pid", int64(f.gatewayPid))
		runtime.Set("hermes_home", f.root)
		runtime.Set("gateway_state", "running")
		if flag := f.configFlag(); flag != nil && *flag {
			runtime.Set("served_profiles", []any{"default", "coder", "ops"})
		}
		f.writeJSON(runtimePath, runtime)
	}
	return nil
}

func (f *migrateFleet) writeJSON(path string, v any) {
	text, err := pyjson.Dumps(v)
	if err != nil {
		f.t.Fatal(err)
	}
	fleetWrite(f.t, path, text)
}

func (f *migrateFleet) writePidRecord() {
	f.writeJSON(filepath.Join(f.root, "gateway.pid"), pyjson.ObjectOf("pid", int64(f.gatewayPid), "hermes_home", f.root))
}

func (f *migrateFleet) fakeSpawn(string) bool {
	f.writePidRecord()
	return true
}

func pidOfStartedGateway(h string) int {
	raw, err := os.ReadFile(filepath.Join(h, "gateway.pid"))
	if err != nil {
		return 0
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return 0
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return 0
	}
	pid, _ := obj.GetOr("pid", nil).(int64)
	return int(pid)
}

// gateway.multiplex_profiles of the root config.yaml, nil when unset
func (f *migrateFleet) configFlag() *bool {
	doc, err := config.ReadDocument(filepath.Join(f.root, "config.yaml"))
	if err != nil {
		f.t.Fatal(err)
	}
	section, _ := doc["gateway"].(map[string]any)
	v, ok := section["multiplex_profiles"].(bool)
	if !ok {
		return nil
	}
	return &v
}

func (f *migrateFleet) manifestExists() bool {
	_, err := os.Stat(filepath.Join(f.root, MigrationManifestName))
	return err == nil
}

func (f *migrateFleet) plan() *MigrationPlan {
	f.t.Helper()
	plan, err := BuildMigrationPlan(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return plan
}

func (f *migrateFleet) apply(plan *MigrationPlan, wait float64) bool {
	f.t.Helper()
	ok, err := ApplyMigration(f.ctx, f.out, plan, time.Duration(wait*float64(time.Second)))
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *migrateFleet) rollback() bool {
	f.t.Helper()
	ok, err := RollbackMigration(f.ctx, f.out, f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *migrateFleet) autoMigrate() {
	f.t.Helper()
	if err := MaybeAutoMigrateAfterUpdate(f.ctx, f.out); err != nil {
		f.t.Fatal(err)
	}
}

// Exit code of cmd_migrate, -1 when it returned without exiting
func (f *migrateFleet) cmdMigrate(dryRun, yes bool) int {
	f.t.Helper()
	err := CmdMigrate(f.ctx, f.out, map[string]any{"multiplex": true, "dry_run": dryRun, "yes": yes})
	if err == nil {
		return -1
	}
	exit, ok := err.(*gateway.SystemExitError)
	if !ok {
		f.t.Fatal(err)
	}
	return exit.Code
}

// Captured output since the last read
func (f *migrateFleet) readOut() string {
	s := f.out.String()
	f.out.Reset()
	return s
}

// Applies and returns the manifest as written, read mid flight because a confirmed convergence deletes it
func (f *migrateFleet) applyCapturingManifest(plan *MigrationPlan, wait float64, restore bool) (bool, *pyjson.Object) {
	f.t.Helper()
	var seen *pyjson.Object
	real := migrateWaitForServed
	migrateWaitForServed = func(ctx context.Context, defaultHome string, expected map[string]bool, timeout time.Duration) ([]string, bool, error) {
		seen = readManifest(defaultHome)
		return real(ctx, defaultHome, expected, timeout)
	}
	defer func() { migrateWaitForServed = real }()
	ok := f.apply(plan, wait)
	if restore {
		if err := config.AtomicJSONWrite(f.ctx, manifestPath(plan.DefaultHome), seen); err != nil {
			f.t.Fatal(err)
		}
	}
	return ok, seen
}

func (f *migrateFleet) writeManifest(manifest any) {
	if err := config.AtomicJSONWrite(f.ctx, manifestPath(f.root), manifest); err != nil {
		f.t.Fatal(err)
	}
}

func (f *migrateFleet) removeFiles(names ...string) {
	for _, n := range names {
		os.Remove(filepath.Join(f.root, n))
	}
}

func (f *migrateFleet) servicesEqual(want map[string][]ServiceUnit) bool {
	if len(want) != len(f.services) {
		return false
	}
	for k, v := range want {
		got := f.services[k]
		if len(got) != len(v) {
			return false
		}
		for i := range v {
			if got[i] != v[i] {
				return false
			}
		}
	}
	return true
}

func standaloneNames(plan *MigrationPlan) []string {
	out := []string{}
	for _, p := range plan.StandaloneSecondaries() {
		out = append(out, p.Name)
	}
	return out
}

func opsFor(ops [][2]string, keep func(name, verb string) bool) [][2]string {
	out := [][2]string{}
	for _, op := range ops {
		if keep(op[0], op[1]) {
			out = append(out, op)
		}
	}
	return out
}

func hasOp(ops [][2]string, name, verb string) bool {
	return len(opsFor(ops, func(n, v string) bool { return n == name && v == verb })) > 0
}

func anyContains(items []string, subs ...string) bool {
	for _, item := range items {
		all := true
		for _, s := range subs {
			all = all && strings.Contains(item, s)
		}
		if all {
			return true
		}
	}
	return false
}

// The real service leg and preflight, captured before any fixture fakes the seams
var (
	realServiceOp      = migrateServiceOp
	realPreflightApply = migratePreflightApply
)
