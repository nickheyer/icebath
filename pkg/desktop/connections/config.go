package connections

import (
	"errors"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	whatwg "github.com/nlnwa/whatwg-url/url"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// AtCookieVariants are the access token cookie names, icebath names read ahead of hermes names
var AtCookieVariants = []string{
	"__Host-icebath_session_at", "__Secure-icebath_session_at", "icebath_session_at",
	"__Host-hermes_session_at", "__Secure-hermes_session_at", "hermes_session_at",
}

// RtCookieVariants are the refresh token cookie names, icebath names read ahead of hermes names
var RtCookieVariants = []string{
	"__Host-icebath_session_rt", "__Secure-icebath_session_rt", "icebath_session_rt",
	"__Host-hermes_session_rt", "__Secure-hermes_session_rt", "hermes_session_rt",
}

var gatewayCookieSuffix = regexp.MustCompile(`_[0-9a-f]{16}$`)

func isCookieVariant(name string, variants []string) bool {
	return slices.Contains(variants, name) || slices.Contains(variants, gatewayCookieSuffix.ReplaceAllString(name, ""))
}

var reservedRemoteProfiles = map[string]bool{"icebath": true, "test": true, "tmp": true, "root": true, "sudo": true}

var (
	schemePrefix      = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)
	trailingSlashes   = regexp.MustCompile(`/+$`)
	remoteProfileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// Parses a URL with the WHATWG rules Node and Chromium apply
func parseURL(raw string) (*whatwg.Url, error) { return whatwg.Parse(raw) }

// Parses a reference against a base with the WHATWG rules
func parseURLRef(ref, base string) (*whatwg.Url, error) { return whatwg.ParseRef(base, ref) }

// NormalizeRemoteBaseURL validates a gateway URL and returns it without hash, query or trailing slash
func NormalizeRemoteBaseURL(rawURL any) (string, error) {
	value := strings.TrimSpace(jsOrString(rawURL))
	if value == "" {
		return "", errors.New("Remote gateway URL is required.")
	}
	if !schemePrefix.MatchString(value) {
		value = "http://" + value
	}
	parsed, err := parseURL(value)
	if err != nil {
		return "", errors.New("Remote gateway URL is not valid: Invalid URL")
	}
	if parsed.Protocol() != "http:" && parsed.Protocol() != "https:" {
		return "", errors.New("Remote gateway URL must be http:// or https://, got " + parsed.Protocol())
	}
	parsed.SetHash("")
	parsed.SetSearch("")
	parsed.SetPathname(trailingSlashes.ReplaceAllString(parsed.Pathname(), ""))
	return trailingSlashes.ReplaceAllString(parsed.Href(false), ""), nil
}

func wsBase(baseURL string) (string, error) {
	parsed, err := parseURL(baseURL)
	if err != nil {
		return "", err
	}
	scheme := "ws"
	if parsed.Protocol() == "https:" {
		scheme = "wss"
	}
	return scheme + "://" + parsed.Host() + trailingSlashes.ReplaceAllString(parsed.Pathname(), ""), nil
}

// BuildGatewayWsURL is the gateway socket URL authenticated by a session token
func BuildGatewayWsURL(baseURL, token string) (string, error) {
	base, err := wsBase(baseURL)
	if err != nil {
		return "", err
	}
	return base + "/api/ws?token=" + encodeURIComponent(token), nil
}

// BuildGatewayWsURLWithTicket is the gateway socket URL authenticated by a one use ticket
func BuildGatewayWsURLWithTicket(baseURL, ticket string) (string, error) {
	base, err := wsBase(baseURL)
	if err != nil {
		return "", err
	}
	return base + "/api/ws?ticket=" + encodeURIComponent(ticket), nil
}

// Percent encodes like JavaScript encodeURIComponent
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(string("0123456789abcdef"[c>>4])+string("0123456789abcdef"[c&15])))
	}
	return b.String()
}

// GatewayError is a failed gateway exchange with the flags the ticket, probe and reauth paths read
type GatewayError struct {
	Message          string
	Status           int
	NeedsOauth       bool
	IsReauthRequired bool
	AppTokenRejected bool
	Code             string
	Cause            error
}

func (e *GatewayError) Error() string { return e.Message }
func (e *GatewayError) Unwrap() error { return e.Cause }

// NeedsOauthLogin reports a rejection the user resolves by signing in again
func (e *GatewayError) NeedsOauthLogin() bool { return e.NeedsOauth }

