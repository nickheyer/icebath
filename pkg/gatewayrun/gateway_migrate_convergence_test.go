package gatewayrun

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestPreflightNeverImportsAParkedProfilesPlugins(t *testing.T) {
	f := newMigrateFleet(t)
	h := filepath.Join(f.root, "profiles/coder")
	fleetWrite(t, filepath.Join(h, "gateway.parked"), "")
	fleetWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [p1]\n")
	marker := filepath.Join(f.root, "p1-registered")
	plugin := filepath.Join(h, "plugins/p1")
	fleetWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: p1\n")
	fleetWrite(t, filepath.Join(plugin, "__init__.py"),
		"from pathlib import Path\n\ndef register(ctx):\n    Path("+pyReprString(marker)+").write_text('imported')\n")
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	plan := f.plan()
	if !reflect.DeepEqual(standaloneNames(plan), []string{"coder", "ops"}) {
		t.Fatalf("standalone %v", standaloneNames(plan))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("migration preflight imported a parked profile's plugin")
	}
	if _, err := DuplicateCredentialFindings(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("duplicate credential findings imported a parked profile's plugin")
	}
}

func pyReprString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

func TestEveryInstalledUnitOfASecondaryIsRemovedAndRecorded(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["coder"] = []ServiceUnit{unit("systemd", false), unit("systemd", true)}
	plan := f.plan()
	for _, p := range plan.StandaloneSecondaries() {
		if p.Name == "coder" && !reflect.DeepEqual(p.Services, []ServiceUnit{unit("systemd", false), unit("systemd", true)}) {
			t.Fatalf("coder services %v", p.Services)
		}
	}
	ok, manifest := f.applyCapturingManifest(plan, 5, true)
	if !ok || f.services["coder"] != nil {
		t.Fatalf("ok %v services %v", ok, f.services)
	}
	secondaries, _ := manifestSecondaries(manifest)
	var coderRec *pyjson.Object
	for _, r := range secondaries {
		if r.GetOr("profile", nil) == "coder" {
			coderRec = r
		}
	}
	if got := recordedServices(coderRec); !reflect.DeepEqual(got, []ServiceUnit{unit("systemd", false), unit("systemd", true)}) {
		t.Fatalf("recorded %v", got)
	}
	f.readOut()
	if !f.rollback() {
		t.Fatal(f.readOut())
	}
	if got := opsFor(f.ops, func(n, v string) bool { return n == "coder" && (v == "install" || v == "start") }); len(got) != 0 {
		t.Fatalf("the compensator restores one gateway, not a two-unit per-profile fleet %v", got)
	}
	f.removeFiles(MigrationManifestName, "gateway.pid", "gateway_state.json")
	fleetWrite(t, filepath.Join(f.root, "config.yaml"), "model:\n  default: x\n")
	f.services["default"] = []ServiceUnit{unit("systemd", false)}
	f.services["coder"] = []ServiceUnit{unit("systemd", false), unit("systemd", true)}
	f.pids["coder"], f.pids["ops"] = 4101, 4102
	f.ops = nil
	f.autoMigrate()
	if out := f.readOut(); !strings.Contains(out, "more than one installed service") || len(f.ops) != 0 {
		t.Fatalf("ops %v\n%s", f.ops, out)
	}
}

