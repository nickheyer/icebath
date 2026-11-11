package dashboard

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/builtin/dashboardauth"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Members of a Python set built from a config value, false where set() would raise
func pySetMembers(v any) ([]string, bool) {
	switch x := v.(type) {
	case string:
		out := []string{}
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, true
	case map[string]any:
		out := []string{}
		for k := range x {
			out = append(out, k)
		}
		return out, true
	case []any:
		out := []string{}
		for _, e := range x {
			switch m := e.(type) {
			case []any, map[string]any:
				return nil, false
			case string:
				out = append(out, m)
			}
		}
		return out, true
	}
	return nil, false
}

// Whether basic auth credentials are configured while the bundled basic plugin sits in plugins.disabled
func basicAuthDisabledWithCredentials(ctx context.Context) bool {
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), config.EnvRefLookupContext(ctx))
	if err != nil {
		return false
	}
	or := func(v any) any {
		if !pyrepr.Truthy(v) {
			return map[string]any{}
		}
		return v
	}
	dashboard, isMap := or(map[string]any(cfg)["dashboard"]).(map[string]any)
	if !isMap {
		return false
	}
	ba, isMap := or(dashboard["basic_auth"]).(map[string]any)
	if !isMap {
		return false
	}
	pluginsSection, isMap := or(map[string]any(cfg)["plugins"]).(map[string]any)
	if !isMap {
		return false
	}
	disabledRaw := pluginsSection["disabled"]
	if !pyrepr.Truthy(disabledRaw) {
		disabledRaw = []any{}
	}
	disabled, ok := pySetMembers(disabledRaw)
	if !ok {
		return false
	}
	hasCreds := pyrepr.Truthy(ba["username"]) && (pyrepr.Truthy(ba["password_hash"]) || pyrepr.Truthy(ba["password"]))
	if !hasCreds {
		return false
	}
	for _, name := range disabled {
		if plugins.IsBasicAuthPluginKey(name) {
			return true
		}
	}
	return false
}

// Actionable exit text for a gated bind with no provider, naming the trigger and every bundled skip reason
func noAuthProviderMessage(ctx context.Context, app *FastAPI, host string) string {
	var skipReasons []string
	if reason := dashboardauth.NousLastSkipReason(); reason != "" {
		skipReasons = append(skipReasons, "  • nous: "+reason)
	}
	own, hasOwn := app.State.Get("listener_public_url").(string)
	setting := "dashboard.public_url"
	if hasOwn {
		setting = "webapp.public_url"
	}
	var gateReason, fixHint string
	if gateway.LoopbackHostValues[host] {
		publicURL := own
		if !hasOwn {
			publicURL = gateway.ResolvePublicURL(ctx, nil)
		}
		if publicURL == "" {
			publicURL = "<a non-loopback URL>"
		}
		gateReason = fmt.Sprintf("%s is set to %s — an operator-declared external URL engages the auth gate "+
			"even on a loopback bind", setting, publicURL)
		unsetEnv := ""
		if !hasOwn {
			unsetEnv = "(and unset ICEBATH_DASHBOARD_PUBLIC_URL) "
		}
		fixHint = "If this dashboard should be LOCAL-ONLY (no reverse proxy), remove " + setting + " from config.yaml " +
			unsetEnv + "to restore the unauthenticated loopback mode.\n"
	} else {
		gateReason = fmt.Sprintf("the auth gate engages on non-loopback binds (%s)", host)
	}
	fixHint += "Configure an auth provider before exposing the dashboard:\n" +
		"  • Password: set dashboard.basic_auth.username + " +
		"password_hash in config.yaml\n" +
		"    (hash with: python -c \"from " +
		"plugins.dashboard_auth.basic import hash_password; " +
		"print(hash_password('your-password'))\")\n" +
		"  • OAuth: run `icebath dashboard register` (Nous Portal) or " +
		"install a DashboardAuthProvider plugin.\n" +
		"  • Deployment token: provision ICEBATH_DASHBOARD_SESSION_TOKEN " +
		"(a literal token, or 'file:<path>' naming a secret-manager-decrypted " +
		"token file) — the bundled session-token provider then authorizes " +
		"bearer REST + /api/ws ?token= for token-mode clients such as the " +
		"Desktop's remote-gateway 'Session token' field.\n" +
		"There is no unauthenticated public-dashboard option. For " +
		"local-only use, bind 127.0.0.1 and leave " + setting + " " +
		"unset; a configured external public URL requires auth even " +
		"when a local reverse proxy reaches a loopback backend."
	if basicAuthDisabledWithCredentials(ctx) {
		fixHint = "The 'basic' dashboard-auth plugin is in " +
			"plugins.disabled but dashboard.basic_auth is " +
			"configured.\n" +
			"Remove 'basic' from plugins.disabled (or run " +
			"`icebath plugins enable basic`), then restart the " +
			"dashboard.\n\n" + fixHint
	}
	msg := fmt.Sprintf("Refusing to bind dashboard to %s — %s, but no auth providers are registered.\n\n", host, gateReason)
	if len(skipReasons) > 0 {
		msg += "Bundled providers reported these issues:\n" + strings.Join(skipReasons, "\n") + "\n\n"
	}
	return msg + fixHint
}

// Trusted public hosts, origins and the auth gate flag onto app state, a SystemExit when the gate has no provider
func configureAuthGate(ctx context.Context, app *FastAPI, host string, allowPublic bool, sshSessionToken, sshOwnerNonce *string) error {
	own, hasOwn := app.State.Get("listener_public_url").(string)
	if hasOwn {
		app.State.Set("trusted_public_hosts", gateway.PublicURLHosts(own))
		app.State.Set("trusted_public_origins", gateway.PublicURLOrigins(own))
	} else {
		app.State.Set("trusted_public_hosts", gateway.DashboardPublicHosts(ctx))
		app.State.Set("trusted_public_origins", gateway.DashboardPublicOrigins(ctx))
	}
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	if !hasOwn && gateway.DesktopLoopbackAuthExempt(host, deref(sshSessionToken), deref(sshOwnerNonce)) {
		app.State.Set("auth_required", gateway.ShouldRequireAuth(host))
		webServerLog().Info("Desktop-owned loopback backend: dashboard.public_url does not " +
			"engage the ticket gate for this process; the public deployment " +
			"keeps its own gate.")
	} else {
		trusted, _ := app.State.Get("trusted_public_hosts").(map[string]bool)
		app.State.Set("auth_required", ShouldRequireDashboardAuth(ctx, host, trusted))
	}
	if allowPublic && !gateway.LoopbackHostValues[host] {
		webServerLog().Warn(fmt.Sprintf("--insecure no longer bypasses dashboard authentication. A "+
			"non-loopback bind (%s) now ALWAYS requires an auth provider "+
			"(OAuth or the bundled password provider). Configure one — see "+
			"below — or bind to 127.0.0.1 and reach it over an SSH tunnel / "+
			"Tailscale.", host))
	}
	if app.State.Bool("auth_required") {
		providers := gateway.ListDashboardAuthProviders(ctx, "")
		if len(providers) == 0 {
			return &gateway.DashboardExit{Code: 1, Msg: noAuthProviderMessage(ctx, app, host)}
		}
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = p.Name()
		}
		webServerLog().Info(fmt.Sprintf("Dashboard binding to %s with auth gate enabled. Providers: %s", host, strings.Join(names, ", ")))
	}
	return nil
}