// HTTP status a failure carries on itself, false when it has none
func readStatusCode(err error) (int, bool) {
	switch e := err.(type) {
	case *GatewayError:
		return e.Status, e.Status != 0
	case *backend.HTTPError:
		return e.StatusCode, e.StatusCode != 0
	case *backend.CloudBackendDownError:
		return e.StatusCode, e.StatusCode != 0
	case interface{ HTTPStatus() int }:
		return e.HTTPStatus(), e.HTTPStatus() != 0
	}
	return 0, false
}

// IsGatewayAuthRejection reports an explicit sign in demand or a 401 or 403 answer
func IsGatewayAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	if n, ok := err.(interface{ NeedsOauthLogin() bool }); ok && n.NeedsOauthLogin() {
		return true
	}
	status, _ := readStatusCode(err)
	return status == 401 || status == 403
}

func isStaleAppTokenRejection(err error) bool {
	g, ok := err.(*GatewayError)
	return ok && g.AppTokenRejected
}

// Node style error code of a transport failure, empty when none applies
func errorCode(err error) string {
	var g *GatewayError
	if errors.As(err, &g) && g.Code != "" {
		return g.Code
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsNotFound {
			return "ENOTFOUND"
		}
		if dns.IsTimeout {
			return "ETIMEOUT"
		}
		return "EAI_AGAIN"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ECONNREFUSED"
	case errors.Is(err, syscall.ECONNRESET):
		return "ECONNRESET"
	case errors.Is(err, syscall.ENETUNREACH):
		return "ENETUNREACH"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "EHOSTUNREACH"
	case errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, os.ErrDeadlineExceeded):
		return "ETIMEDOUT"
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "ETIMEDOUT"
	}
	return ""
}

// GatewayTicketTransportMessage explains a ticket refresh that never reached the gateway
func GatewayTicketTransportMessage(err error) string {
	switch errorCode(err) {
	case "ETIMEDOUT", "ETIMEOUT", "ENETUNREACH", "EHOSTUNREACH", "EAI_AGAIN", "ENOTFOUND":
		return "Could not reach the remote Icebath gateway while refreshing its WebSocket ticket: " +
			"the connection timed out or the host could not be resolved. " +
			"Check your network/VPN path and the gateway URL, then reconnect."
	case "ECONNREFUSED":
		return "Could not reach the remote Icebath gateway while refreshing its WebSocket ticket: " +
			"the connection was refused. The gateway process is likely down or listening on another port. " +
			"Start the gateway (or fix its URL), then reconnect."
	}
	return "Could not reach the remote Icebath gateway while refreshing its WebSocket ticket. Try reconnecting."
}

// GatewayTicketFailure wraps a ticket mint failure as a reauth demand or a retryable transport error
func GatewayTicketFailure(err error, authMessage, transportMessage string) *GatewayError {
	needsOauth := IsGatewayAuthRejection(err)
	message := transportMessage
	if needsOauth {
		message = authMessage
		if isStaleAppTokenRejection(err) {
			message = "Reached the gateway over HTTP, but the app token is invalid. " +
				"The app's saved gateway bearer is no longer valid. Sign out in the app and sign in again to replace it."
		}
	}
	out := &GatewayError{Message: message, Cause: err}
	if needsOauth {
		out.NeedsOauth = true
		out.IsReauthRequired = true
	}
	if status, ok := readStatusCode(err); ok {
		out.Status = status
	}
	return out
}

// RetryOptions tunes WithTransientRetries, zero values take the defaults
type RetryOptions struct {
	Attempts    int
	DelaysMs    []int
	Sleep       func(ms int)
	IsRetryable func(error) bool
}

// WithTransientRetries reruns a gateway exchange across transport blips, never across an auth rejection
func WithTransientRetries[T any](run func() (T, error), o RetryOptions) (T, error) {
	attempts := o.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	delays := o.DelaysMs
	if len(delays) == 0 {
		delays = []int{250, 750}
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	}
	retryable := o.IsRetryable
	if retryable == nil {
		retryable = func(err error) bool { return !IsGatewayAuthRejection(err) }
	}
	var zero T
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		v, err := run()
		if err == nil {
			return v, nil
		}
		last = err
		if !retryable(err) || attempt >= attempts-1 {
			return zero, err
		}
		sleep(delays[min(attempt, len(delays)-1)])
	}
	return zero, last
}

// GatewayWsURLResult is the IPC shape of a fresh gateway socket URL or its failure
type GatewayWsURLResult struct {
	OK              bool   `json:"ok"`
	WsURL           string `json:"wsUrl,omitempty"`
	Error           string `json:"error,omitempty"`
	NeedsOauthLogin bool   `json:"needsOauthLogin,omitempty"`
}

