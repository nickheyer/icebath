package gatewayrun

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestDryRunAndBlockedPreflightChangeNothing(t *testing.T) {
	f := newMigrateFleet(t)
	plan := f.plan()
	if plan.Blocked() || !plan.EligibleForMigration() {
		t.Fatalf("blocked %v eligible %v %v", plan.Blocked(), plan.EligibleForMigration(), plan.Blockers)
	}
	if got := standaloneNames(plan); !reflect.DeepEqual(got, []string{"coder", "ops"}) {
		t.Fatalf("standalone %v", got)
	}
	if code := f.cmdMigrate(true, true); code != -1 {
		t.Fatalf("dry run exited %d", code)
	}
	if !strings.Contains(f.readOut(), "dry run") {
		t.Fatal("dry run header missing")
	}
	fleetWrite(t, filepath.Join(f.root, "profiles/coder/.env"), "TELEGRAM_BOT_TOKEN=111111:default-token\n")
	blocked := f.plan()
	if !blocked.Blocked() || !strings.Contains(blocked.Blockers[0], "profile_routes") || !strings.Contains(blocked.Blockers[0], "'coder'") {
		t.Fatalf("blockers %v", blocked.Blockers)
	}
	if code := f.cmdMigrate(false, true); code != 1 {
		t.Fatalf("blocked exit %d", code)
	}
	if len(f.ops) != 0 || !f.servicesEqual(map[string][]ServiceUnit{"coder": {unit("systemd", false)}, "ops": {unit("systemd", false)}}) {
		t.Fatalf("ops %v services %v", f.ops, f.services)
	}
	if !reflect.DeepEqual(f.pids, map[string]int{"coder": 4101, "ops": 4102}) || f.configFlag() != nil || f.manifestExists() {
		t.Fatalf("pids %v flag %v manifest %v", f.pids, f.configFlag(), f.manifestExists())
	}
	if !strings.Contains(f.readOut(), "nothing will be changed") {
		t.Fatal("refusal wording missing")
	}
}

func TestParkedProfileRemainsInMigrationInventory(t *testing.T) {
	f := newMigrateFleet(t)
	h := filepath.Join(f.root, "profiles/coder")
	fleetWrite(t, filepath.Join(h, "gateway.parked"), "")
	plan := f.plan()
	var coder *ProfileGateway
	for _, p := range plan.StandaloneSecondaries() {
		if p.Name == "coder" {
			coder = p
		}
	}
	if coder == nil || coder.Home != h || coder.PID != f.pids["coder"] || !reflect.DeepEqual(coder.Services, []ServiceUnit{unit("systemd", false)}) {
		t.Fatalf("coder %+v", coder)
	}
	fleetWrite(t, filepath.Join(h, ".env"), "TELEGRAM_BOT_TOKEN=111111:default-token\n")
	if !anyContains(f.plan().Blockers, "'coder'", "credential") {
		t.Fatal("parking hid the credential conflict")
	}
}

