package dashboard

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
)

var (
	webServerOnce sync.Once
	webServerErr  error
	sessionMu     sync.RWMutex
	sessionToken  string
)

// Runs the import time setup of the web server module once, the session token resolution
func ImportWebServer() error {
	webServerOnce.Do(func() {
		token, err := gateway.ResolveSessionToken()
		if err != nil {
			webServerErr = err
			return
		}
		setSessionToken(token)
		importWebServerState()
	})
	return webServerErr
}

// Session token the web UI and the desktop shell present
func SessionToken() string {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return sessionToken
}

func setSessionToken(token string) {
	sessionMu.Lock()
	sessionToken = token
	sessionMu.Unlock()
}

// Whether a non-loopback bind or a non-loopback dashboard.public_url requires the auth gate, nil hosts resolving the public URL
func ShouldRequireDashboardAuth(ctx context.Context, host string, trustedPublicHosts map[string]bool) bool {
	if trustedPublicHosts == nil {
		trustedPublicHosts = gateway.DashboardPublicHosts(ctx)
	}
	return gateway.ShouldRequireAuth(host) || gateway.PublicHostsEngageGate(trustedPublicHosts)
}

// Gate decision of the dashboard launch after the web server module import, the SystemExit of a bad token file surfacing
func ShouldRequireDashboardAuthAfterImport(ctx context.Context, host string) (bool, error) {
	if err := ImportWebServer(); err != nil {
		return false, err
	}
	return ShouldRequireDashboardAuth(ctx, host, nil), nil
}
