package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Published shared listener application of a secondary's adapter with its forwarding base and callback URL
type SharedIngress struct {
	App  *WebApplication
	Base string
	URL  string
}

// Shared listener publication of the adapter, nil while it binds its own port or has not connected
func (b *BasePlatformAdapter) SharedIngress() *SharedIngress {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sharedIngress
}

// Adapter that binds its own listener on a host and port
type listenerAddressAdapter interface {
	ListenHost() *string
	ListenPort() int
}

// Runner holding the launch profile's adapter map
type launchAdaptersRunner interface {
	Adapters() *pydict.Dict[Platform, PlatformAdapter]
}

// Wildcard hosts that bind every interface
var wildcardHosts = map[string]bool{"": true, "0.0.0.0": true, "::": true, "*": true}

// True for the dual stack default and the per family wildcards
func IsWildcardHost(host *string) bool {
	return host == nil || wildcardHosts[strings.TrimSpace(*host)]
}

// Profile name when the adapter was built in shared listener mode, empty otherwise
func SharedIngressProfile(adapter PlatformAdapter) string {
	return adapter.Base().SharedListenerProfile()
}

// Base URL clients use to reach a listener bound on host and port, wildcards mapped to loopback
func ListenerBaseURL(host *string, port int) string {
	h := "127.0.0.1"
	if !IsWildcardHost(host) {
		h = *host
	}
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		h = "[" + h + "]"
	}
	return fmt.Sprintf("http://%s:%d", h, port)
}

// Base URL of the default profile's live listener, api_server first then webhook, empty when neither runs
func SharedListenerBase(runner any) string {
	holder, ok := runner.(launchAdaptersRunner)
	if !ok || holder == nil {
		return ""
	}
	adapters := holder.Adapters()
	for _, platform := range []Platform{PlatformAPIServer, PlatformWebhook} {
		adapter, _ := adapters.Get(platform)
		if adapter == nil {
			continue
		}
		var host *string
		port := 0
		if bound, ok := adapter.(listenerAddressAdapter); ok {
			host, port = bound.ListenHost(), bound.ListenPort()
		}
		return ListenerBaseURL(host, port)
	}
	return ""
}

// Starts the app on host and port, or in shared listener mode publishes it for prefix forwarding and returns nil
func BindListener(ctx context.Context, adapter PlatformAdapter, app *WebApplication, host *string, port int, ingressPath string, opts ListenOptions) (*WebAppRunner, error) {
	if SharedIngressProfile(adapter) != "" {
		PublishSharedIngress(ctx, adapter, app, ingressPath)
		return nil, nil
	}
	return StartWebApp(ctx, app, host, port, opts)
}

// Freezes the app, exposes it to the default listener and records the profile prefixed callback URL
func PublishSharedIngress(ctx context.Context, adapter PlatformAdapter, app *WebApplication, ingressPath string) {
	b := adapter.Base()
	profile := SharedIngressProfile(adapter)
	app.Freeze()
	base := SharedListenerBase(b.GatewayRunner)
	prefix := "/p/" + profile
	published := &SharedIngress{App: app, Base: prefix}
	if base != "" {
		published.Base = base + prefix
	}
	published.URL = published.Base + ingressPath
	b.mu.Lock()
	b.sharedIngress = published
	b.mu.Unlock()
	platform := string(b.Platform)
	if platform == "" {
		platform = "adapter"
	}
	if base != "" {
		slog.Info(fmt.Sprintf("[%s] profile '%s' is served on the default profile's shared listener: %s "+
			"(point the vendor's callback URL at this path behind your public host)", platform, profile, published.URL))
	} else {
		slog.Warn(fmt.Sprintf("[%s] profile '%s' is in shared-listener mode but the default profile has no live api_server "+
			"or webhook listener yet; it will be reachable at <listener>%s%s once one is up", platform, profile, prefix, ingressPath))
	}
	b.writeRuntimeStatusSafe(ctx, "shared_ingress", map[string]any{"ingress_url": published.URL})
}

// Shared listener adapter and its app for one served named profile
type SharedIngressApp struct {
	Adapter PlatformAdapter
	App     *WebApplication
}

// Every shared listener app of a named served profile, nothing for default or an unknown profile
func SharedIngressApps(runner any, profile string) []SharedIngressApp {
	if profile == "" || profile == "default" {
		return nil
	}
	holder, ok := runner.(servedProfileAdaptersRunner)
	if !ok || holder == nil {
		return nil
	}
	adapters, _ := holder.ProfileAdapters().Get(profile)
	var out []SharedIngressApp
	for _, adapter := range adapters.All() {
		if adapter == nil {
			continue
		}
		if published := adapter.Base().SharedIngress(); published != nil && published.App != nil {
			out = append(out, SharedIngressApp{Adapter: adapter, App: published.App})
		}
	}
	return out
}

// Forwards /p/profile/tail to the profile adapter app routing /tail under that profile's runtime scope
func DispatchProfileIngress(ctx context.Context, runner any, profile, tail string, request *WebRequest, scoped bool) (*WebResponse, error) {
	candidates := SharedIngressApps(runner, profile)
	if len(candidates) == 0 && profileParked(runner, profile) {
		return nil, NewWebHTTPError(http.StatusServiceUnavailable, "Profile temporarily unavailable; retry", ParkedRouteRefusedHeaders)
	}
	if profile == "" || len(candidates) == 0 {
		return nil, NewWebHTTPError(http.StatusNotFound, "Unknown or unconfigured profile", nil)
	}
	relURL := &url.URL{Path: "/" + strings.TrimLeft(tail, "/"), RawQuery: request.RelURL.RawQuery, Fragment: request.RelURL.Fragment}
	forwarded := request.Clone(relURL, request.ClientMaxSize)
	var chosen *WebApplication
	for _, candidate := range candidates {
		_, _, refusal := candidate.App.Resolve(forwarded)
		if refusal != nil && refusal.Status == http.StatusNotFound {
			continue
		}
		chosen = candidate.App
		break
	}
	if chosen == nil {
		return nil, NewWebHTTPError(http.StatusNotFound, "No adapter serves this path for the profile", nil)
	}
	forwarded = request.Clone(relURL, chosen.ClientMaxSize)
	if scoped {
		return chosen.Handle(ctx, forwarded)
	}
	dir, err := profiles.GetProfileDir(profile)
	if err != nil {
		return nil, err
	}
	return chosen.Handle(ProfileRuntimeScope(ctx, dir, true), forwarded)
}