func TestMigrationRemovesParkedFootprintWithoutWaitingForItToServe(t *testing.T) {
	f := newMigrateFleet(t)
	h := filepath.Join(f.root, "profiles/coder")
	fleetWrite(t, filepath.Join(h, "gateway.parked"), "")
	serviceOp := migrateServiceOp
	swapMigrateSeam(t, &migrateServiceOp, func(ctx context.Context, w io.Writer, kind string, system bool, verb, home, runAs string) error {
		if err := serviceOp(ctx, w, kind, system, verb, home, runAs); err != nil {
			return err
		}
		if migrateProfileName(home) == "default" && (verb == "start" || verb == "restart") {
			path := filepath.Join(f.root, "gateway_state.json")
			raw, _ := os.ReadFile(path)
			v, _ := pyjson.Loads(string(raw))
			runtime := v.(*pyjson.Object)
			served, err := profiles.ProfilesToServe(ctx, true, false, false)
			if err != nil {
				return err
			}
			names := []any{}
			for _, p := range served {
				names = append(names, p.Name)
			}
			runtime.Set("served_profiles", names)
			f.writeJSON(path, runtime)
		}
		return nil
	})
	plan := f.plan()
	if !anyContains(FormatPlan(plan, true), "verify it serves 2 profiles") {
		t.Fatal("parked profile counted as expected served")
	}
	ok, manifest := f.applyCapturingManifest(plan, 0.1, false)
	if !ok {
		t.Fatal(f.readOut())
	}
	if _, has := f.pids["coder"]; has {
		t.Fatal("coder still running")
	}
	if _, has := f.services["coder"]; has {
		t.Fatal("coder unit kept")
	}
	if _, err := os.Stat(filepath.Join(h, "gateway.parked")); err != nil {
		t.Fatal("parked marker removed")
	}
	secondaries, _ := manifestSecondaries(manifest)
	names := map[string]bool{}
	for _, rec := range secondaries {
		names[rec.GetOr("profile", "").(string)] = true
	}
	if !reflect.DeepEqual(names, map[string]bool{"coder": true, "ops": true}) {
		t.Fatalf("manifest secondaries %v", names)
	}
	f.writeManifest(manifest)
	if f.plan().Interrupted {
		t.Fatal("parked profile flagged the manifest as interrupted")
	}
}

func TestApplyClearsTheManifestOnSuccessAndTheCompensatorRestores(t *testing.T) {
	f := newMigrateFleet(t)
	ok, manifest := f.applyCapturingManifest(f.plan(), 5, false)
	if !ok || f.manifestExists() {
		t.Fatalf("ok %v manifest kept %v\n%s", ok, f.manifestExists(), f.readOut())
	}
	secondaries, _ := manifestSecondaries(manifest)
	if len(secondaries) != 2 {
		t.Fatalf("secondaries %v", secondaries)
	}
	for _, rec := range secondaries {
		if !pyjson.Equal(rec.GetOr("service", nil), pyjson.ObjectOf("kind", "systemd", "system", false)) {
			t.Fatalf("service %v", rec.GetOr("service", nil))
		}
	}
	if flag := f.configFlag(); flag == nil || !*flag {
		t.Fatal("flag not on")
	}
	if len(f.pids) != 0 || f.services["coder"] != nil || f.services["ops"] != nil {
		t.Fatalf("pids %v services %v", f.pids, f.services)
	}
	if !reflect.DeepEqual(f.services["default"], []ServiceUnit{unit("systemd", false)}) || !hasOp(f.ops, "default", "install") || !hasOp(f.ops, "default", "start") {
		t.Fatalf("default %v ops %v", f.services["default"], f.ops)
	}
	if !strings.Contains(f.readOut(), "serves 3 profiles") {
		t.Fatal("served line missing")
	}
	again := f.plan()
	if !again.AlreadyMultiplexed() || !f.apply(again, 90) {
		t.Fatal("second run not idempotent")
	}
	runtimePath := filepath.Join(f.root, "gateway_state.json")
	raw, _ := os.ReadFile(runtimePath)
	v, _ := pyjson.Loads(string(raw))
	runtime := v.(*pyjson.Object)
	runtime.Set("platforms", pyjson.ObjectOf("telegram", pyjson.ObjectOf("state", "connected"),
		"coder:telegram", pyjson.ObjectOf("state", "connected"), "ops:discord", pyjson.ObjectOf("state", "connected")))
	f.writeJSON(runtimePath, runtime)
	f.ops = nil
	f.writeManifest(manifest)
	if !f.rollback() {
		t.Fatal(f.readOut())
	}
	if flag := f.configFlag(); flag == nil || *flag {
		t.Fatal("flag not restored to false")
	}
	if !f.servicesEqual(map[string][]ServiceUnit{"default": {unit("systemd", false)}}) {
		t.Fatalf("services %v", f.services)
	}
	if len(opsFor(f.ops, func(n, _ string) bool { return n != "default" })) != 0 || f.ops[len(f.ops)-1] != [2]string{"default", "restart"} {
		t.Fatalf("ops %v", f.ops)
	}
	if !strings.Contains(f.readOut(), "coder, ops") {
		t.Fatal("removed secondaries not named")
	}
	raw, _ = os.ReadFile(runtimePath)
	v, _ = pyjson.Loads(string(raw))
	runtime = v.(*pyjson.Object)
	if !pyjson.Equal(runtime.GetOr("served_profiles", nil), []any{}) ||
		!pyjson.Equal(runtime.GetOr("platforms", nil), pyjson.ObjectOf("telegram", pyjson.ObjectOf("state", "connected"))) {
		t.Fatalf("runtime %v", runtime)
	}
	coder := "coder"
	if served, _ := gateway.NamedProfileServedByRunningMultiplexerCtx(f.ctx, &coder); served {
		t.Fatal("coder still served by the multiplexer record")
	}
	if f.manifestExists() {
		t.Fatal("manifest kept")
	}
}

