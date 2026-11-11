package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type modeFleet struct {
	ctx      context.Context
	root     string
	services map[string][]ServiceUnit
	pids     map[string]int
}

// default plus coder and ops with distinct bot tokens, nothing running a gateway unless a case says so
func newModeFleet(t *testing.T) *modeFleet {
	root := filepath.Join(t.TempDir(), "icebath")
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
	for _, name := range []string{"GATEWAY_MULTIPLEX_PROFILES", "TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "API_SERVER_KEY", "WEBHOOK_ENABLED"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	f := &modeFleet{ctx: context.Background(), root: root, services: map[string][]ServiceUnit{}, pids: map[string]int{}}
	swapMigrateSeam(t, &migrateInstalledServices, func(_ context.Context, h string) ([]ServiceUnit, error) {
		return f.services[migrateProfileName(h)], nil
	})
	swapMigrateSeam(t, &migrateLiveGatewayPid, func(h string) int { return f.pids[migrateProfileName(h)] })
	swapMigrateSeam(t, &migrateHostSupportsMigration, func() string { return "" })
	swapMigrateSeam(t, &MigrateS6.Running, func() bool { return false })
	return f
}

func (f *modeFleet) resolve(t *testing.T) (gateway.MultiplexDecision, *gateway.GatewayConfig) {
	t.Helper()
	cfg, err := gateway.LoadGatewayConfig(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := ResolveMultiplexMode(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return decision, cfg
}

func isTrue(b *bool) bool  { return b != nil && *b }
func isFalse(b *bool) bool { return b != nil && !*b }

func TestDefaultConfigIsOnButTheLoaderLeavesAnUnsetFlagUndecided(t *testing.T) {
	ctx := context.Background()
	load := func(raw map[string]any) *gateway.GatewayConfig {
		cfg, err := gateway.GatewayConfigFromDict(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if load(map[string]any{}).MultiplexProfiles != nil {
		t.Fatal("unset flag decided by the loader")
	}
	if !isFalse(load(map[string]any{"gateway": map[string]any{"multiplex_profiles": false}}).MultiplexProfiles) {
		t.Fatal("explicit false lost")
	}
	if !isTrue(load(map[string]any{"multiplex_profiles": "true"}).MultiplexProfiles) {
		t.Fatal("string true lost")
	}
}

func TestUnsetFlagMultiplexesAQuietFleetAndStaysStandaloneBesideALiveSecondary(t *testing.T) {
	f := newModeFleet(t)
	decision, cfg := f.resolve(t)
	if !decision.Enabled || decision.Source != "default" || !isTrue(cfg.MultiplexProfiles) {
		t.Fatalf("decision %+v", decision)
	}
	f.pids["coder"] = 4101
	decision, cfg = f.resolve(t)
	if decision.Enabled || decision.Source != "guard" || !strings.Contains(decision.Reason, "'coder'") ||
		!strings.Contains(decision.Reason, MigrateCommand) || !isFalse(cfg.MultiplexProfiles) {
		t.Fatalf("decision %+v", decision)
	}
	coder := "coder"
	if served, err := gateway.NamedProfileServedByRunningMultiplexerCtx(f.ctx, &coder); err != nil || served {
		t.Fatalf("coder served %v %v", served, err)
	}
	if on, err := gateway.DefaultGatewayMultiplexes(f.ctx, f.root); err != nil || on {
		t.Fatalf("default multiplexes %v %v", on, err)
	}
	f.pids = map[string]int{}
	f.services["ops"] = []ServiceUnit{unit("systemd", false)}
	if decision, _ := f.resolve(t); decision.Source != "guard" {
		t.Fatalf("installed unit not a blocker %+v", decision)
	}
}

func TestPreflightBlockerAndSingleProfileKeepTheUnsetDefaultStandalone(t *testing.T) {
	f := newModeFleet(t)
	fleetWrite(t, filepath.Join(f.root, "profiles/coder/.env"), "TELEGRAM_BOT_TOKEN=111111:default-token\n")
	if decision, _ := f.resolve(t); decision.Source != "guard" || !strings.Contains(decision.Reason, "profile_routes") {
		t.Fatalf("decision %+v", decision)
	}
	for _, sub := range []string{"profiles/coder", "profiles/ops"} {
		os.RemoveAll(filepath.Join(f.root, sub))
	}
	if decision, _ := f.resolve(t); decision != (gateway.MultiplexDecision{Enabled: false, Source: "guard", Reason: gateway.SingleProfileReason}) {
		t.Fatalf("decision %+v", decision)
	}
}

func TestStandaloneNamedProfileLeavesNothingToMultiplex(t *testing.T) {
	f := newModeFleet(t)
	os.RemoveAll(filepath.Join(f.root, "profiles/ops"))
	fleetWrite(t, filepath.Join(f.root, "profiles/coder/config.yaml"), "gateway:\n  standalone: true\n")
	if decision, _ := f.resolve(t); decision != (gateway.MultiplexDecision{Enabled: false, Source: "guard", Reason: gateway.SingleProfileReason}) {
		t.Fatalf("decision %+v", decision)
	}
}

func TestStandaloneLauncherNeverMultiplexesOtherProfiles(t *testing.T) {
	for _, flag := range []string{"", "  multiplex_profiles: true\n", "  multiplex_profiles: false\n"} {
		t.Run(strings.TrimSpace(flag), func(t *testing.T) {
			f := newModeFleet(t)
			solo := filepath.Join(f.root, "profiles/coder")
			fleetWrite(t, filepath.Join(solo, "config.yaml"), "gateway:\n  standalone: true\n"+flag)
			for _, h := range []string{solo, f.root, solo} {
				scoped := gateway.ProfileRuntimeScope(f.ctx, h, true)
				cfg, err := gateway.LoadGatewayConfig(scoped)
				if err != nil {
					t.Fatal(err)
				}
				decision, err := ResolveMultiplexMode(scoped, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if h == solo {
					if decision.Enabled || !isFalse(cfg.MultiplexProfiles) || !strings.Contains(decision.Reason, "this profile is standalone") {
						t.Fatalf("solo decision %+v", decision)
					}
				} else if !decision.Enabled {
					t.Fatalf("default decision %+v", decision)
				}
			}
		})
	}
}

func TestExplicitTrueIsNeverSecondGuessedAndExplicitFalseIsRetired(t *testing.T) {
	f := newModeFleet(t)
	f.pids["coder"] = 4101
	fleetWrite(t, filepath.Join(f.root, "config.yaml"), "gateway:\n  multiplex_profiles: true\n")
	decision, cfg := f.resolve(t)
	if decision != (gateway.MultiplexDecision{Enabled: true, Source: "config"}) || !isTrue(cfg.MultiplexProfiles) {
		t.Fatalf("decision %+v", decision)
	}
	if flag, err := gateway.ExplicitMultiplexFlag(f.ctx, f.root); err != nil || !isTrue(flag) {
		t.Fatalf("explicit flag %v %v", flag, err)
	}
	t.Setenv("GATEWAY_MULTIPLEX_PROFILES", "false")
	f.pids = map[string]int{}
	decision, cfg = f.resolve(t)
	if !decision.Enabled || decision.Source != "retired-opt-out" || !isTrue(cfg.MultiplexProfiles) {
		t.Fatalf("decision %+v", decision)
	}
	if flag, err := gateway.ExplicitMultiplexFlag(f.ctx, f.root); err != nil || !isFalse(flag) {
		t.Fatalf("explicit flag %v %v", flag, err)
	}
	f.pids["coder"] = 4101
	if guarded, _ := f.resolve(t); guarded.Enabled || guarded.Source != "guard" || !strings.Contains(guarded.Reason, "coder") {
		t.Fatalf("guarded %+v", guarded)
	}
}

func TestMigrationPlanTreatsTheUnsetDefaultAsNotYetMultiplexed(t *testing.T) {
	f := newModeFleet(t)
	f.pids["coder"], f.pids["ops"] = 4101, 4102
	f.services["coder"] = []ServiceUnit{unit("systemd", false)}
	f.services["ops"] = []ServiceUnit{unit("systemd", false)}
	plan, err := BuildMigrationPlan(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.MultiplexFlagOn || plan.AlreadyMultiplexed() || !plan.EligibleForMigration() {
		t.Fatalf("plan flag %v converged %v eligible %v", plan.MultiplexFlagOn, plan.AlreadyMultiplexed(), plan.EligibleForMigration())
	}
}
