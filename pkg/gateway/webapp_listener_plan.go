package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/pm"
)

// What the gateway dashboard mount needs to serve the Webapp, its bundle, credential home and own public URL
type WebappMount struct {
	Dist      string
	Home      string
	PublicURL string
}

// Whether native clients can dial the host itself, any loopback address or localhost
func webappIsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, ok := pyipaddress.IPAddress(host)
	return ok && pyipaddress.IsLoopback(addr)
}

// Loopback address native clients dial for a listener bound to the bind host
func WebappDialHost(bindHost string) string {
	if webappIsLoopback(bindHost) {
		return bindHost
	}
	if bindHost == "::" {
		return "::1"
	}
	return "127.0.0.1"
}

// Api origin native clients get for a listener bound to the bind host
func WebappDialOrigin(bindHost string, port int) string {
	host := WebappDialHost(bindHost)
	if strings.Contains(host, ":") {
		return fmt.Sprintf("http://[%s]:%d", host, port)
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

// Built Webapp bundle of a checkout, the packaged one when this install ships it
func webappDistDir(projectRoot string) string {
	if packaged := WebappRendererBuild.PackagedDist(); packaged != "" {
		return packaged
	}
	return filepath.Join(projectRoot, "apps", "desktop", "dist-webapp")
}

// Listener host, port, Webapp mount and the reason a webapp block asking for the Webapp cannot be used
func WebappListenerPlan(ctx context.Context, h string) (string, int, *WebappMount, string) {
	settings := WebappSettingsOf(ctx, h)
	host, port := settings.Bind()
	if !settings.Enabled {
		return host, port, nil, settings.Reason
	}
	return host, port, &WebappMount{Dist: webappDistDir(pm.RepoRoot()), Home: h, PublicURL: settings.PublicURL}, ""
}