func TestMigrationPreservesRootSystemServiceUserForDefaultInstall(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["coder"] = []ServiceUnit{unit("systemd", true)}
	swapMigrateSeam(t, &migrateSystemdServiceUser, func(_ context.Context, h string, services []ServiceUnit) (string, error) {
		if migrateProfileName(h) == "coder" && containsUnit(services, unit("systemd", true)) {
			return "root", nil
		}
		return "", nil
	})
	plan := f.plan()
	for _, p := range plan.StandaloneSecondaries() {
		if p.Name == "coder" && p.RunAsUser != "root" {
			t.Fatalf("run as %q", p.RunAsUser)
		}
	}
	type install struct {
		kind   string
		system bool
		runAs  string
	}
	installs := []install{}
	swapMigrateSeam(t, &migrateServiceOp, func(_ context.Context, _ io.Writer, kind string, system bool, verb, h, runAs string) error {
		name := migrateProfileName(h)
		if name == "default" && verb == "install" {
			installs = append(installs, install{kind, system, runAs})
		}
		if verb == "uninstall" {
			delete(f.services, name)
		}
		if verb == "install" {
			f.services[name] = []ServiceUnit{unit(kind, system)}
		}
		if (verb == "start" || verb == "restart") && name == "default" {
			fleetWrite(t, filepath.Join(f.root, "gateway_state.json"), `{"served_profiles": ["default", "coder", "ops"]}`)
		}
		return nil
	})
	swapMigrateSeam(t, &migrateWaitForServed, func(context.Context, string, map[string]bool, time.Duration) ([]string, bool, error) {
		return []string{"default", "coder", "ops"}, true, nil
	})
	if !f.apply(plan, 0.1) {
		t.Fatal(f.readOut())
	}
	if !reflect.DeepEqual(installs, []install{{"systemd", true, "root"}}) {
		t.Fatalf("installs %v", installs)
	}
}

func TestRollbackThatCannotBringTheGatewayBackReportsFailureAndKeepsTheManifest(t *testing.T) {
	f := newMigrateFleet(t)
	if ok, _ := f.applyCapturingManifest(f.plan(), 5, true); !ok {
		t.Fatal(f.readOut())
	}
	f.ops = nil
	realOp := migrateServiceOp
	swapMigrateSeam(t, &migrateServiceOp, func(ctx context.Context, w io.Writer, kind string, system bool, verb, h, runAs string) error {
		if (verb == "start" || verb == "restart") && migrateProfileName(h) == "default" {
			os.Remove(filepath.Join(f.root, "gateway.pid"))
			return nil
		}
		return realOp(ctx, w, kind, system, verb, h, runAs)
	})
	swapMigrateSeam(t, &compensatorWait, 500*time.Millisecond)
	f.readOut()
	if f.rollback() {
		t.Fatal("rollback reported success over a gateway that never came up")
	}
	out := f.readOut()
	if !strings.Contains(out, "no gateway confirmed serving this host") || !strings.Contains(out, "Compensation incomplete") {
		t.Fatal(out)
	}
	if flag := f.configFlag(); flag == nil || *flag {
		t.Fatal("flag not restored")
	}
	if len(opsFor(f.ops, func(n, _ string) bool { return n != "default" })) != 0 || !f.manifestExists() {
		t.Fatalf("ops %v manifest %v", f.ops, f.manifestExists())
	}
}