func TestUnresolvableSystemUnitUserIsUnknownPrincipalNotDirectoryOwner(t *testing.T) {
	f := newMigrateFleet(t)
	f.systemUnitDir = filepath.Join(f.tmp, "system")
	coderHome := filepath.Join(f.root, "profiles/coder")
	scoped, restore := HomeEnv(f.ctx, coderHome)
	unitPath, err := migrateSystemdUnitPath(scoped, true)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	fleetWrite(t, unitPath, "[Service]\nUser=nobody-such-user-xyz\n")
	uid, _, err := gatewayIdentity(f.ctx, coderHome, 0, []ServiceUnit{unit("systemd", true)})
	if err != nil || uid != nil {
		t.Fatalf("uid %v err %v", uid, err)
	}
	info, _ := os.Stat(coderHome)
	owner, _ := statUID(info)
	if uid, _, _ := gatewayIdentity(f.ctx, coderHome, 0, []ServiceUnit{unit("systemd", false)}); uid == nil || *uid != owner {
		t.Fatalf("user scope uid %v", uid)
	}
	f.services["default"] = []ServiceUnit{unit("systemd", true)}
	f.services["coder"] = []ServiceUnit{unit("systemd", true)}
	swapMigrateSeam(t, &migrateGatewayIdentity, func(_ context.Context, h string, _ int, _ []ServiceUnit) (*int, string, error) {
		if migrateProfileName(h) == "coder" {
			return nil, h, nil
		}
		known := 1000
		return &known, h, nil
	})
	f.autoMigrate()
	out := f.readOut()
	if !strings.Contains(out, "cannot be resolved") || !strings.Contains(out, MigrateCommand) || len(f.ops) != 0 || f.configFlag() != nil {
		t.Fatalf("ops %v\n%s", f.ops, out)
	}
}

func TestOptOutReadsEffectiveConfigManagedFalseWinsAndStringFalseIsFalse(t *testing.T) {
	f := newMigrateFleet(t)
	if section, _ := config.Defaults()["gateway"].(map[string]any); section["auto_multiplex_migration"] != true {
		t.Fatal("declared default is not true")
	}
	optedOut := func() bool {
		out, err := AutoMigrationOptedOut(f.ctx, f.root)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if optedOut() {
		t.Fatal("absent key opted out")
	}
	cfgPath := filepath.Join(f.root, "config.yaml")
	fleetWrite(t, cfgPath, "model:\n  default: x\ngateway:\n  auto_multiplex_migration: 'false'\n")
	if !optedOut() {
		t.Fatal("string false is not an opt out")
	}
	fleetWrite(t, cfgPath, "model:\n  default: x\ngateway:\n  auto_multiplex_migration: true\n")
	if optedOut() {
		t.Fatal("true opted out")
	}
	managed := filepath.Join(f.tmp, "managed")
	fleetWrite(t, filepath.Join(managed, "config.yaml"), "gateway:\n  auto_multiplex_migration: false\n")
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	scoped, restore := HomeEnv(f.ctx, f.root)
	cfg, err := config.LoadReadonly(scoped, f.root, nil)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	if section, _ := cfg["gateway"].(map[string]any); section["auto_multiplex_migration"] != false {
		t.Fatalf("managed value %v", section["auto_multiplex_migration"])
	}
	if !optedOut() {
		t.Fatal("managed false did not win")
	}
	f.autoMigrate()
	if len(f.ops) != 0 || f.configFlag() != nil {
		t.Fatalf("ops %v", f.ops)
	}
}

func TestInterruptionAfterTheDefaultUnitExistsIsStillInterruptedNotAlreadyMultiplexed(t *testing.T) {
	for _, preinstalled := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "preinstalled"}[preinstalled], func(t *testing.T) {
			f := newMigrateFleet(t)
			if preinstalled {
				f.services["default"] = []ServiceUnit{unit("systemd", false)}
			}
			realOp := migrateServiceOp
			migrateServiceOp = func(ctx context.Context, w io.Writer, kind string, system bool, verb, h, runAs string) error {
				if (verb == "start" || verb == "restart") && migrateProfileName(h) == "default" {
					panic(errKeyboardInterrupt)
				}
				return realOp(ctx, w, kind, system, verb, h, runAs)
			}
			interrupted := applyInterrupted(f, f.plan())
			migrateServiceOp = realOp
			if !interrupted {
				t.Fatal("interrupt swallowed")
			}
			if flag := f.configFlag(); flag == nil || !*flag || !f.servicesEqual(map[string][]ServiceUnit{"default": {unit("systemd", false)}}) || !f.manifestExists() {
				t.Fatalf("services %v manifest %v", f.services, f.manifestExists())
			}
			plan := f.plan()
			if !plan.Interrupted || plan.AlreadyMultiplexed() {
				t.Fatal("interrupted apply read as converged")
			}
			f.ops = nil
			f.readOut()
			if !f.apply(plan, 5) {
				t.Fatal(f.readOut())
			}
			if f.ops[len(f.ops)-1] != [2]string{"default", "restart"} || !strings.Contains(f.readOut(), "serves 3 profiles") {
				t.Fatalf("ops %v", f.ops)
			}
			if !f.plan().AlreadyMultiplexed() {
				t.Fatal("converged host not already multiplexed")
			}
		})
	}
}

