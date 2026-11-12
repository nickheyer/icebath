package dashboard

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pypsutil"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Whether the auth gate is engaged, the registered providers and the sign in flows a native app may pick from
func authGateStatus(ctx context.Context, app *FastAPI) *pyjson.Object {
	authRequired := app.State.Bool("auth_required")
	providers := []any{}
	for _, p := range gateway.ListDashboardAuthProviders(ctx, "") {
		providers = append(providers, p.Name())
	}
	flows := []any{}
	if authRequired {
		flows = append(flows, "cookie")
		if len(gateway.ListInteractiveDashboardAuthProviders(ctx)) > 0 {
			flows = append(flows, "native_pkce")
		}
	}
	return pyjson.ObjectOf("auth_required", authRequired, "auth_providers", providers, "auth_flows", flows)
}

// Validity of the Nous bootstrap session read from the local auth store of the home in ctx
func nousSessionValidity(ctx context.Context) string {
	return provider.GetNousSessionValidity(bootstrapPoolEnv(ctx))
}

// Status rollup of the gateway, the dashboard, the state store and the platforms, counts and enums only
func componentHealth(ctx context.Context, gw *gatewayStatus) *pyjson.Object {
	gatewayStatusText := "degraded"
	if gw.running && (gw.state == "running" || gw.state == "draining") {
		gatewayStatusText = "ok"
	}
	var gatewayState any = gw.state
	if !pyrepr.Truthy(gatewayState) {
		gatewayState = "stopped"
		if gw.running {
			gatewayState = "running"
		}
	}
	components := pyjson.ObjectOf(
		"gateway", pyjson.ObjectOf("status", gatewayStatusText, "state", gatewayState),
		"dashboard", DashboardHealthState.Snapshot())
	check := gateway.ProbeStateDB(home.Dir(ctx))
	storage := pyjson.ObjectOf("status", check.GetOr("status", "degraded"))
	if check.GetOr("detail", nil) == "corrupt" {
		storage.Set("reason", "corrupt")
	}
	components.Set("storage", storage)
	configured, connected := int64(0), int64(0)
	for _, key := range gw.platforms.Keys {
		entry, ok := gw.platforms.Vals[key].(*pyjson.Object)
		if !ok {
			continue
		}
		raw := entry.GetOr("state", nil)
		if !pyrepr.Truthy(raw) {
			raw = entry.GetOr("status", nil)
		}
		text := ""
		if pyrepr.Truthy(raw) {
			text = pystr.Lower(pyrepr.Str(raw))
		}
		if text == "disabled" {
			continue
		}
		configured++
		if healthyPlatformStates[text] {
			connected++
		}
	}
	platformsStatus := "degraded"
	if connected == configured {
		platformsStatus = "ok"
	}
	components.Set("platforms", pyjson.ObjectOf("status", platformsStatus, "configured", configured, "connected", connected))
	return components
}

