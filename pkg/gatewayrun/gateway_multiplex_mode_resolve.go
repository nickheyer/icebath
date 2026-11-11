package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Why this process must not multiplex right now, empty when it may
func ImplicitMultiplexBlocker(ctx context.Context) (string, error) {
	standalone, err := gateway.StandaloneLauncher(ctx)
	if err != nil {
		return "", err
	}
	if standalone {
		return gateway.StandaloneProfileReason, nil
	}
	homes, err := profiles.ProfilesToServe(ctx, true, false, true)
	if err != nil {
		return "", err
	}
	if len(homes) < 2 {
		return gateway.SingleProfileReason, nil
	}
	if reason := migrateHostSupportsMigration(); reason != "" {
		return reason, nil
	}
	plan, err := BuildMigrationPlan(ctx)
	if err != nil {
		return "", err
	}
	if secondaries := plan.StandaloneSecondaries(); len(secondaries) > 0 {
		owned := make([]string, len(secondaries))
		for i, p := range secondaries {
			who := p.ServiceLabel()
			if p.PID != 0 {
				who = fmt.Sprintf("pid %d", p.PID)
			}
			owned[i] = fmt.Sprintf("'%s' (%s)", p.Name, who)
		}
		return fmt.Sprintf("profile(s) %s still run their own gateway; fold them with `%s`", strings.Join(owned, ", "), MigrateCommand), nil
	}
	if plan.Blocked() {
		return strings.Join(plan.Blockers, "; "), nil
	}
	return "", nil
}

// Blocker of the preflight, a failure of the preflight itself turned into its own blocker
func preflightBlocker(ctx context.Context) (blocker string) {
	defer func() {
		if r := recover(); r != nil {
			multiplexResolveLog().Warn(fmt.Sprintf("Multiplex preflight failed; starting standalone: %v", r))
			blocker = fmt.Sprintf("preflight failed (%v)", r)
		}
	}()
	blocker, err := ImplicitMultiplexBlocker(ctx)
	if err != nil {
		multiplexResolveLog().Warn("Multiplex preflight failed; starting standalone: " + err.Error())
		return fmt.Sprintf("preflight failed (%s)", err.Error())
	}
	return blocker
}

// Settles the config's multiplex flag for one boot in place, refusing while any blocker holds
func ResolveMultiplexMode(ctx context.Context, cfg *gateway.GatewayConfig) (gateway.MultiplexDecision, error) {
	current := cfg.MultiplexProfiles
	standalone, err := gateway.StandaloneLauncherDecision(ctx, cfg)
	if err != nil {
		return gateway.MultiplexDecision{}, err
	}
	if standalone != nil {
		return *standalone, nil
	}
	if settled := gateway.SettledExplicitFlag(current); settled != nil && *settled {
		return gateway.MultiplexDecision{Enabled: true, Source: "config"}, nil
	}
	retiredOptOut := current != nil && !*current
	var decision gateway.MultiplexDecision
	if blocker := preflightBlocker(ctx); blocker != "" {
		decision = gateway.MultiplexDecision{Enabled: false, Source: "guard", Reason: blocker}
	} else if retiredOptOut {
		decision = gateway.MultiplexDecision{Enabled: true, Source: "retired-opt-out", Reason: gateway.RetiredOptOutReason}
	} else {
		decision = gateway.MultiplexDecision{Enabled: true, Source: "default", Reason: "gateway.multiplex_profiles unset; default applies"}
	}
	enabled := decision.Enabled
	cfg.MultiplexProfiles = &enabled
	return decision, nil
}

// Logger records carry the module name of the multiplex verdict
func multiplexResolveLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.gateway_multiplex_mode")
}