func TestFailureAnywhereInTheDestructivePhaseRestoresTheRemovedSecondaries(t *testing.T) {
	for _, failing := range [][2]string{{"ops", "stop"}, {"coder", "uninstall"}, {"default", "flag"}} {
		t.Run(failing[0]+"_"+failing[1], func(t *testing.T) {
			f := newMigrateFleet(t)
			name, verb := failing[0], failing[1]
			realOp := migrateServiceOp
			swapMigrateSeam(t, &migrateServiceOp, func(ctx context.Context, w io.Writer, kind string, system bool, v, h, runAs string) error {
				if migrateProfileName(h) == name && v == verb {
					return &migrateRuntimeError{name + " " + verb + " failed"}
				}
				return realOp(ctx, w, kind, system, v, h, runAs)
			})
			if verb == "flag" {
				realFlag := migrateWriteMultiplexFlag
				swapMigrateSeam(t, &migrateWriteMultiplexFlag, func(ctx context.Context, h string, value bool) error {
					if value {
						return errors.New("read-only config")
					}
					return realFlag(ctx, h, value)
				})
			}
			if f.apply(f.plan(), 0.1) {
				t.Fatal("failed apply reported success")
			}
			out := f.readOut()
			if !strings.Contains(out, "Restoring one host gateway") || !strings.Contains(out, "Compensated: one host gateway is running") {
				t.Fatal(out)
			}
			if flag := f.configFlag(); flag != nil && *flag {
				t.Fatal("flag left on")
			}
			if got := opsFor(f.ops, func(n, v string) bool { return n != "default" && (v == "install" || v == "start") }); len(got) != 0 || f.manifestExists() {
				t.Fatalf("ops %v manifest %v", got, f.manifestExists())
			}
			after := f.plan()
			if after.Blocked() || !after.EligibleForMigration() {
				t.Fatalf("after %v %v", after.Blockers, standaloneNames(after))
			}
		})
	}
}

func TestKnownBringupRefusalIsRejectedBeforeAnySecondaryIsTouched(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["coder"] = []ServiceUnit{unit("systemd", true)}
	f.services["ops"] = []ServiceUnit{unit("systemd", true)}
	swapMigrateSeam(t, &migrateSystemdServiceUser, func(context.Context, string, []ServiceUnit) (string, error) { return "", nil })
	swapMigrateSeam(t, &migratePreflightApply, realPreflightApply)
	swapMigrateSeam(t, &migrateRequireRoot, func(string) error { return nil })
	for _, v := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		t.Setenv(v, "root")
	}
	if f.apply(f.plan(), 0.1) {
		t.Fatal("refusal applied")
	}
	out := f.readOut()
	if !strings.Contains(out, "before changing anything") || !strings.Contains(out, "--run-as-user root") {
		t.Fatal(out)
	}
	if len(f.ops) != 0 || f.configFlag() != nil || f.manifestExists() {
		t.Fatalf("ops %v", f.ops)
	}
}

