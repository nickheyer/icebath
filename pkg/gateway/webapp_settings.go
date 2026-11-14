package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const (
	webappDefaultHost = "127.0.0.1"
	webappDefaultPort = 9120
)

// Webapp block of a home config with the reason a block asking for the Webapp keeps it off
type WebappSettings struct {
	Enabled   bool
	Host      string
	Port      int
	PublicURL string
	Reason    string
}

// Settings of a home without a webapp block, off on the default address
func defaultWebappSettings() WebappSettings {
	return WebappSettings{Host: webappDefaultHost, Port: webappDefaultPort}
}

// Listener bind address, the configured one when on else a private loopback ephemeral port
func (s WebappSettings) Bind() (string, int) {
	if s.Enabled {
		return s.Host, s.Port
	}
	return webappDefaultHost, 0
}

// Merged config of this process's own home, else the file of another home as written
func webappConfigDocument(ctx context.Context, h string) (map[string]any, error) {
	own := false
	if a, err := pypath.Resolve(h); err == nil {
		if b, err := pypath.Resolve(home.Dir(ctx)); err == nil {
			own = a == b
		}
	}
	if own {
		tree, err := config.LoadReadonly(ctx, home.Dir(ctx), config.NewSecrets(home.Dir(ctx)).Lookup())
		return tree, err
	}
	return config.ReadUserConfigRaw(ctx, filepath.Join(h, "config.yaml"))
}

// Mapping value of a YAML node, false when it is not a mapping
func webappMapping(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return m, true
	}
	return nil, false
}

// Webapp settings of a home, off for a missing, unreadable or malformed block with the reason it is unusable
func WebappSettingsOf(ctx context.Context, h string) WebappSettings {
	path := filepath.Join(h, "config.yaml")
	off := func(reason string, configured WebappSettings) WebappSettings {
		slog.Warn("Webapp stays off: " + reason)
		configured.Reason = reason
		return configured
	}
	document, err := webappConfigDocument(ctx, h)
	if err != nil {
		return off(fmt.Sprintf("%s could not be read (%v)", path, err), defaultWebappSettings())
	}
	raw, present := document["webapp"]
	if !present || raw == nil {
		return defaultWebappSettings()
	}
	block, ok := webappMapping(raw)
	if !ok {
		return off(fmt.Sprintf("webapp in %s must be a mapping", path), defaultWebappSettings())
	}
	host, hostOK, port, portOK, publicURL, urlOK := webappConfiguredAddress(block)
	configured := defaultWebappSettings()
	if hostOK && host != "" {
		configured.Host = host
	}
	if portOK && port != 0 {
		configured.Port = port
	}
	if urlOK {
		configured.PublicURL = publicURL
	}
	enabled, has := block["enabled"]
	if !has || enabled == false {
		return configured
	}
	if enabled != true {
		return off(fmt.Sprintf("webapp.enabled must be true or false in %s", path), configured)
	}
	if !hostOK {
		return off(fmt.Sprintf("webapp.host must be 127.0.0.1, ::1, localhost, 0.0.0.0 or :: in %s", path), configured)
	}
	if !portOK {
		return off(fmt.Sprintf("webapp.port must be a port number (1-65535) in %s", path), configured)
	}
	if !urlOK {
		return off(fmt.Sprintf("webapp.public_url must be an http:// or https:// address with no credentials, "+
			"query or fragment in %s", path), configured)
	}
	configured.Enabled = true
	return configured
}

// Host, port and public URL of a block, each false when invalid
func webappConfiguredAddress(block map[string]any) (string, bool, int, bool, string, bool) {
	rawHost, has := block["host"]
	if !has {
		rawHost = webappDefaultHost
	}
	host, hostOK := "", false
	if s, ok := rawHost.(string); ok {
		host = webappCanonicalHost(webappBareHost(s))
		hostOK = webappLocalOnly(host) || webappIsWildcard(host)
	}
	if !hostOK {
		host = ""
	}
	rawPort, has := block["port"]
	if !has {
		rawPort = webappDefaultPort
	}
	port, portOK := webappPortNumber(rawPort)
	publicURL, urlOK := webappPublicURL(block["public_url"])
	return host, hostOK, port, portOK, publicURL, urlOK
}

// Integer port in 1 through 65535 of a YAML value, booleans refused
func webappPortNumber(v any) (int, bool) {
	var n *big.Int
	switch x := v.(type) {
	case int:
		n = big.NewInt(int64(x))
	case int64:
		n = big.NewInt(x)
	case uint64:
		n = new(big.Int).SetUint64(x)
	case *big.Int:
		n = x
	default:
		return 0, false
	}
	if n.Cmp(big.NewInt(1)) < 0 || n.Cmp(big.NewInt(65535)) > 0 {
		return 0, false
	}
	return int(n.Int64()), true
}

// Host without surrounding space or the brackets a URL puts around an IPv6 address
func webappBareHost(host string) string {
	host = pystr.Strip(host)
	if len(host) >= 2 && strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		inner := host[1 : len(host)-1]
		if strings.Contains(inner, ":") && !strings.Contains(inner, "[") {
			return inner
		}
	}
	return host
}

// Host spelled as the sign in gate compares it, an IP in short form and a name in lower case
func webappCanonicalHost(host string) string {
	if addr, ok := pyipaddress.IPAddress(host); ok {
		return addr.String()
	}
	return strings.ToLower(host)
}

// True for the loopback spellings the sign in gate leaves open
func webappLocalOnly(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// True for the IPv4 and IPv6 wildcard binds
func webappIsWildcard(host string) bool { return host == "0.0.0.0" || host == "::" }

// Public URL without a trailing slash, empty when unset, false when malformed
func webappPublicURL(v any) (string, bool) {
	if v == nil || v == "" {
		return "", true
	}
	s, ok := v.(string)
	if !ok || strings.ContainsAny(s, "\"'<> \t\r\n?#@\\") {
		return "", false
	}
	url := strings.TrimRight(s, "/")
	parts, err := pyurl.Split(url)
	if err != nil {
		return "", false
	}
	port, hasPort, err := parts.Port()
	if err != nil {
		return "", false
	}
	if (parts.Scheme == "http" || parts.Scheme == "https") && parts.Hostname() != "" && !(hasPort && port == 0) {
		return url, true
	}
	return "", false
}

// Marker an update writes into a bundle it could not rebuild
const WebappStaleMarker = ".icebath-stale"

// Reasons the Webapp is not served, unbuilt or not rebuilt for this version
const (
	WebappNotBuilt   = "The Webapp is not built"
	WebappNotRebuilt = WebappNotBuilt + " for this version of Icebath: icebath update could not rebuild it. " +
		"Run icebath webapp to rebuild it."
)

// True when the bundle in dist carries the out of date marker
func WebappBundleStale(dist string) bool {
	info, err := os.Stat(filepath.Join(dist, WebappStaleMarker))
	return err == nil && info.Mode().IsRegular()
}

// Not rebuilt reason while the bundle is marked out of date, empty otherwise
func BundleStaleReason(dist string) string {
	if WebappBundleStale(dist) {
		return WebappNotRebuilt
	}
	return ""
}

// Why the Webapp cannot be served from dist, empty once a current bundle is built
func BundleMissingReason(dist string) string {
	index := filepath.Join(dist, "index.html")
	if info, err := os.Stat(index); err == nil && info.Mode().IsRegular() {
		return BundleStaleReason(dist)
	}
	return fmt.Sprintf("%s (%s is missing). Run icebath webapp to build it.", WebappNotBuilt, index)
}