func TestRollbackNeverRespawnsADetachedSecondary(t *testing.T) {
	f := newMigrateFleet(t)
	secondaries := []any{}
	for _, n := range []string{"coder", "ops"} {
		secondaries = append(secondaries, pyjson.ObjectOf("profile", n, "home", filepath.Join(f.root, "profiles", n), "pid", int64(4100), "service", nil))
	}
	f.writeManifest(pyjson.ObjectOf("version", int64(1), "flag_was", false, "default", pyjson.ObjectOf("service", nil), "secondaries", secondaries))
	f.services = map[string][]ServiceUnit{}
	f.pids = map[string]int{}
	spawned := []string{}
	swapMigrateSeam(t, &migrateSpawnDetachedGateway, func(_ context.Context, h string) (bool, error) {
		spawned = append(spawned, migrateProfileName(h))
		f.pids[migrateProfileName(h)] = 5000 + len(spawned)
		return f.fakeSpawn(h), nil
	})
	if !f.rollback() || !reflect.DeepEqual(spawned, []string{"default"}) {
		t.Fatalf("spawned %v\n%s", spawned, f.readOut())
	}
}

func TestMalformedManifestIsRefusedBeforeAnyMutation(t *testing.T) {
	f := newMigrateFleet(t)
	fleetWrite(t, filepath.Join(f.root, MigrationManifestName), `{"version": 1, "flag_was": false, "default": [], "secondaries": [{"home": "x"}]}`)
	if f.rollback() || f.configFlag() != nil || len(f.ops) != 0 {
		t.Fatal("malformed manifest acted on")
	}
	if !strings.Contains(f.readOut(), "fix or delete the manifest") {
		t.Fatal("refusal wording missing")
	}
}

func TestSecondaryPortBinderIsNoticeWithIngressAndBlockerWithout(t *testing.T) {
	f := newMigrateFleet(t)
	fleetWrite(t, filepath.Join(f.root, "profiles/ops/.env"), "DISCORD_BOT_TOKEN=ops-discord-333333\nAPI_SERVER_KEY=ops-api-key-abcdef\n")
	fleetWrite(t, filepath.Join(f.root, "profiles/ops/config.yaml"), "platforms:\n  api_server:\n    enabled: true\n    extra:\n      port: 9999\n")
	plan := f.plan()
	if plan.Blocked() || !anyContains(plan.Notices, "api_server", "/p/ops/") {
		t.Fatalf("blockers %v notices %v", plan.Blockers, plan.Notices)
	}
	swapMigrateSeam(t, &platformServesProfilePrefix, func(context.Context, string) bool { return false })
	plan = f.plan()
	if !plan.Blocked() || !strings.Contains(plan.Blockers[0], "api_server") || !strings.Contains(plan.Blockers[0], "/p/ops/") {
		t.Fatalf("blockers %v", plan.Blockers)
	}
}

func TestServesProfilePrefixIsReadFromAdapterClasses(t *testing.T) {
	ctx := context.Background()
	for _, platform := range []string{"api_server", "webhook"} {
		spec := gateway.BuiltinAdapters[gateway.Platform(platform)]
		cls, _, err := gateway.BuiltinAdapterImport(spec.Module, spec.AdapterName, spec.Requirement)
		if err != nil || cls == nil || !cls.Traits.ServesProfilePrefix {
			t.Fatalf("%s class trait %v %v", platform, cls, err)
		}
		if !platformServesProfilePrefix(ctx, platform) {
			t.Fatalf("%s not served at the prefix", platform)
		}
	}
	for _, platform := range []string{"sms", "line", "teams", "bluebubbles", "whatsapp_cloud", "msgraph_webhook"} {
		if !platformServesProfilePrefix(ctx, platform) {
			t.Errorf("%s not served at the prefix", platform)
		}
	}
	if platformServesProfilePrefix(ctx, "telegram") {
		t.Fatal("outbound only adapter declared the prefix")
	}
}