func TestUnknownDefaultSystemPrincipalBlocksTheUpdateHook(t *testing.T) {
	f := newMigrateFleet(t)
	f.systemUnitDir = filepath.Join(f.tmp, "system")
	writeUnit := func(h, text string) {
		scoped, restore := HomeEnv(f.ctx, h)
		p, err := migrateSystemdUnitPath(scoped, true)
		restore()
		if err != nil {
			t.Fatal(err)
		}
		fleetWrite(t, p, text)
	}
	writeUnit(f.root, "[Service]\nUser=no-such-pr111062-user\n")
	for _, name := range []string{"coder", "ops"} {
		writeUnit(filepath.Join(f.root, "profiles", name), "[Service]\nUser=root\n")
	}
	f.services = map[string][]ServiceUnit{"default": {unit("systemd", true)}, "coder": {unit("systemd", true)}, "ops": {unit("systemd", true)}}
	f.pids = map[string]int{}
	plan := f.plan()
	if plan.Default().UID != nil {
		t.Fatalf("default uid %v", *plan.Default().UID)
	}
	for _, p := range plan.StandaloneSecondaries() {
		if p.UID == nil || *p.UID != 0 {
			t.Fatalf("%s uid %v", p.Name, p.UID)
		}
	}
	if uid, _, _ := gatewayIdentity(f.ctx, f.root, 0, []ServiceUnit{unit("systemd", true)}); uid != nil {
		t.Fatalf("root identity %v", *uid)
	}
	blockers := AutoMigrationBlockers(plan)
	if len(blockers) != 1 || !strings.Contains(blockers[0], "default gateway") || !strings.Contains(blockers[0], "cannot be resolved") {
		t.Fatalf("blockers %v", blockers)
	}
	f.autoMigrate()
	out := f.readOut()
	if !strings.Contains(out, "cannot be resolved") || !strings.Contains(out, MigrateCommand) || len(f.ops) != 0 || f.configFlag() != nil {
		t.Fatalf("ops %v\n%s", f.ops, out)
	}
	swapMigrateSeam(t, &migrateGatewayIdentity, func(_ context.Context, h string, _ int, _ []ServiceUnit) (*int, string, error) {
		zero := 0
		return &zero, h, nil
	})
	if b := AutoMigrationBlockers(f.plan()); len(b) != 0 {
		t.Fatalf("same uid refused %v", b)
	}
	migrateGatewayIdentity = func(_ context.Context, h string, _ int, _ []ServiceUnit) (*int, string, error) {
		uid := 1000
		if migrateProfileName(h) == "default" {
			uid = 0
		}
		return &uid, h, nil
	}
	if !anyContains(AutoMigrationBlockers(f.plan()), "UNIX privilege boundary") {
		t.Fatal("different uid folded")
	}
}

func TestMigrateLeavesExactlyOneHostUnitServingEveryProfileAndIsIdempotent(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["default"] = []ServiceUnit{unit("systemd", false)}
	if code := f.cmdMigrate(false, true); code != 0 {
		t.Fatalf("exit %d\n%s", code, f.readOut())
	}
	if !f.servicesEqual(map[string][]ServiceUnit{"default": {unit("systemd", false)}}) || len(f.pids) != 0 {
		t.Fatalf("services %v pids %v", f.services, f.pids)
	}
	raw, _ := os.ReadFile(filepath.Join(f.root, "gateway_state.json"))
	v, _ := pyjson.Loads(string(raw))
	served := []string{}
	for _, s := range v.(*pyjson.Object).GetOr("served_profiles", nil).([]any) {
		served = append(served, s.(string))
	}
	sort.Strings(served)
	if !reflect.DeepEqual(served, []string{"coder", "default", "ops"}) {
		t.Fatalf("served %v", served)
	}
	f.readOut()
	f.ops = nil
	plan := f.plan()
	if !plan.AlreadyMultiplexed() || len(plan.StandaloneSecondaries()) != 0 {
		t.Fatal("not converged")
	}
	if code := f.cmdMigrate(false, true); code != -1 || len(f.ops) != 0 || !strings.Contains(f.readOut(), "already multiplexing") {
		t.Fatalf("exit %d ops %v", code, f.ops)
	}
}