// GatewayWsURLIpcResult runs a socket URL resolver and serializes its auth or transport outcome
func GatewayWsURLIpcResult(resolve func() (string, error)) GatewayWsURLResult {
	ws, err := resolve()
	if err != nil {
		return GatewayWsURLResult{Error: err.Error(), NeedsOauthLogin: IsGatewayAuthRejection(err)}
	}
	return GatewayWsURLResult{OK: true, WsURL: ws}
}

// ResolveTestWsURL is the socket URL a connection test probes, empty when a token gateway has no token
func ResolveTestWsURL(baseURL, authMode, token string, mintTicket func(string) (string, error)) (string, error) {
	if authMode == "oauth" {
		if mintTicket == nil {
			return "", errors.New("resolveTestWsUrl: a mintTicket function is required in OAuth mode.")
		}
		ticket, err := mintTicket(baseURL)
		if err != nil {
			return "", GatewayTicketFailure(err,
				"Reached the gateway over HTTP, but the OAuth session was rejected while minting a WebSocket ticket. "+
					"Open Settings → Gateway and sign in again.",
				"Reached the gateway over HTTP, but could not mint a WebSocket ticket. Check the remote gateway connection and try again.")
		}
		return BuildGatewayWsURLWithTicket(baseURL, ticket)
	}
	if token == "" {
		return "", nil
	}
	return BuildGatewayWsURL(baseURL, token)
}

// ConnectionScopeKey is the trimmed profile name, empty for the global scope
func ConnectionScopeKey(profile any) string {
	return strings.TrimSpace(jsNullishString(profile))
}

// ResolveRemoteSSHDashboardProfile is the remote profile an SSH dashboard serves, never a conn pool key
func ResolveRemoteSSHDashboardProfile(configuredRemoteProfile any, poolOrProfileKey any) string {
	configured := strings.TrimSpace(jsOrString(configuredRemoteProfile))
	if configured != "" {
		if configured == "default" {
			return ""
		}
		return configured
	}
	key := strings.TrimSpace(jsOrString(poolOrProfileKey))
	requested := key
	if strings.HasPrefix(key, "conn:") {
		parts := strings.Split(key, "::")
		requested = parts[len(parts)-1]
	}
	if requested == "" || requested == "default" {
		return ""
	}
	return requested
}

// NormAuthMode coerces an auth mode to token unless it is exactly oauth
func NormAuthMode(mode any) string {
	if mode == "oauth" {
		return "oauth"
	}
	return "token"
}

var remoteHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

var forbiddenRemoteHeaderNames = map[string]bool{
	"authorization": true, "connection": true, "content-length": true, "content-type": true, "cookie": true,
	"host": true, "origin": true, "referer": true, "te": true, "trailer": true, "transfer-encoding": true,
	"upgrade": true, "x-hermes-session-token": true, "x-icebath-session-token": true,
}

var crlf = regexp.MustCompile(`[\r\n]+`)

// SanitizeRemoteHeaderValue strips CR and LF and trims so a pasted value cannot split a request
func SanitizeRemoteHeaderValue(value any) string {
	return strings.TrimSpace(crlf.ReplaceAllString(jsOrString(value), ""))
}

// SecretEnvelope is a stored secret, plain text or encrypted by the OS keychain policy
type SecretEnvelope struct {
	Encoding string `json:"encoding"`
	Value    string `json:"value"`
}

// NormalizeRemoteHeaders keeps safe proxy headers as secret envelopes and drops transport and auth headers
func NormalizeRemoteHeaders(raw any) map[string]SecretEnvelope {
	out := map[string]SecretEnvelope{}
	obj := jsObject(raw)
	if obj == nil {
		return out
	}
	for name, secret := range obj {
		headerName := strings.TrimSpace(name)
		if headerName == "" || !remoteHeaderName.MatchString(headerName) || forbiddenRemoteHeaderNames[strings.ToLower(headerName)] {
			continue
		}
		if s, ok := secret.(string); ok {
			if value := SanitizeRemoteHeaderValue(s); value != "" {
				out[headerName] = SecretEnvelope{Encoding: "plain", Value: value}
			}
			continue
		}
		if env := jsObject(secret); env != nil {
			encoding := jsOrString(env["encoding"])
			value := jsOrString(env["value"])
			if value != "" && (encoding == "safeStorage" || encoding == "plain" || encoding == "") {
				if encoding == "" {
					encoding = "plain"
				}
				out[headerName] = SecretEnvelope{Encoding: encoding, Value: value}
			}
		}
	}
	return out
}