func TestUpdateHookMigratesWhenUnblockedAndOnlyWarnsWhenBlocked(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["default"] = []ServiceUnit{unit("systemd", false)}
	f.autoMigrate()
	out := f.readOut()
	if !strings.Contains(out, "Migrating per-profile gateways") || !strings.Contains(out, "serves 3 profiles") {
		t.Fatal(out)
	}
	if flag := f.configFlag(); flag == nil || !*flag || f.manifestExists() {
		t.Fatal("not migrated")
	}
	f.removeFiles("gateway.pid", "gateway_state.json", MigrationManifestName)
	fleetWrite(t, filepath.Join(f.root, "config.yaml"), "model:\n  default: x\n")
	f.services["coder"] = []ServiceUnit{unit("systemd", false)}
	f.services["default"] = []ServiceUnit{unit("systemd", false)}
	f.pids["coder"] = 4101
	f.ops = nil
	fleetWrite(t, filepath.Join(f.root, "profiles/coder/.env"), "TELEGRAM_BOT_TOKEN=111111:default-token\n")
	f.autoMigrate()
	out = f.readOut()
	if !strings.Contains(out, MigrateCommand) || !strings.Contains(out, "profile_routes") {
		t.Fatal(out)
	}
	if len(f.ops) != 0 || f.configFlag() != nil {
		t.Fatalf("ops %v flag %v", f.ops, f.configFlag())
	}
}

func TestUpdateHookNeverTouchesSingleProfileOrAlreadyMultiplexed(t *testing.T) {
	f := newMigrateFleet(t)
	f.services = map[string][]ServiceUnit{}
	f.pids = map[string]int{}
	f.autoMigrate()
	if out := f.readOut(); out != "" || f.configFlag() != nil {
		t.Fatalf("out %q flag %v", out, f.configFlag())
	}
}

func TestUpdateHookReadsAMaskedDefaultUnitAsAnOptOut(t *testing.T) {
	f := newMigrateFleet(t)
	name, err := migrateSystemdUnitPath(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	swapMigrateSeam(t, &migrateSystemdUnitPath, func(_ context.Context, system bool) (string, error) {
		if system {
			return filepath.Join(f.tmp, "none"), nil
		}
		return name, nil
	})
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.DevNull, name); err != nil {
		t.Fatal(err)
	}
	f.services["default"] = []ServiceUnit{unit("systemd", false)}
	f.autoMigrate()
	out := f.readOut()
	if !strings.Contains(out, "masked") || strings.Contains(out, "refused") || !strings.Contains(out, MigrateCommand) {
		t.Fatal(out)
	}
	if len(f.ops) != 0 || f.configFlag() != nil {
		t.Fatalf("ops %v", f.ops)
	}
}

func TestUpdateHookRefusesToCrossServiceUserOrHomeBoundary(t *testing.T) {
	cases := []struct {
		service  ServiceUnit
		uid      int
		home     string
		expected string
	}{
		{unit("systemd", true), 1000, "profiles/coder", "different service domain"},
		{unit("launchd", false), 1000, "profiles/coder", "different service domain"},
		{unit("systemd", false), 2000, "profiles/coder", "UNIX privilege boundary"},
		{unit("systemd", false), 1000, "external", "outside"},
	}
	for _, c := range cases {
		t.Run(c.expected+c.service.Kind, func(t *testing.T) {
			f := newMigrateFleet(t)
			f.services["default"] = []ServiceUnit{unit("systemd", false)}
			f.services["ops"] = []ServiceUnit{c.service}
			opsHome := filepath.Join(f.root, c.home)
			if c.home == "external" {
				opsHome = filepath.Join(filepath.Dir(f.root), "external-ops")
			}
			swapMigrateSeam(t, &migrateGatewayIdentity, func(_ context.Context, h string, _ int, _ []ServiceUnit) (*int, string, error) {
				if migrateProfileName(h) == "ops" {
					uid := c.uid
					return &uid, opsHome, nil
				}
				uid := 1000
				return &uid, h, nil
			})
			f.autoMigrate()
			out := f.readOut()
			if !strings.Contains(out, c.expected) || !strings.Contains(out, MigrateCommand) || !strings.Contains(out, "'ops'") {
				t.Fatal(out)
			}
			if len(f.ops) != 0 || !reflect.DeepEqual(f.pids, map[string]int{"coder": 4101, "ops": 4102}) {
				t.Fatalf("ops %v pids %v", f.ops, f.pids)
			}
			if !f.servicesEqual(map[string][]ServiceUnit{"default": {unit("systemd", false)}, "coder": {unit("systemd", false)}, "ops": {c.service}}) {
				t.Fatalf("services %v", f.services)
			}
			if f.configFlag() != nil || f.manifestExists() {
				t.Fatal("changed")
			}
			plan := f.plan()
			if plan.Blocked() || !anyContains(plan.Notices, c.expected) {
				t.Fatalf("blockers %v notices %v", plan.Blockers, plan.Notices)
			}
			f.cmdMigrate(true, true)
			if !strings.Contains(f.readOut(), c.expected) {
				t.Fatal("dry run lacks the finding")
			}
		})
	}
}

