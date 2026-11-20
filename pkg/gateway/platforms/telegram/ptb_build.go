package telegram

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Validated fallback IPs from config extra, a comma separated string or a list
func fallbackIPsFromExtra(extra map[string]any) []string {
	var parts []string
	switch v := extra["fallback_ips"].(type) {
	case nil:
	case string:
		if v != "" {
			parts = strings.Split(v, ",")
		}
	case []string:
		parts = v
	case []any:
		for _, item := range v {
			parts = append(parts, pyrepr.Str(item))
		}
	default:
		parts = []string{pyrepr.Str(v)}
	}
	return ParseFallbackIPEnv(strings.Join(parts, ","))
}

// Hosts of a custom Bot API server named by base_url and base_file_url, none for the public Telegram API
func customAPIHosts(extra map[string]any) []string {
	var hosts []string
	for _, key := range []string{"base_url", "base_file_url"} {
		raw := pystr.Strip(pyrepr.Str(extra[key]))
		if extra[key] == nil || raw == "" {
			continue
		}
		if u, err := url.Parse(strings.ReplaceAll(raw, "{token}", "token")); err == nil && u.Hostname() != "" && !slices.Contains(hosts, u.Hostname()) {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

// URLs of the Bot API hosts the adapter calls, each of which NO_PROXY must keep off the proxy before a failed request's text is shown
func apiProxyTargets(extra map[string]any) []string {
	targets := []string{"https://" + TelegramAPIHost}
	for _, host := range customAPIHosts(extra) {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		targets = append(targets, "https://"+host)
	}
	return targets
}

// Builds the general and getUpdates request pair over fallback IPs, an explicit proxy or direct DNS
func buildPTBRequests(ctx context.Context, name string, extra map[string]any, lifecycle *pollingLifecycle) (*ptbRequest, *ptbRequest, error) {
	poolSize := int(config.EnvInt("ICEBATH_TELEGRAM_HTTP_POOL_SIZE", 512))
	settings := requestSettings{poolTimeout: config.EnvFloat("ICEBATH_TELEGRAM_HTTP_POOL_TIMEOUT", 8.0),
		connectTimeout: config.EnvFloat("ICEBATH_TELEGRAM_HTTP_CONNECT_TIMEOUT", 10.0),
		readTimeout:    config.EnvFloat("ICEBATH_TELEGRAM_HTTP_READ_TIMEOUT", 20.0),
		writeTimeout:   config.EnvFloat("ICEBATH_TELEGRAM_HTTP_WRITE_TIMEOUT", 20.0), mediaWriteTimeout: 60.0, pollTimeout: 10}
	base := gateway.PlatformHTTPXLimits()
	poolLimits := PoolLimits{MaxConnections: poolSize, MaxKeepaliveConnections: *base.MaxKeepaliveConnections,
		KeepaliveExpiry: seconds(*base.KeepaliveExpiry)}
	updatesLimits := PoolLimits{MaxConnections: poolSize, MaxKeepaliveConnections: 0, KeepaliveExpiry: seconds(*base.KeepaliveExpiry)}
	disableFallback := false
	switch pystr.Lower(pystr.Strip(os.Getenv("ICEBATH_TELEGRAM_DISABLE_FALLBACK_IPS"))) {
	case "1", "true", "yes", "on":
		disableFallback = true
	}
	var fallbackIPs []string
	if !disableFallback {
		fallbackIPs = fallbackIPsFromExtra(extra)
	}
	customHosts := customAPIHosts(extra)
	if len(fallbackIPs) == 0 && !disableFallback && len(customHosts) == 0 {
		zero := 0.0
		discoveryTimeout := envFloatClamped("ICEBATH_TELEGRAM_FALLBACK_DISCOVERY_TIMEOUT", 5.0, &zero, nil)
		pkgLog().Warn(fmt.Sprintf("[%s] Discovering Telegram API fallback IPs via DNS-over-HTTPS…", name))
		discovered, err := awaitWithThreadDeadline(ctx, func(ctx context.Context) (any, error) { return DiscoverFallbackIPs(ctx), nil },
			discoveryTimeout, nil, "telegram", true)
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("[%s] Telegram fallback-IP discovery failed after %.0fs; using seed IPv4 Telegram API IPs so a blackholed IPv6 hostname path cannot hang initialize() (#87015): %s",
				name, discoveryTimeout, redactTelegramProxyErrorText(err, gateway.EnvProxyForRedaction())))
			fallbackIPs = slices.Clone(SeedFallbackIPs)
		} else {
			fallbackIPs, _ = discovered.([]string)
			pkgLog().Info(fmt.Sprintf("[%s] Auto-discovered Telegram fallback IPs: %s", name, strings.Join(fallbackIPs, ", ")))
		}
	}
	configured := ""
	if raw := extra["proxy_url"]; raw != nil {
		configured = pyrepr.Str(raw)
	}
	targets := append([]string{"api.telegram.org"}, fallbackIPs...)
	if len(customHosts) > 0 {
		targets = customHosts
	}
	proxyURL, err := gateway.ResolveProxyURL(ctx, "TELEGRAM_PROXY", targets, configured)
	if err != nil {
		return nil, nil, err
	}
	lifecycle.mu.Lock()
	lifecycle.ptbProxyURL = proxyURL
	lifecycle.mu.Unlock()
	tlsConfig := provider.PlatformTLSConfig()
	general, updates := settings, settings
	switch {
	case len(fallbackIPs) > 0 && proxyURL == "" && !disableFallback:
		pkgLog().Info(fmt.Sprintf("[%s] Telegram fallback IPs active: %s", name, strings.Join(fallbackIPs, ", ")))
		keepalive := TCPKeepaliveSocketOptions()
		general.transport = TransportOptions{Limits: &poolLimits, SocketOptions: &keepalive, TLSConfig: tlsConfig}
		updates.transport = TransportOptions{Limits: &updatesLimits, SocketOptions: &keepalive, TLSConfig: tlsConfig}
		general.fallbackIPs, updates.fallbackIPs = fallbackIPs, fallbackIPs
	case proxyURL != "":
		pkgLog().Info(fmt.Sprintf("[%s] Proxy detected; passing explicitly to HTTPXRequest: %s", name, gateway.SafeProxyURLForLog(proxyURL)))
		general.transport = TransportOptions{Proxy: &proxyURL, Limits: &poolLimits, TLSConfig: tlsConfig}
		updates.transport = TransportOptions{Proxy: &proxyURL, Limits: &updatesLimits, TLSConfig: tlsConfig}
	default:
		if disableFallback {
			pkgLog().Info(fmt.Sprintf("[%s] Telegram fallback-IP transport disabled via env", name))
		}
		general.transport = TransportOptions{Limits: &poolLimits, TLSConfig: tlsConfig}
		updates.transport = TransportOptions{Limits: &updatesLimits, TLSConfig: tlsConfig}
	}
	request, err := newPTBRequest(ctx, general)
	if err != nil {
		return nil, nil, err
	}
	getUpdatesRequest, err := newPTBRequest(ctx, updates)
	if err != nil {
		return nil, nil, err
	}
	return request, instrumentPollingRequest(getUpdatesRequest, lifecycle), nil
}

// Instruments the dedicated getUpdates request so every observed response feeds polling progress
func instrumentPollingRequest(request *ptbRequest, lifecycle *pollingLifecycle) *ptbRequest {
	request.observer = lifecycle.observePollingRequestResult
	return request
}
