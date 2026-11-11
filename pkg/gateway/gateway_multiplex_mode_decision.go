package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Guard reason of a profile that opted out of the host multiplexer
const StandaloneProfileReason = "this profile is standalone (gateway.standalone: true); it serves only itself"

// Guard reason of a single profile boot the auto migration opt out keeps standalone
const AutoMigrationOptedOutReason = "gateway.auto_multiplex_migration is false, so this gateway serves only its own profile; " +
	"run `icebath gateway migrate --multiplex` to serve every profile"

// Notice every surface naming gateway.standalone prints
const StandaloneDeprecationNotice = "gateway.standalone is a temporary compatibility shim while multiplexing gaps are fixed; " +
	"it will be removed once they are — plan to fold this profile with `icebath gateway migrate --multiplex`."

// Notice of a retired multiplex opt out that was rewritten to true
const RetiredOptOutReason = "gateway.multiplex_profiles: false is retired and was rewritten to true; one gateway per host " +
	"serves every profile. A per-profile gateway is `gateway.standalone: true` in that profile's " +
	"config (temporary shim) or `--force`."

// Boot verdict on multiplexing with where it came from and the guard's reason
type MultiplexDecision struct {
	Enabled bool
	Source  string
	Reason  string
}

// Logger records carry the module name of the multiplex verdict
func multiplexModeLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.gateway_multiplex_mode")
}

// Whether the launching home is a named profile that opted out with gateway.standalone
func standaloneLauncher(ctx context.Context) (bool, error) {
	h := home.Dir(ctx)
	name, ok := ProfileNameForHome(h)
	if !ok || name == "default" {
		return false, nil
	}
	return profiles.ProfileIsStandalone(ctx, h)
}

// Standalone guard verdict of an opted out launcher, turning multiplexing off on cfg, nil otherwise
func StandaloneLauncherDecision(ctx context.Context, cfg *GatewayConfig) (*MultiplexDecision, error) {
	standalone, err := standaloneLauncher(ctx)
	if err != nil || !standalone {
		return nil, err
	}
	off := false
	cfg.MultiplexProfiles = &off
	return &MultiplexDecision{Enabled: false, Source: "guard", Reason: StandaloneProfileReason}, nil
}

// Writes gateway.multiplex_profiles true into the default profile's config once, never on a guard refusal
func PersistResolvedDefault(ctx context.Context, decision MultiplexDecision, defaultHome string) bool {
	if !decision.Enabled || decision.Source == "guard" {
		return false
	}
	if defaultHome == "" {
		defaultHome = home.Root()
	}
	cfgPath := filepath.Join(defaultHome, "config.yaml")
	inFile, err := func() (any, error) {
		cfg := map[string]any{}
		exists, err := config.ConfigExists(ctx, cfgPath)
		if err != nil {
			return nil, err
		}
		if exists {
			raw, err := config.ReadUserConfigRaw(ctx, cfgPath)
			if err != nil {
				return nil, err
			}
			if raw != nil {
				cfg = raw
			}
		}
		section, _ := cfg["gateway"].(map[string]any)
		inFile, ok := cfg["multiplex_profiles"]
		if !ok {
			inFile = section["multiplex_profiles"]
		}
		if inFile == true {
			return inFile, errAlreadyPersisted
		}
		if err := writeMultiplexFlag(ctx, defaultHome, true); err != nil {
			return nil, err
		}
		if decision.Source == "retired-opt-out" {
			if err := os.WriteFile(filepath.Join(defaultHome, RewrittenMarkerName), []byte(RetiredOptOutReason+"\n"), 0o644); err != nil {
				return nil, err
			}
		}
		return inFile, nil
	}()
	if err == errAlreadyPersisted {
		return false
	}
	if err != nil {
		multiplexModeLog().Debug("could not persist gateway.multiplex_profiles: true", "error", err)
		return false
	}
	was := "unset"
	if inFile != nil {
		was = pyrepr.Repr(inFile)
	}
	multiplexModeLog().Info(fmt.Sprintf("Wrote gateway.multiplex_profiles: true to %s (was %s).", cfgPath, was))
	return true
}

// Sentinel of a config that already reads true, so nothing was written
var errAlreadyPersisted = fmt.Errorf("gateway.multiplex_profiles already true")

// Decision held back until this process owns the runtime lock
var pendingMultiplexDecision struct {
	mu       sync.Mutex
	decision *MultiplexDecision
}

// Publishes a guard refusal into the runtime status once this process owns the lock, holding it back before
func RecordMultiplexDecision(ctx context.Context, decision MultiplexDecision) {
	defer func() {
		if rec := recover(); rec != nil {
			multiplexModeLog().Debug("could not record the multiplex decision", "error", rec)
		}
	}()
	owns := OwnsGatewayRuntimeLock()
	pendingMultiplexDecision.mu.Lock()
	if !owns {
		d := decision
		pendingMultiplexDecision.decision = &d
		pendingMultiplexDecision.mu.Unlock()
		return
	}
	pendingMultiplexDecision.decision = nil
	pendingMultiplexDecision.mu.Unlock()
	var reason any
	if decision.Source == "guard" {
		reason = decision.Reason
	}
	if _, err := WriteRuntimeStatus(ctx, RuntimeStatusUpdate{Fields: map[string]any{"multiplex_standalone_reason": reason}}, false, nil); err != nil {
		multiplexModeLog().Debug("could not record the multiplex decision", "error", err)
	}
}

// Writes the decision held back before the runtime lock claim
func PublishPendingMultiplexDecision(ctx context.Context) {
	pendingMultiplexDecision.mu.Lock()
	pending := pendingMultiplexDecision.decision
	pendingMultiplexDecision.mu.Unlock()
	if pending != nil {
		RecordMultiplexDecision(ctx, *pending)
	}
}

// Records the verdict, logs it and prints the boxed notice or warning it calls for
func LogMultiplexDecision(ctx context.Context, decision MultiplexDecision) {
	RecordMultiplexDecision(ctx, decision)
	switch {
	case decision.Source == "retired-opt-out":
		multiplexModeLog().Warn(RetiredOptOutReason)
		PersistResolvedDefault(ctx, decision, "")
		for _, line := range RetiredOptOutNoticeLines() {
			fmt.Println(line)
		}
	case decision.Source == "guard" && decision.Reason == SingleProfileReason:
		multiplexModeLog().Info("Single-profile install: gateway.multiplex_profiles unset, serving the default profile only.")
	case decision.Source == "guard":
		multiplexModeLog().Warn(fmt.Sprintf("This gateway stays standalone: %s. It serves only the launching profile.", decision.Reason))
		for _, line := range StandaloneWarningLines(ctx, decision.Source, decision.Reason, nil, false) {
			fmt.Println(line)
		}
	case decision.Source == "default":
		multiplexModeLog().Info("Serving every profile on this host (gateway.multiplex_profiles unset; default on).")
		PersistResolvedDefault(ctx, decision, "")
	}
}