func TestUpdateHookStillMigratesSameUserSameScopeProfilesUnderTheDefaultTree(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["default"] = []ServiceUnit{unit("systemd", false)}
	swapMigrateSeam(t, &migrateGatewayIdentity, func(_ context.Context, h string, _ int, _ []ServiceUnit) (*int, string, error) {
		uid := 1000
		return &uid, h, nil
	})
	f.autoMigrate()
	out := f.readOut()
	if !strings.Contains(out, "Migrating per-profile gateways") || !strings.Contains(out, "serves 3 profiles") {
		t.Fatal(out)
	}
	if flag := f.configFlag(); flag == nil || !*flag || !hasOp(f.ops, "ops", "uninstall") {
		t.Fatalf("ops %v", f.ops)
	}
}

func TestUpdateHookFoldsAUnitLessDefaultWhenEverySecondarySharesOneManager(t *testing.T) {
	f := newMigrateFleet(t)
	swapMigrateSeam(t, &migrateGatewayIdentity, func(_ context.Context, h string, _ int, _ []ServiceUnit) (*int, string, error) {
		uid := 1000
		return &uid, h, nil
	})
	f.services["coder"] = []ServiceUnit{unit("launchd", false)}
	f.services["ops"] = []ServiceUnit{unit("launchd", false)}
	plan := f.plan()
	if target := plan.TargetServiceKind(); target == nil || *target != unit("launchd", false) {
		t.Fatalf("target %v", target)
	}
	if blockers := AutoMigrationBlockers(plan); len(blockers) != 0 {
		t.Fatalf("blockers %v", blockers)
	}
	f.autoMigrate()
	out := f.readOut()
	if flag := f.configFlag(); !strings.Contains(out, "serves 3 profiles") || flag == nil || !*flag {
		t.Fatal(out)
	}
	if !hasOp(f.ops, "coder", "uninstall") || !hasOp(f.ops, "ops", "uninstall") || !hasOp(f.ops, "default", "install") ||
		!f.servicesEqual(map[string][]ServiceUnit{"default": {unit("launchd", false)}}) {
		t.Fatalf("ops %v services %v", f.ops, f.services)
	}
	f.services = map[string][]ServiceUnit{"default": {unit("systemd", false)}, "coder": {unit("launchd", false)}, "ops": {unit("launchd", false)}}
	f.pids["coder"], f.pids["ops"] = 4101, 4102
	blockers := AutoMigrationBlockers(f.plan())
	if len(blockers) == 0 {
		t.Fatal("a default under another manager folded")
	}
	for _, b := range blockers {
		if !strings.Contains(b, "different service domain") {
			t.Fatal(b)
		}
	}
	delete(f.services, "default")
	f.services["coder"] = []ServiceUnit{unit("launchd", false)}
	f.services["ops"] = []ServiceUnit{unit("systemd", false)}
	blockers = AutoMigrationBlockers(f.plan())
	if len(blockers) != 1 || !strings.Contains(blockers[0], "'ops'") || !strings.Contains(blockers[0], "different service domain") {
		t.Fatalf("blockers %v", blockers)
	}
}