// RemoteRequestMatchesBaseURL reports a request URL inside a gateway base URL, ws schemes matching http ones
func RemoteRequestMatchesBaseURL(requestURL, baseURL string) bool {
	request, err := parseURL(requestURL)
	if err != nil {
		return false
	}
	base, err := parseURL(baseURL)
	if err != nil {
		return false
	}
	basePath := trailingSlashes.ReplaceAllString(base.Pathname(), "")
	httpish := func(p string) string {
		switch p {
		case "ws:":
			return "http:"
		case "wss:":
			return "https:"
		}
		return p
	}
	if httpish(request.Protocol()) != httpish(base.Protocol()) || request.Host() != base.Host() {
		return false
	}
	return basePath == "" || request.Pathname() == basePath || strings.HasPrefix(request.Pathname(), basePath+"/")
}

// ModeIsRemoteLike reports the URL shaped remote modes
func ModeIsRemoteLike(mode any) bool { return mode == "remote" || mode == "cloud" }

// SSHConfig is a normalized SSH target
type SSHConfig struct {
	Mode             string `json:"mode"`
	Host             string `json:"host"`
	User             string `json:"user,omitempty"`
	Port             int    `json:"port,omitempty"`
	KeyPath          string `json:"keyPath,omitempty"`
	RemoteHermesPath string `json:"remoteHermesPath,omitempty"`
	RemoteProfile    string `json:"remoteProfile,omitempty"`
}

var (
	sshCommandPrefix = regexp.MustCompile(`(?i)^ssh\s+`)
	bracketedHost    = regexp.MustCompile(`^\[([^\]]+)](?::(\d+))?$`)
	digitsOnly       = regexp.MustCompile(`^\d+$`)
)

// NormalizeSSHConfig parses an SSH entry, folding user@host:port forms, nil when it names no host
func NormalizeSSHConfig(entry any) *SSHConfig {
	obj := jsObject(entry)
	if obj == nil || obj["mode"] != "ssh" {
		return nil
	}
	host := strings.TrimSpace(jsOrString(obj["host"]))
	host = strings.TrimSpace(sshCommandPrefix.ReplaceAllString(host, ""))
	if host == "" {
		return nil
	}
	parsedUser := ""
	parsedPort := -1.0
	if at := strings.Index(host, "@"); at > 0 {
		parsedUser = host[:at]
		host = host[at+1:]
	}
	if m := bracketedHost.FindStringSubmatch(host); m != nil {
		host = m[1]
		if m[2] != "" {
			parsedPort = digitsValue(m[2])
		}
	} else if strings.Count(host, ":") == 1 {
		name, rawPort, _ := strings.Cut(host, ":")
		if digitsOnly.MatchString(rawPort) {
			host = name
			parsedPort = digitsValue(rawPort)
		}
	}
	if host == "" {
		return nil
	}
	out := &SSHConfig{Mode: "ssh", Host: host}
	if user := strings.TrimSpace(jsOrString(obj["user"])); user != "" {
		out.User = user
	} else {
		out.User = parsedUser
	}
	port := parsedPort
	if raw := strings.TrimSpace(jsNullishString(obj["port"])); digitsOnly.MatchString(raw) {
		port = digitsValue(raw)
	}
	if port == float64(int(port)) && port > 0 && port <= 65535 && port != 22 {
		out.Port = int(port)
	}
	out.KeyPath = strings.TrimSpace(jsOrString(obj["keyPath"]))
	out.RemoteHermesPath = strings.TrimSpace(jsOrString(obj["remoteHermesPath"]))
	if rp := strings.TrimSpace(jsOrString(obj["remoteProfile"])); remoteProfileName.MatchString(rp) && !reservedRemoteProfiles[rp] {
		out.RemoteProfile = rp
	}
	return out
}

// Numeric value of a digit string the way Number() reads it
func digitsValue(s string) float64 {
	v := 0.0
	for _, c := range s {
		v = v*10 + float64(c-'0')
	}
	return v
}

// Object form of a normalized SSH config for spreading into other entries
func (s *SSHConfig) object() map[string]any {
	out := map[string]any{"mode": "ssh", "host": s.Host}
	if s.User != "" {
		out["user"] = s.User
	}
	if s.Port != 0 {
		out["port"] = float64(s.Port)
	}
	if s.KeyPath != "" {
		out["keyPath"] = s.KeyPath
	}
	if s.RemoteHermesPath != "" {
		out["remoteHermesPath"] = s.RemoteHermesPath
	}
	if s.RemoteProfile != "" {
		out["remoteProfile"] = s.RemoteProfile
	}
	return out
}