func TestHalfMigratedHostConvergesOnAReRun(t *testing.T) {
	f := newMigrateFleet(t)
	fleetWrite(t, filepath.Join(f.root, "config.yaml"), "model:\n  default: x\ngateway:\n  multiplex_profiles: true\n")
	f.services = map[string][]ServiceUnit{"default": {unit("systemd", false)}, "coder": {unit("systemd", false)}}
	f.pids = map[string]int{"coder": 4101}
	plan := f.plan()
	if !plan.MultiplexFlagOn || plan.AlreadyMultiplexed() || !reflect.DeepEqual(standaloneNames(plan), []string{"coder"}) {
		t.Fatal("a host with a live per-profile gateway is not converged")
	}
	if code := f.cmdMigrate(false, true); code != 0 {
		t.Fatalf("exit %d\n%s", code, f.readOut())
	}
	if !f.servicesEqual(map[string][]ServiceUnit{"default": {unit("systemd", false)}}) || len(f.pids) != 0 {
		t.Fatalf("services %v", f.services)
	}
	out := f.readOut()
	if !strings.Contains(out, "Half-migrated host") || !strings.Contains(out, "serves 3 profiles") || !f.plan().AlreadyMultiplexed() {
		t.Fatal(out)
	}
}

func TestMigrateEnablesTheSurvivorBeforeItRemovesAnything(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["default"] = []ServiceUnit{unit("systemd", false)}
	if f.enabled[fleetUnitKey{"default", "systemd", false}] {
		t.Fatal("survivor enabled up front")
	}
	if code := f.cmdMigrate(false, true); code != 0 {
		t.Fatalf("exit %d\n%s", code, f.readOut())
	}
	firstRemoval, enable := -1, -1
	for i, op := range f.ops {
		if firstRemoval < 0 && (op[1] == "stop" || op[1] == "uninstall") {
			firstRemoval = i
		}
		if enable < 0 && op == [2]string{"default", "enable"} {
			enable = i
		}
	}
	if enable < 0 || enable >= firstRemoval {
		t.Fatalf("ops %v", f.ops)
	}
	if !reflect.DeepEqual(f.enabled, map[fleetUnitKey]bool{{"default", "systemd", false}: true}) {
		t.Fatalf("enabled %v", f.enabled)
	}
	if f.ops[len(f.ops)-1] != [2]string{"default", "restart"} || !strings.Contains(f.readOut(), "serves 3 profiles") {
		t.Fatalf("ops %v", f.ops)
	}
}

func TestMigrateRefusesWhenTheSurvivorCannotBeEnabled(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["default"] = []ServiceUnit{unit("systemd", false)}
	swapMigrateSeam(t, &migrateRunSystemctl, func(context.Context, []string, bool, time.Duration) (gateway.CommandResult, error) {
		return gateway.CommandResult{ReturnCode: 1}, nil
	})
	fakeOp := migrateServiceOp
	swapMigrateSeam(t, &migrateServiceOp, func(ctx context.Context, w io.Writer, kind string, system bool, verb, h, runAs string) error {
		if verb == "enable" {
			return realServiceOp(ctx, w, kind, system, verb, h, runAs)
		}
		return fakeOp(ctx, w, kind, system, verb, h, runAs)
	})
	if code := f.cmdMigrate(false, true); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := opsFor(f.ops, func(_, v string) bool { return v == "stop" || v == "uninstall" }); len(got) != 0 {
		t.Fatalf("refusal must precede every removal %v", got)
	}
	if !reflect.DeepEqual(f.pids, map[string]int{"coder": 4101, "ops": 4102}) || f.configFlag() != nil || f.manifestExists() {
		t.Fatal("changed")
	}
	if !strings.Contains(f.readOut(), "refused before changing anything") {
		t.Fatal("refusal wording missing")
	}
}