// Deferred FTS rebuild progress of the state store of the home in ctx, nil when none
func ftsRebuildStatus(ctx context.Context) (*pyjson.Object, error) {
	dbPath := home.StatePath(home.Dir(ctx))
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}
	db, err := state.Open(dbPath, state.Options{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.FTSRebuildStatus(ctx)
}

// Adds the memory and disk pressure rollups and the FTS rebuild progress to a status payload
func advisoryPressure(ctx context.Context, status *pyjson.Object, h string) {
	for _, block := range []struct {
		key     string
		collect func(ctx context.Context, h string) (*pyjson.Object, error)
	}{{"memory", statusCollectMemory}, {"disk", statusCollectDisk}} {
		value, err := block.collect(ctx, h)
		if err != nil {
			value = pyjson.ObjectOf("pressure", "unknown")
		}
		status.Set(block.key, value)
	}
	if rebuild, err := ftsRebuildStatus(ctx); err == nil && rebuild != nil {
		status.Set("fts_rebuild", rebuild)
	}
}

// Version, config version stamp, update ability and Nous session validity the status probe reads from disk
func statusLocalReads(ctx context.Context) (*pyjson.Object, error) {
	current, latest, err := statusCheckConfigVersion(ctx, home.Dir(ctx), false)
	if err != nil {
		return nil, err
	}
	info, err := statusVersionInfo()
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("version", info.BaseVersion, "config_version", int64(current), "latest_config_version", int64(latest),
		"can_update_icebath", !dashboardLocalUpdateManagedExternally(ctx), "nous_session_valid", nousSessionValidity(ctx)), nil
}

var webappSignedOutStatusKeys = map[string]bool{"version": true, "release_date": true, "gateway_running": true, "gateway_state": true,
	"overall": true, "components": true, "auth_required": true, "auth_providers": true, "auth_flows": true}

// Whether the request reached the gated Webapp of the gateway without a session the gate accepts
func webappCallerSignedOut(ctx context.Context, req *Request) bool {
	if Surface(req.App().State) != "webapp" || !req.App().State.Bool("auth_required") {
		return false
	}
	if req.State().Get("session") == nil {
		if session := PeekSession(ctx, &req.HTTPConnection); session != nil {
			req.State().Set("session", session)
		}
	}
	return requireToken(req) != nil
}

// Public liveness probe of the machine or of one profile, host recon added only on an ungated bind
func getStatus(ctx context.Context, req *Request, profile string) (*pyjson.Object, error) {
	signedOut := webappCallerSignedOut(ctx, req)
	requested := ""
	if !signedOut {
		requested = pystr.Strip(profile)
	}
	profileDir := ""
	if requested != "" && pystr.Lower(requested) != "current" {
		dir, err := resolveProfileDir(requested)
		if err != nil {
			return nil, err
		}
		sctx, _, err := configProfileScope(ctx, requested)
		if err != nil {
			return nil, err
		}
		profileDir, ctx = dir, sctx
	}
	local, err := statusLocalReads(ctx)
	if err != nil {
		return nil, err
	}
	healthURL, _ := GatewayHealthURL()
	gw, err := resolveGatewayStatus(ctx, profileDir, healthURL)
	if err != nil {
		return nil, err
	}
	topology := statusTopologyCached(ctx)
	if requested == "" {
		gw.platforms = mergeProfileGatewayPlatforms(ctx, gw.platforms, topology.GetOr("profile_platforms", nil))
	}
	activeSessions := statusActiveSessions(ctx)
	var rawAgents any = int64(0)
	if gw.runtime != nil {
		rawAgents = gw.runtime.GetOr("active_agents", int64(0))
	}
	activeAgents, err := gateway.ParseActiveAgents(rawAgents)
	if err != nil {
		return nil, err
	}
	drainTimeout, err := statusRestartDrainTimeout(ctx)
	if err != nil {
		return nil, err
	}
	auth := authGateStatus(ctx, req.App())
	busy, err := gateway.DeriveGatewayBusy(gw.running, gw.state, activeAgents)
	if err != nil {
		return nil, err
	}
	status := pyjson.ObjectOf(
		"version", local.GetOr("version", nil), "release_date", tuiserver.Banner.ReleaseDate,
		"config_version", local.GetOr("config_version", nil), "latest_config_version", local.GetOr("latest_config_version", nil),
		"can_update_icebath", local.GetOr("can_update_icebath", nil),
		"gateway_running", gw.running, "gateway_state", gw.state,
		"gateway_platforms", gw.platforms,
		"gateway_exit_reason", gw.exitReason,
		"gateway_updated_at", gw.updatedAt,
		"gateway_heartbeat_stale_s", gw.heartbeatStaleS,
		"gateway_shared_with", gw.sharedWith,
		"active_agents", activeAgents,
		"gateway_busy", busy,
		"gateway_drainable", gateway.DeriveGatewayDrainable(gw.running, gw.state),
		"restart_drain_timeout", drainTimeout, "active_sessions", int64(activeSessions))
	status.Update(auth)
	status.Set("nous_session_valid", local.GetOr("nous_session_valid", nil))
	installID, err := getInstallID(ctx)
	if err != nil {
		return nil, err
	}
	if installID != "" {
		status.Set("install_id", installID)
	}
	status.Set("shared_profile_warning", profiles.SharedProfileWarning(ctx, "", "") != "")
	components := componentHealth(ctx, gw)
	status.Set("components", components)
	overall := "ok"
	for _, key := range components.Keys {
		if item, ok := components.Vals[key].(*pyjson.Object); !ok || item.GetOr("status", nil) != "ok" {
			overall = "degraded"
		}
	}
	status.Set("overall", overall)
	pressureHome := profileDir
	if pressureHome == "" {
		pressureHome = home.Dir(ctx)
	}
	advisoryPressure(ctx, status, pressureHome)
	status.Set("profiles", topology.GetOr("profiles", nil))
	status.Set("parked_profiles", topology.GetOr("parked_profiles", []any{}))
	status.Set("gateway_mode", topology.GetOr("gateway_mode", nil))
	status.Set("multiplex_standalone_reason", topology.GetOr("multiplex_standalone_reason", nil))
	if !pyrepr.Truthy(auth.GetOr("auth_required", false)) {
		h := home.Dir(ctx)
		var healthValue any
		if url, set := GatewayHealthURL(); set {
			healthValue = url
		}
		status.Set("hermes_home", h)
		status.Set("config_path", home.ConfigPath(h))
		status.Set("env_path", home.EnvPath(h))
		status.Set("gateway_pid", gw.pid)
		status.Set("gateway_health_url", healthValue)
		status.Set("gateways", topology.GetOr("gateways", nil))
	}
	if signedOut {
		public := pyjson.NewObject()
		for _, key := range status.Keys {
			if webappSignedOutStatusKeys[key] {
				public.Set(key, status.Vals[key])
			}
		}
		return public, nil
	}
	return status, nil
}

// Host and process figures for the System page, the psutil probes each added only when it answers
func getSystemStats(ctx context.Context) (*pyjson.Object, error) {
	version, err := statusVersionInfo()
	if err != nil {
		return nil, err
	}
	info := displaySystemPlatform(pyplatform.System(), pyplatform.Release(), pyplatform.Version(), pyplatform.Platform())
	info.Set("arch", pyplatform.Machine())
	info.Set("hostname", pyplatform.Node())
	info.Set("go_version", runtime.Version())
	info.Set("runtime", runtime.Compiler)
	info.Set("hermes_version", version.BaseVersion)
	info.Set("cpu_count", int64(pypsutil.CPUCount()))
	vm, err := pypsutil.VirtualMemory()
	if err != nil {
		info.Set("psutil", false)
		if runtime.GOOS != "windows" {
			if load, err := pypsutil.Getloadavg(); err == nil {
				info.Set("load_avg", []any{load[0], load[1], load[2]})
			}
		}
		return info, nil
	}
	info.Set("memory", pyjson.ObjectOf("total", int64(vm.Total), "available", int64(vm.Available), "used", int64(vm.Used), "percent", vm.Percent))
	if du, err := pypsutil.DiskUsage(home.Dir(ctx)); err == nil {
		info.Set("disk", pyjson.ObjectOf("total", int64(du.Total), "used", int64(du.Used), "free", int64(du.Free), "percent", du.Percent))
	}
	if percent, err := pypsutil.CPUPercent(100 * time.Millisecond); err == nil {
		info.Set("cpu_percent", percent)
		if load, err := pypsutil.Getloadavg(); err == nil {
			info.Set("load_avg", []any{load[0], load[1], load[2]})
		}
	}
	if boot, err := proc.BootTime(); err == nil {
		info.Set("uptime_seconds", int64(float64(time.Now().UnixNano())/1e9-boot))
	}
	pid := os.Getpid()
	rss, rssErr := proc.RSS(pid)
	created, createdErr := proc.Birth(pid)
	threads, threadsErr := proc.NumThreads(pid)
	if rssErr == nil && createdErr == nil && threadsErr == nil {
		info.Set("process", pyjson.ObjectOf("pid", int64(pid), "rss", rss, "create_time", int64(created), "num_threads", int64(threads)))
	}
	info.Set("psutil", true)
	return info, nil
}
