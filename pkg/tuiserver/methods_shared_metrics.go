package tuiserver

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Counts one user friction action of a session against the model of its last turn
func tuiModelFriction(ctx context.Context, signal string, session *tuigateway.Session, turns int) {
	opts := observability.FrictionOptions{SessionID: session.Get("session_key"), HomeDir: strOr(session.Get("profile_home")), Turns: turns}
	if a := sessionAgent(session); a != nil {
		opts.Agent = a
	}
	observability.RecordModelFriction(ctx, signal, opts)
}

var sharedMetricsRegistry = &handlerRegistry{}

func init() {
	sharedMetricsRegistry.scopedMethod("shared_metrics.status", sharedMetricsStatus)
	sharedMetricsRegistry.scopedMethod("shared_metrics.set", sharedMetricsSet)
	sharedMetricsRegistry.scopedMethod("shared_metrics.slash_command", sharedMetricsSlashCommand)
	sharedMetricsRegistry.scopedMethod("shared_metrics.startup_latency", sharedMetricsStartupLatency)
	sharedMetricsRegistry.scopedMethod("shared_metrics.update_run", sharedMetricsUpdateRun)
	sharedMetricsRegistry.scopedMethod("shared_metrics.desktop_feature_use", sharedMetricsDesktopFeatureUse)
	sharedMetricsRegistry.scopedMethod("shared_metrics.desktop_friction", sharedMetricsDesktopFriction)
	sharedMetricsRegistry.scopedMethod("shared_metrics.desktop_onboarding", sharedMetricsDesktopOnboarding)
	sharedMetricsRegistry.scopedMethod("shared_metrics.desktop_dislike", sharedMetricsDesktopDislike)
	sharedMetricsRegistry.scopedMethod("shared_metrics.desktop_daily", sharedMetricsDesktopDaily)
	sharedMetricsRegistry.install()
}

// The enabled, send, decided and reask reading every consent surface shares
func sharedMetricsConsent(cfg any) map[string]any {
	answer := observability.ConsentState(cfg)
	return map[string]any{"enabled": answer.Enabled, "send": answer.Send, "decided": answer.Decided, "reask": answer.Reask}
}

// Records the Desktop first run answer as the setup completed metric with the configured provider
func sharedMetricsRecordSetupCompleted(ctx context.Context, cfg map[string]any) {
	var providerName any
	if model, isDict := cfg["model"].(map[string]any); isDict {
		if text, isText := model["provider"].(string); isText && text != "" {
			providerName = text
		}
	}
	observability.RecordSetupCompleted(ctx, "desktop", providerName)
}

// Consent reading of the focused profile's raw config
func sharedMetricsStatus(ctx context.Context, rid any, _ map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = rpcErr(rid, 5095, failureText(failure), nil)
		}
	}()
	return ok(rid, sharedMetricsConsent(loadCfg(ctx)))
}

// Writes both opt ins with send forced off without collection and reconciles the consent windows
func sharedMetricsSet(ctx context.Context, rid any, params map[string]any) map[string]any {
	enabled := params["enabled"] == true
	send := enabled && params["send"] == true
	cfg, err := func() (cfg map[string]any, err error) {
		defer func() {
			if failure := recover(); failure != nil {
				if configOutage(failure) {
					panic(failure)
				}
				if raised, isError := failure.(error); isError {
					err = raised
					return
				}
				err = errors.New(failureText(failure))
			}
		}()
		fileTooling, err := config.SupportsFileTooling(ctx)
		if err != nil {
			return nil, err
		}
		if fileTooling {
			if cfg, err = loadCfgRaw(ctx); err != nil {
				return nil, err
			}
			observability.SetAnswer(cfg, enabled, send)
			return cfg, saveCfg(ctx, cfg)
		}
		if err := writeConfigChanges(ctx, []config.KeyValue{{Key: "telemetry.shared_metrics.enabled", Value: enabled},
			{Key: "telemetry.shared_metrics.send", Value: send},
			{Key: "telemetry.shared_metrics.offer_version", Value: observability.OfferVersion}}, nil); err != nil {
			return nil, err
		}
		return loadCfgRaw(ctx)
	}()
	if err != nil {
		var writeErr *config.ConfigWriteError
		if errors.As(err, &writeErr) {
			return rpcErr(rid, 4002, err.Error(), nil)
		}
		return rpcErr(rid, 5096, err.Error(), nil)
	}
	observability.RecordSendConsentChange(ctx, send)
	if !enabled {
		observability.PurgeOnboardingLatches(ctx)
		observability.PurgePendingReceipts(ctx, home.Dir(ctx))
	}
	if params["first_run"] == true {
		sharedMetricsRecordSetupCompleted(ctx, cfg)
	}
	return ok(rid, sharedMetricsConsent(cfg))
}

// Counts one user typed slash command on the surface this backend serves
func sharedMetricsSlashCommand(ctx context.Context, rid any, params map[string]any) map[string]any {
	observability.RecordSlashCommand(ctx, relayString(params["command"]), resolveSessionPlatform())
	return ok(rid, map[string]any{"ok": true})
}

// Records the client's launch to ready time once per launch id, its declared surface winning
func sharedMetricsStartupLatency(ctx context.Context, rid any, params map[string]any) map[string]any {
	var surface any = resolveSessionPlatform()
	if truthy(params["surface"]) {
		surface = params["surface"]
	}
	observability.RecordRpcStartupLatency(ctx, surface, params["elapsed_ms"], params["launch_id"])
	return ok(rid, map[string]any{"ok": true})
}

// Records one packaged Desktop self update run
func sharedMetricsUpdateRun(ctx context.Context, rid any, params map[string]any) map[string]any {
	observability.RecordDesktopUpdate(ctx, map[string]any{"outcome": params["outcome"], "failed_stage": params["failed_stage"],
		"duration_ms": params["duration_ms"], "mechanism": params["mechanism"], "from_commit_date": params["from_commit_date"]})
	return ok(rid, map[string]any{"ok": true})
}

// Records one Desktop area opened, latched once per UTC day
func sharedMetricsDesktopFeatureUse(ctx context.Context, rid any, params map[string]any) map[string]any {
	observability.RecordDesktopFeatureUse(ctx, params["area"])
	return ok(rid, map[string]any{"ok": true})
}

// Records one Desktop friction event
func sharedMetricsDesktopFriction(ctx context.Context, rid any, params map[string]any) map[string]any {
	observability.RecordDesktopFriction(ctx, params["kind"], params["detail"])
	return ok(rid, map[string]any{"ok": true})
}

// Records one Desktop first run step transition
func sharedMetricsDesktopOnboarding(ctx context.Context, rid any, params map[string]any) map[string]any {
	observability.RecordDesktopOnboarding(ctx, params["step"], params["event"])
	return ok(rid, map[string]any{"ok": true})
}

// Records one Desktop dislike signal
func sharedMetricsDesktopDislike(ctx context.Context, rid any, params map[string]any) map[string]any {
	observability.RecordDesktopDislike(ctx, params["signal"], params["target"], params["setting"])
	return ok(rid, map[string]any{"ok": true})
}

// Records one finished Desktop day, recorded true once it settled
func sharedMetricsDesktopDaily(ctx context.Context, rid any, params map[string]any) map[string]any {
	recorded := observability.RecordDesktopDaily(ctx, params["day"], params["modes"], params["actions"], params["bot_count"])
	return ok(rid, map[string]any{"recorded": recorded})
}