func TestPlanNamesEveryProcessItWillSigtermBeforeItSignalsAnything(t *testing.T) {
	f := newMigrateFleet(t)
	f.cmdMigrate(true, true)
	out := f.readOut()
	if !strings.Contains(out, "SIGTERM") || !strings.Contains(out, "coder (pid 4101)") || !strings.Contains(out, "ops (pid 4102)") {
		t.Fatal(out)
	}
	if len(f.ops) != 0 || !reflect.DeepEqual(f.pids, map[string]int{"coder": 4101, "ops": 4102}) {
		t.Fatal("dry run changed something")
	}
}

func TestWindowsTaskDetectionReadsBothTheTaskAndTheStartupFallback(t *testing.T) {
	ctx := context.Background()
	for _, c := range [][3]bool{{true, false, true}, {false, true, true}, {false, false, false}} {
		task, startup := c[0], c[1]
		swapMigrateSeam(t, &migrateTaskRegistered, func(context.Context) (bool, error) { return task, nil })
		swapMigrateSeam(t, &migrateStartupEntryInstalled, func(context.Context) (bool, error) { return startup, nil })
		if got := windowsTaskInstalled(ctx); got != c[2] {
			t.Fatalf("task %v startup %v got %v", task, startup, got)
		}
	}
	swapMigrateSeam(t, &migrateTaskRegistered, func(context.Context) (bool, error) { return false, errors.New("schtasks") })
	if windowsTaskInstalled(ctx) {
		t.Fatal("a schtasks failure read as installed")
	}
}

func TestWindowsIsMigratableAndOnlyS6IsRefused(t *testing.T) {
	swapMigrateSeam(t, &MigrateS6.Running, func() bool { return false })
	if reason := migrateHostSupportsMigration(); reason != "" {
		t.Fatal(reason)
	}
	swapMigrateSeam(t, &MigrateS6.Running, func() bool { return true })
	swapMigrateSeam(t, &MigrateS6.Scandir, func() string { return filepath.Join(t.TempDir(), "run-service") })
	swapMigrateSeam(t, &MigrateS6.NamedSlotName, func(p string) string { return "gateway-" + p })
	if reason := migrateHostSupportsMigration(); reason == "" {
		t.Fatal("s6 without a root slot was not refused")
	}
}

func TestStandaloneProfileIsListedLeftAloneAndNotAFoldTarget(t *testing.T) {
	f := newMigrateFleet(t)
	fleetWrite(t, filepath.Join(f.root, "profiles/ops/config.yaml"), "gateway:\n  standalone: true\n")
	plan := f.plan()
	for _, p := range plan.Profiles {
		if p.Name == "ops" {
			t.Fatal("standalone profile in the inventory")
		}
	}
	if !reflect.DeepEqual(standaloneNames(plan), []string{"coder"}) || !reflect.DeepEqual(plan.StandaloneByConfig, []string{"ops"}) {
		t.Fatalf("standalone %v by config %v", standaloneNames(plan), plan.StandaloneByConfig)
	}
	text, err := pyjson.Dumps(plan.ToDict())
	if err != nil {
		t.Fatal(err)
	}
	v, _ := pyjson.Loads(text)
	payload := v.(*pyjson.Object)
	if !pyjson.Equal(payload.GetOr("standalone_by_config", nil), []any{"ops"}) {
		t.Fatalf("payload %v", payload.GetOr("standalone_by_config", nil))
	}
	for _, p := range payload.GetOr("profiles", nil).([]any) {
		if p.(*pyjson.Object).GetOr("profile", nil) == "ops" {
			t.Fatal("standalone profile in the payload")
		}
	}
	if !anyContains(FormatPlan(plan, true), "ops") {
		t.Fatal("plan does not name the standalone profile")
	}
}