func profileEntry(config map[string]any, profile any) map[string]any {
	key := ConnectionScopeKey(profile)
	if key == "" || config == nil {
		return nil
	}
	return jsObject(jsObject(config["profiles"])[key])
}

// ProfileSSHOverride is the active SSH target a profile overrides with, nil when none
func ProfileSSHOverride(config map[string]any, profile any) *SSHConfig {
	entry := profileEntry(config, profile)
	if entry == nil {
		return nil
	}
	return NormalizeSSHConfig(entry)
}

// SavedProfileSSH is the inactive SSH draft a local profile keeps, nil when none
func SavedProfileSSH(config map[string]any, profile any) *SSHConfig {
	entry := profileEntry(config, profile)
	if entry == nil || entry["mode"] != "local" {
		return nil
	}
	return NormalizeSSHConfig(entry["savedSsh"])
}

// ProfileHasRemoteConnection reports a profile overriding the global connection with a remote or SSH target
func ProfileHasRemoteConnection(config map[string]any, profile any) bool {
	return ProfileRemoteOverride(config, profile) != nil || ProfileSSHOverride(config, profile) != nil
}

// LocalProfileEntry is the local entry that keeps an SSH draft inactive, nil when there is no draft
func LocalProfileEntry(existing any) map[string]any {
	ssh := NormalizeSSHConfig(existing)
	if ssh == nil {
		ssh = NormalizeSSHConfig(jsObject(existing)["savedSsh"])
	}
	if ssh == nil {
		return nil
	}
	return map[string]any{"mode": "local", "savedSsh": ssh.object()}
}

// HostLabelFromBaseURL is the host and non default port of a gateway URL, empty when it has no host
func HostLabelFromBaseURL(baseURL any) string {
	raw := strings.TrimSpace(jsOrString(baseURL))
	if raw == "" {
		return ""
	}
	parsed, err := parseURL(raw)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return parsed.Hostname() + ":" + port
	}
	return parsed.Hostname()
}

// RemoteOverride is a profile's own URL shaped remote gateway
type RemoteOverride struct {
	URL           string                    `json:"url"`
	AuthMode      string                    `json:"authMode"`
	Token         any                       `json:"token,omitempty"`
	Headers       map[string]SecretEnvelope `json:"headers,omitempty"`
	RemoteProfile string                    `json:"remoteProfile,omitempty"`
}

// ProfileRemoteOverride is the remote or cloud gateway a profile overrides with, nil when none
func ProfileRemoteOverride(config map[string]any, profile any) *RemoteOverride {
	entry := profileEntry(config, profile)
	if entry == nil || !ModeIsRemoteLike(entry["mode"]) {
		return nil
	}
	u := strings.TrimSpace(jsOrString(entry["url"]))
	if u == "" {
		return nil
	}
	out := &RemoteOverride{URL: u, AuthMode: NormAuthMode(entry["authMode"]), Token: entry["token"]}
	if headers := NormalizeRemoteHeaders(entry["headers"]); len(headers) > 0 {
		out.Headers = headers
	}
	out.RemoteProfile = NormalizeRemoteProfileName(entry["remoteProfile"])
	return out
}

// NormalizeRemoteProfileName is a safe remote profile mapping, empty when invalid or reserved
func NormalizeRemoteProfileName(value any) string {
	rp := strings.TrimSpace(jsOrString(value))
	if remoteProfileName.MatchString(rp) && !reservedRemoteProfiles[rp] {
		return rp
	}
	return ""
}

// ProfileRouteOptions are the facts profile backend routing decides on
type ProfileRouteOptions struct {
	BackendProfile        string
	GlobalRemote          bool
	PrimaryProfile        string
	ProfileRemoteOverride bool
	PrimaryRemoteActive   bool
	OwnEntry              bool
	IsolatedBackend       bool
	RequestMethod         string
	RequestPath           string
}

// ProfileBackendRoute names the backend a profile request uses, DescriptorProfile empty when untagged
type ProfileBackendRoute struct {
	Backend           string `json:"backend"`
	DescriptorProfile string `json:"descriptorProfile"`
	ScopePath         bool   `json:"scopePath"`
}

var localPrimaryScopedRoutes = map[string]bool{}