func TestAutoMultiplexMigrationFalseOptsOutOfTheUpdateHookButNotTheExplicitCommand(t *testing.T) {
	f := newMigrateFleet(t)
	if !f.plan().EligibleForMigration() {
		t.Fatal("fleet not eligible")
	}
	cfgPath := filepath.Join(f.root, "config.yaml")
	fleetWrite(t, cfgPath, "model:\n  default: x\nauto_multiplex_migration: false\ngateway:\n  auto_multiplex_migration: false\n")
	f.autoMigrate()
	if out := f.readOut(); out != "" || len(f.ops) != 0 || f.configFlag() != nil {
		t.Fatalf("out %q ops %v", out, f.ops)
	}
	if !f.servicesEqual(map[string][]ServiceUnit{"coder": {unit("systemd", false)}, "ops": {unit("systemd", false)}}) ||
		!reflect.DeepEqual(f.pids, map[string]int{"coder": 4101, "ops": 4102}) || f.manifestExists() {
		t.Fatal("opted out fleet changed")
	}
	for _, text := range []string{"model:\n  default: x\nauto_multiplex_migration: false\n", "model:\n  default: x\n",
		"model:\n  default: x\ngateway:\n  auto_multiplex_migration: true\n"} {
		fleetWrite(t, cfgPath, text)
		if out, err := AutoMigrationOptedOut(f.ctx, f.root); err != nil || out {
			t.Fatalf("%q opted out %v %v", text, out, err)
		}
	}
	fleetWrite(t, cfgPath, "model:\n  default: x\ngateway:\n  auto_multiplex_migration: false\n")
	if code := f.cmdMigrate(false, true); code != 0 {
		t.Fatalf("explicit exit %d\n%s", code, f.readOut())
	}
	if flag := f.configFlag(); flag == nil || !*flag || f.manifestExists() {
		t.Fatal("explicit command did not migrate")
	}
}

func TestExplicitMigrateWithNoStandaloneSecondariesStillFlipsFlagAndRestartsDefault(t *testing.T) {
	f := newMigrateFleet(t)
	f.services = map[string][]ServiceUnit{}
	f.pids = map[string]int{}
	detached := false
	swapMigrateSeam(t, &migrateSpawnDetachedGateway, func(context.Context, string) (bool, error) {
		detached = true
		f.writePidRecord()
		f.writeJSON(filepath.Join(f.root, "gateway_state.json"), pyjson.ObjectOf("pid", int64(f.gatewayPid), "hermes_home", f.root,
			"gateway_state", "running", "served_profiles", []any{"default", "coder", "ops"}))
		return true, nil
	})
	if len(f.plan().StandaloneSecondaries()) != 0 {
		t.Fatal("standalone secondaries present")
	}
	var manifest *pyjson.Object
	realWait := migrateWaitForServed
	swapMigrateSeam(t, &migrateWaitForServed, func(ctx context.Context, h string, expected map[string]bool, timeout time.Duration) ([]string, bool, error) {
		if manifest == nil {
			manifest = readManifest(h)
		}
		return realWait(ctx, h, expected, timeout)
	})
	if code := f.cmdMigrate(false, true); code != 0 {
		t.Fatalf("exit %d\n%s", code, f.readOut())
	}
	if !detached {
		t.Fatal("default not started detached")
	}
	if flag := f.configFlag(); flag == nil || !*flag || f.manifestExists() {
		t.Fatal("flag or manifest wrong")
	}
	if !pyjson.Equal(manifest.GetOr("secondaries", nil), []any{}) || manifest.GetOr("flag_was", nil) != false || hasOp(f.ops, "default", "install") {
		t.Fatalf("manifest %v ops %v", manifest, f.ops)
	}
	if !strings.Contains(f.readOut(), "serves 3 profiles") {
		t.Fatal("served line missing")
	}
	f.writeManifest(manifest)
	if !f.rollback() {
		t.Fatal(f.readOut())
	}
	if flag := f.configFlag(); flag == nil || *flag {
		t.Fatal("flag not restored")
	}
}

