package mcp

import (
	"context"
	"strings"
)

// Published Client ID Metadata Document, on an origin that never redirects as CIMD fetchers require
const cimdClientMetadataURL = "https://nousresearch.github.io/hermes-agent/docs/oauth/client-metadata.json"

// Loopback callback ports the published document declares, below the Linux ephemeral floor
var cimdPorts = []int{27890, 27891, 27892, 27893, 27894}

// Loopback hostnames the published document declares
var cimdRedirectHosts = map[string]bool{"127.0.0.1": true, "localhost": true}

// Pinned CIMD ports this process committed to, never released, guarded by reservedMu
var assignedCIMDPorts []int

// Reports whether a port is one of the published CIMD callback ports
func isCIMDPort(port int) bool {
	for _, p := range cimdPorts {
		if p == port {
			return true
		}
	}
	return false
}

// Reports whether a port is already committed to a CIMD flow in this process
func cimdPortAssigned(port int) bool {
	reservedMu.Lock()
	defer reservedMu.Unlock()
	for _, p := range assignedCIMDPorts {
		if p == port {
			return true
		}
	}
	return false
}

// Commits a CIMD port to this process unless it already is
func claimCIMDPort(port int) {
	reservedMu.Lock()
	defer reservedMu.Unlock()
	for _, p := range assignedCIMDPorts {
		if p == port {
			return
		}
	}
	assignedCIMDPorts = append(assignedCIMDPorts, port)
}

// Reports whether a URL may be a CIMD client_id, an https URL with a non root path
func isValidClientMetadataURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := pyURLParse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Path != "" && u.Path != "/"
}

// Reports whether a URL is usable as a CIMD client_id, refusing userinfo, fragments and dot segments
func isValidCIMDURL(raw string) bool {
	if !isValidClientMetadataURL(raw) {
		return false
	}
	u, err := pyURLParse(raw)
	if err != nil {
		return false
	}
	if at := strings.LastIndex(u.Netloc, "@"); at >= 0 {
		user, pass, hasPass := strings.Cut(u.Netloc[:at], ":")
		if user != "" || (hasPass && pass != "") {
			return false
		}
	}
	if u.Fragment != "" {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Reserves a pinned CIMD port, wrapping onto this process's own ports once every one is taken
func pickCIMDPort() (int, bool) {
	for _, port := range cimdPorts {
		if cimdPortAssigned(port) {
			continue
		}
		if _, ok, _ := bindReserved(port); ok {
			claimCIMDPort(port)
			return port, true
		}
	}
	reservedMu.Lock()
	defer reservedMu.Unlock()
	if len(assignedCIMDPorts) > 0 {
		return assignedCIMDPorts[0], true
	}
	return 0, false
}

// Reports whether cached metadata shows the server does not advertise CIMD
func serverDeclinedCIMD(storage *TokenStorage) bool {
	if storage == nil {
		return false
	}
	md := storage.LoadOAuthMetadata()
	return md != nil && (md.ClientIDMetadataDocumentSupported == nil || !*md.ClientIDMetadataDocumentSupported)
}

// CIMD client_id URL and pinned callback port, false when the flow must use dynamic registration
func maybeUseCIMD(ctx context.Context, cfg map[string]any, storage *TokenStorage) (string, int, bool) {
	urlValue := cfg["client_metadata_url"]
	if !pyTruthy(urlValue) {
		urlValue = cimdClientMetadataURL
	}
	url, isStr := urlValue.(string)
	authMethod := cfg["token_endpoint_auth_method"]
	if !pyTruthy(authMethod) {
		authMethod = "none"
	}
	redirectHost := cfg["redirect_host"]
	if !pyTruthy(redirectHost) {
		redirectHost = "127.0.0.1"
	}
	hostStr, hostIsStr := redirectHost.(string)
	ineligible := cfg["cimd"] == false ||
		!isStr || !isValidCIMDURL(url) ||
		pyTruthy(cfg["client_id"]) || pyTruthy(cfg["client_secret"]) ||
		pyTruthy(cfg["client_name"]) || authMethod != "none" ||
		dashboardOAuthFlowFrom(ctx) != nil ||
		pyTruthy(cfg["redirect_uri"]) || pyTruthy(cfg["redirect_port"]) ||
		!hostIsStr || !cimdRedirectHosts[hostStr] ||
		cachedClientInfo(storage) != nil ||
		(storage != nil && storage.CIMDRejected()) ||
		serverDeclinedCIMD(storage)
	if ineligible {
		return "", 0, false
	}
	port, ok := pickCIMDPort()
	if !ok {
		return "", 0, false
	}
	return url, port, true
}