func init() {
	for _, r := range []string{
		"GET /api/config", "PUT /api/config", "GET /api/config/raw", "PUT /api/config/raw", "GET /api/config/schema",
		"DELETE /api/env", "GET /api/env", "PUT /api/env", "POST /api/env/reveal",
		"GET /api/model/auxiliary", "GET /api/model/info", "GET /api/model/moa", "PUT /api/model/moa",
		"GET /api/model/options", "POST /api/model/set",
		"GET /api/skills", "GET /api/skills/content", "PUT /api/skills/toggle", "POST /api/skills/hub/install",
		"GET /api/skills/hub/official", "GET /api/skills/hub/preview", "GET /api/skills/hub/scan",
		"GET /api/skills/hub/search", "GET /api/skills/hub/sources", "POST /api/skills/hub/uninstall",
		"POST /api/skills/hub/update", "POST /api/mcp/catalog/install",
		"POST /api/gateway/restart", "POST /api/gateway/start", "POST /api/gateway/stop",
		"GET /api/memory", "PUT /api/memory/provider", "POST /api/memory/reset",
		"GET /api/curator", "PUT /api/curator/paused", "POST /api/curator/run",
		"GET /api/logs", "GET /api/portal", "GET /api/icebath/update/check", "POST /api/local-models/activate",
		"GET /api/dashboard/themes", "PUT /api/dashboard/theme", "GET /api/dashboard/font", "PUT /api/dashboard/font",
		"GET /api/dashboard/plugins", "GET /api/webapp-access/status", "POST /api/webapp-access/start",
		"POST /api/webapp-access/stop", "POST /api/webapp-access/restart",
	} {
		localPrimaryScopedRoutes[r] = true
	}
}

// Whether a request is scoped on the primary, nil when the route has no fixed answer
func localPrimaryRequestScope(o ProfileRouteOptions) *bool {
	yes, no := true, false
	if o.RequestPath == "" {
		return nil
	}
	parsed, err := parseURLRef(o.RequestPath, "https://example.invalid")
	if err != nil {
		return nil
	}
	pathname := parsed.Pathname()
	method := strings.ToUpper(o.RequestMethod)
	if method == "" {
		method = "GET"
	}
	sessions := pathname == "/api/sessions" || strings.HasPrefix(pathname, "/api/sessions/")
	switch {
	case localPrimaryScopedRoutes[method+" "+pathname], strings.HasPrefix(pathname, "/api/actions/"):
		return &yes
	case method == "GET" && sessions:
		return &yes
	case pathname == "/api/tools" || strings.HasPrefix(pathname, "/api/tools/"):
		return &yes
	case pathname == "/api/profiles" || strings.HasPrefix(pathname, "/api/profiles/"):
		return &no
	case pathname == "/api/webhooks" || strings.HasPrefix(pathname, "/api/webhooks/"):
		return &yes
	case strings.HasPrefix(pathname, "/api/ops/"):
		return &yes
	case method != "GET" && sessions:
		return &no
	}
	return nil
}

var safeRequestMethods = map[string]bool{"GET": true, "HEAD": true, "OPTIONS": true}

// UnscopableMutatingRequest reports a mutating request whose route the shared backend cannot scope
func UnscopableMutatingRequest(o ProfileRouteOptions) bool {
	if o.RequestPath == "" {
		return false
	}
	method := strings.ToUpper(o.RequestMethod)
	if method == "" {
		method = "GET"
	}
	if safeRequestMethods[method] {
		return false
	}
	return localPrimaryRequestScope(o) == nil
}

// SharesHostBackend reports a profile served by the one host backend rather than a private one
func SharesHostBackend(isolated, unscopableRequest, profileRemoteOverride, primaryRemoteActive bool) bool {
	if isolated || unscopableRequest {
		return false
	}
	return !profileRemoteOverride && !primaryRemoteActive
}