func TestFailedDefaultBringupCompensatesToOneDetachedGateway(t *testing.T) {
	f := newMigrateFleet(t)
	realOp := migrateServiceOp
	swapMigrateSeam(t, &migrateServiceOp, func(ctx context.Context, w io.Writer, kind string, system bool, verb, h, runAs string) error {
		if verb == "install" && migrateProfileName(h) == "default" {
			return &gateway.ServiceUserError{Msg: "Refusing to install the gateway system service as root; pass --run-as-user root"}
		}
		return realOp(ctx, w, kind, system, verb, h, runAs)
	})
	if f.apply(f.plan(), 0.1) {
		t.Fatal("failed bring up reported success")
	}
	out := f.readOut()
	for _, want := range []string{"Restoring one host gateway", "Compensated: one host gateway is running", "falling back to a detached gateway", "coder, ops", "NOT reinstalled"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q\n%s", want, out)
		}
	}
	if flag := f.configFlag(); flag == nil || *flag || f.services["coder"] != nil || f.services["ops"] != nil || f.manifestExists() {
		t.Fatalf("flag %v services %v", flag, f.services)
	}
	after := f.plan()
	if after.Blocked() || len(after.StandaloneSecondaries()) != 0 {
		t.Fatalf("after %v %v", after.Blockers, standaloneNames(after))
	}
}

// Raises the stand in for a KeyboardInterrupt and reports whether it escaped apply
func applyInterrupted(f *migrateFleet, plan *MigrationPlan) (interrupted bool) {
	defer func() {
		if r := recover(); r != nil {
			interrupted = r == errKeyboardInterrupt
		}
	}()
	f.apply(plan, 0.1)
	return false
}

var errKeyboardInterrupt = errors.New("KeyboardInterrupt")

func TestInterruptedApplyIsResumedFromTheManifestNotShortCircuited(t *testing.T) {
	f := newMigrateFleet(t)
	f.services["coder"] = []ServiceUnit{unit("systemd", true)}
	swapMigrateSeam(t, &migrateSystemdServiceUser, func(_ context.Context, h string, _ []ServiceUnit) (string, error) {
		if migrateProfileName(h) == "coder" {
			return "root", nil
		}
		return "", nil
	})
	realRestart := migrateRestartDefault
	migrateRestartDefault = func(context.Context, io.Writer, *ProfileGateway, *ServiceUnit, string, string) (string, error) {
		panic(errKeyboardInterrupt)
	}
	interrupted := applyInterrupted(f, f.plan())
	migrateRestartDefault = realRestart
	if !interrupted {
		t.Fatal("interrupt swallowed")
	}
	if flag := f.configFlag(); flag == nil || !*flag || f.services["coder"] != nil || f.services["default"] != nil {
		t.Fatalf("flag %v services %v", flag, f.services)
	}
	type install struct {
		name, kind string
		system     bool
		runAs      string
	}
	installs := []install{}
	realOp := migrateServiceOp
	swapMigrateSeam(t, &migrateServiceOp, func(ctx context.Context, w io.Writer, kind string, system bool, verb, h, runAs string) error {
		if verb == "install" {
			installs = append(installs, install{migrateProfileName(h), kind, system, runAs})
		}
		return realOp(ctx, w, kind, system, verb, h, runAs)
	})
	plan := f.plan()
	if !plan.Interrupted || plan.AlreadyMultiplexed() {
		t.Fatal("interrupted apply read as converged")
	}
	if !f.apply(plan, 5) || !reflect.DeepEqual(installs, []install{{"default", "systemd", true, "root"}}) {
		t.Fatalf("installs %v\n%s", installs, f.readOut())
	}
	if !strings.Contains(f.readOut(), "serves 3 profiles") {
		t.Fatal("served line missing")
	}
}