// ResolveProfileBackendRoute picks the primary or a pooled backend for a profile request
func ResolveProfileBackendRoute(profile any, o ProfileRouteOptions) ProfileBackendRoute {
	scoped := ConnectionScopeKey(profile)
	primary := ConnectionScopeKey(o.PrimaryProfile)
	if primary == "" {
		primary = "default"
	}
	unscoped := ProfileBackendRoute{Backend: "primary"}
	tagged := ProfileBackendRoute{Backend: "primary", DescriptorProfile: scoped, ScopePath: true}
	pool := ProfileBackendRoute{Backend: "pool"}
	if scoped == "" {
		return unscoped
	}
	if scoped == primary {
		if o.GlobalRemote {
			return tagged
		}
		if s := localPrimaryRequestScope(o); s != nil && *s {
			return tagged
		}
		return unscoped
	}
	if o.ProfileRemoteOverride {
		return pool
	}
	if o.GlobalRemote {
		return tagged
	}
	if o.PrimaryRemoteActive {
		if !o.OwnEntry {
			return tagged
		}
		return pool
	}
	if s := localPrimaryRequestScope(o); s != nil {
		if *s {
			return tagged
		}
		return unscoped
	}
	if SharesHostBackend(o.IsolatedBackend, UnscopableMutatingRequest(o), false, false) {
		return tagged
	}
	return pool
}

// PathWithGlobalRemoteProfile scopes a request path to the profile when the route shares a backend
func PathWithGlobalRemoteProfile(path string, profile any, o ProfileRouteOptions) string {
	if translated := TranslateSelfProfileQuery(path, profile, o.BackendProfile); translated != path {
		return translated
	}
	if !ResolveProfileBackendRoute(profile, o).ScopePath {
		return path
	}
	return PathWithProfileScope(path, profile, "")
}

var selfProfileQueryKeysByPath = map[string][]string{"/api/profiles/sessions/sidebar": {"recents_profile"}}

// TranslateSelfProfileQuery rewrites a self profile filter from the desktop alias to the backend profile name
func TranslateSelfProfileQuery(path string, profile any, backendProfile any) string {
	scoped := ConnectionScopeKey(profile)
	backendKey := ConnectionScopeKey(backendProfile)
	if scoped == "" || backendKey == "" || backendKey == scoped || path == "" {
		return path
	}
	parsed, err := parseURLRef(path, "http://hermes.local")
	if err != nil {
		return path
	}
	keys := append([]string{"profile"}, selfProfileQueryKeysByPath[parsed.Pathname()]...)
	params := parseSearchParams(parsed.Query())
	changed := false
	for _, key := range keys {
		if value, ok := params.get(key); !ok || ConnectionScopeKey(value) != scoped {
			continue
		}
		params.set(key, backendKey)
		changed = true
	}
	if !changed {
		return path
	}
	return parsed.Pathname() + params.search() + parsed.Hash()
}

var profileCollectionRoutes = map[string][]string{
	"active": {"GET", "POST"}, "import": {"POST"}, "projects/tree": {"GET"}, "sessions": {"GET"},
	"sessions/pull-requests": {"POST"}, "sessions/sidebar": {"GET"},
}

// Profile name and tail of an /api/profiles/ path, empty for other paths
func profileRouteSuffix(pathname string) string {
	trimmed := trailingSlashes.ReplaceAllString(pathname, "")
	if strings.HasPrefix(trimmed, "/api/profiles/") {
		return trimmed[len("/api/profiles/"):]
	}
	return ""
}

// Collection route a profiles path names for the method, empty when the path names a profile
func profileCollectionRoute(pathname, method string) string {
	suffix := profileRouteSuffix(pathname)
	methods, ok := profileCollectionRoutes[suffix]
	if suffix == "" || !ok {
		return ""
	}
	if method == "" || slices.Contains(methods, strings.ToUpper(method)) {
		return suffix
	}
	return ""
}

// PathWithProfileScope appends profile to the query unless the path already names its profile
func PathWithProfileScope(path string, profile any, method string) string {
	scoped := ConnectionScopeKey(profile)
	if scoped == "" || path == "" {
		return path
	}
	parsed, err := parseURLRef(path, "http://hermes.local")
	if err != nil {
		return path
	}
	params := parseSearchParams(parsed.Query())
	if _, ok := params.get("profile"); ok || (profileRouteSuffix(parsed.Pathname()) != "" && profileCollectionRoute(parsed.Pathname(), method) == "") {
		return path
	}
	params.set("profile", scoped)
	return parsed.Pathname() + params.search() + parsed.Hash()
}

// searchParams is a URLSearchParams list with the WHATWG form urlencoded parser and serializer
type searchParams struct{ pairs [][2]string }

func parseSearchParams(query string) *searchParams {
	out := &searchParams{}
	for _, seq := range strings.Split(query, "&") {
		if seq == "" {
			continue
		}
		name, value, _ := strings.Cut(seq, "=")
		out.pairs = append(out.pairs, [2]string{formDecode(name), formDecode(value)})
	}
	return out
}

func formDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

func formEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("*-._", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(string("0123456789abcdef"[c>>4])+string("0123456789abcdef"[c&15])))
		}
	}
	return b.String()
}

func (s *searchParams) get(name string) (string, bool) {
	for _, p := range s.pairs {
		if p[0] == name {
			return p[1], true
		}
	}
	return "", false
}

// Sets the first pair of the name and drops the rest, appending when absent
func (s *searchParams) set(name, value string) {
	out := s.pairs[:0]
	found := false
	for _, p := range s.pairs {
		if p[0] == name {
			if found {
				continue
			}
			p[1] = value
			found = true
		}
		out = append(out, p)
	}
	if !found {
		out = append(out, [2]string{name, value})
	}
	s.pairs = out
}

// Query with its leading question mark, empty when there are no pairs
func (s *searchParams) search() string {
	parts := make([]string, len(s.pairs))
	for i, p := range s.pairs {
		parts[i] = formEncode(p[0]) + "=" + formEncode(p[1])
	}
	if len(parts) == 0 {
		return ""
	}
	return "?" + strings.Join(parts, "&")
}

// RegistryBackendRequestScope is how a registry backend serves a profile's requests
type RegistryBackendRequestScope struct {
	Mode          string `json:"mode,omitempty"`
	RemoteProfile string `json:"remoteProfile,omitempty"`
	SharedPrimary bool   `json:"sharedPrimary,omitempty"`
	SharedRemote  bool   `json:"sharedRemote,omitempty"`
}

// PathForRegistryBackendRequest scopes a request path for the registry backend that serves it
func PathForRegistryBackendRequest(path string, profile any, b RegistryBackendRequestScope, method string) string {
	if b.SharedRemote || b.SharedPrimary || b.Mode == "local" {
		return PathWithProfileScope(path, profile, method)
	}
	return TranslateSelfProfileQuery(path, profile, b.RemoteProfile)
}

// APIRequestRegistryConnectionID is the trimmed connection id an api request names, empty when unscoped
func APIRequestRegistryConnectionID(request any) string {
	obj := jsObject(request)
	if obj == nil {
		return ""
	}
	return strings.TrimSpace(jsNullishString(obj["connectionId"]))
}

// ProfileAPIRequestRoute is the backend profile and path a profile api request uses
type ProfileAPIRequestRoute struct {
	BackendProfile string `json:"backendProfile"`
	RequestPath    string `json:"requestPath"`
}

// ResolveProfileAPIRequest routes a profile api request to its backend and scoped path
func ResolveProfileAPIRequest(profile any, path string, o ProfileRouteOptions) ProfileAPIRequestRoute {
	scoped := ConnectionScopeKey(profile)
	o.RequestPath = path
	route := ResolveProfileBackendRoute(scoped, o)
	out := ProfileAPIRequestRoute{RequestPath: PathWithGlobalRemoteProfile(path, scoped, o)}
	if route.Backend == "pool" {
		out.BackendProfile = scoped
	}
	return out
}

// TokenPreview masks a token to its last six characters, set for short ones, empty for none
func TokenPreview(value any) string {
	raw := jsOrString(value)
	if raw == "" {
		return ""
	}
	if len([]rune(raw)) <= 8 {
		return "set"
	}
	r := []rune(raw)
	return "..." + string(r[len(r)-6:])
}

// AuthModeFromStatus is oauth when a gateway status says auth is required
func AuthModeFromStatus(statusBody any) string {
	if jsTruthy(jsObject(statusBody)["auth_required"]) {
		return "oauth"
	}
	return "token"
}

// ResolveAuthMode prefers an explicit input auth mode, then the stored one, then token
func ResolveAuthMode(inputAuthMode, existingAuthMode any) string {
	switch {
	case inputAuthMode == "oauth":
		return "oauth"
	case inputAuthMode == "token":
		return "token"
	case existingAuthMode == "oauth":
		return "oauth"
	}
	return "token"
}

// Cookie is one browser cookie as the session checks read it
type Cookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CookiesHaveSession reports an access token cookie with a value
func CookiesHaveSession(cookies []Cookie) bool {
	for _, c := range cookies {
		if c.Value != "" && isCookieVariant(c.Name, AtCookieVariants) {
			return true
		}
	}
	return false
}

// CookiesHaveLiveSession reports an access or refresh token cookie with a value
func CookiesHaveLiveSession(cookies []Cookie) bool {
	for _, c := range cookies {
		if c.Value != "" && (isCookieVariant(c.Name, AtCookieVariants) || isCookieVariant(c.Name, RtCookieVariants)) {
			return true
		}
	}
	return false
}
