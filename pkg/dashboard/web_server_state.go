package dashboard

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/installidentity"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/webbundle"
)

const (
	sessionHeaderName              = "X-Icebath-Session-Token"
	legacySessionHeaderName        = "X-Hermes-Session-Token"
	dashboardEmbeddedChatEnabled   = true
	desktopMCPDiscoveryDelay       = time.Second
	dashboardHealthWindowSeconds   = 300.0
	dashboardSelftestInterval      = 60 * time.Second
	dashboardSelftestRoute         = "/api/sessions?limit=1"
	gatewayHealthTimeoutMax        = 1.0
	managedFileMaxBytes            = 100 * 1024 * 1024
	fsDataURLMaxBytes              = home.WebappAttachmentMaxBytes
	uploadChunkBytes               = 1024 * 1024
	GatewayRestartCooldownSeconds  = 10.0
	dashboardHealthErrorTimesLimit = 256
)

// Paths the loopback gate also accepts a query token on
var queryTokenAPIPaths = map[string]bool{"/api/files/download": true}

var (
	webStateMu           sync.RWMutex
	projectRoot          string
	webDist              string
	gatewayHealthURL     string
	gatewayHealthURLSet  bool
	gatewayHealthTimeout = 1.0
	sshOwnerNonce        string
	sshRuntimePurelib    *sshRuntimeIdentity
	sshRuntimeMarker     string
	voiceListLastError   *string
	installIDCache       = &installidentity.Cache{}
	appStateMu           sync.Mutex
	DashboardHealthState = NewDashboardHealth(dashboardHealthWindowSeconds)
)

// Directory identity of the runtime install recorded when the SSH owner nonce was applied
type sshRuntimeIdentity struct {
	path     string
	dev, ino uint64
}

func webServerLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.web_server")
}

// Module level values of the web server read at import, the project root, web dist and gateway health probe
func importWebServerState() {
	webStateMu.Lock()
	defer webStateMu.Unlock()
	projectRoot = pm.RepoRoot()
	if v, ok := os.LookupEnv("ICEBATH_WEB_DIST"); ok {
		webDist = v
	} else {
		webDist = webbundle.Dist(projectRoot)
	}
	gatewayHealthURL, gatewayHealthURLSet = os.LookupEnv("GATEWAY_HEALTH_URL")
	gatewayHealthTimeout = parseGatewayHealthTimeout()
}

// Gateway health probe timeout from GATEWAY_HEALTH_TIMEOUT, one second when invalid or non positive and capped at one
func parseGatewayHealthTimeout() float64 {
	raw, set := os.LookupEnv("GATEWAY_HEALTH_TIMEOUT")
	if !set {
		raw = "1"
	}
	timeout, err := pyint.ParseFloat(raw)
	if err != nil {
		webServerLog().Warn(fmt.Sprintf("Invalid GATEWAY_HEALTH_TIMEOUT value %s — using default 1.0s", pyQuoteOptional(raw, set)))
		timeout = 1.0
	}
	if timeout <= 0 {
		webServerLog().Warn(fmt.Sprintf("Invalid non-positive GATEWAY_HEALTH_TIMEOUT value %.3fs — using default 1.0s", timeout))
		timeout = 1.0
	} else if timeout > gatewayHealthTimeoutMax {
		webServerLog().Warn(fmt.Sprintf("Capping GATEWAY_HEALTH_TIMEOUT %.3fs to %.3fs for dashboard liveness probes", timeout, gatewayHealthTimeoutMax))
		timeout = gatewayHealthTimeoutMax
	}
	return timeout
}

// Python repr of an optional environment string, None when unset
func pyQuoteOptional(s string, set bool) string {
	if !set {
		return "None"
	}
	return pyrepr.Repr(s)
}

// Project root the web server resolved at import
func ProjectRoot() string {
	webStateMu.RLock()
	defer webStateMu.RUnlock()
	return projectRoot
}

// Built web UI directory the web server resolved at import
func WebDist() string {
	webStateMu.RLock()
	defer webStateMu.RUnlock()
	return webDist
}

// Gateway health URL from GATEWAY_HEALTH_URL and whether it is set
func GatewayHealthURL() (string, bool) {
	webStateMu.RLock()
	defer webStateMu.RUnlock()
	return gatewayHealthURL, gatewayHealthURLSet
}

// Gateway health probe timeout in seconds
func GatewayHealthTimeout() float64 {
	webStateMu.RLock()
	defer webStateMu.RUnlock()
	return gatewayHealthTimeout
}

// Named app state value, created by the factory and stored on first read
func appStateDefault(app *FastAPI, name string, factory func() any) any {
	appStateMu.Lock()
	defer appStateMu.Unlock()
	if v, ok := app.State.Lookup(name); ok {
		return v
	}
	v := factory()
	app.State.Set(name, v)
	return v
}

// Lock serialising chat argv rewrites of the app
func getChatArgvLock(app *FastAPI) *sync.Mutex {
	return appStateDefault(app, "chat_argv_lock", func() any { return &sync.Mutex{} }).(*sync.Mutex)
}

// Active PTY session marker files of the app keyed by PTY id
func getPtyActiveSessionFiles(app *FastAPI) *PtySessionFiles {
	return appStateDefault(app, "pty_active_session_files", func() any { return NewPtySessionFiles() }).(*PtySessionFiles)
}

// Session file paths keyed by PTY id, safe across handler goroutines
type PtySessionFiles struct {
	mu   sync.Mutex
	keys []string
	vals map[string]string
}

// Empty PTY session file map
func NewPtySessionFiles() *PtySessionFiles { return &PtySessionFiles{vals: map[string]string{}} }

// Path stored under the key and whether it is present
func (p *PtySessionFiles) Get(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.vals[key]
	return v, ok
}

// Stores the path under the key keeping first insertion order
func (p *PtySessionFiles) Set(key, path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.vals[key]; !ok {
		p.keys = append(p.keys, key)
	}
	p.vals[key] = path
}

// Removes the key and returns its path and whether it was present
func (p *PtySessionFiles) Pop(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.vals[key]
	if !ok {
		return "", false
	}
	delete(p.vals, key)
	for i, k := range p.keys {
		if k == key {
			p.keys = append(p.keys[:i:i], p.keys[i+1:]...)
			break
		}
	}
	return v, true
}

// Paths in insertion order
func (p *PtySessionFiles) Values() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.keys))
	for _, k := range p.keys {
		out = append(out, p.vals[k])
	}
	return out
}

// Number of entries
func (p *PtySessionFiles) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys)
}

// Replaces the session token with the SSH launch token when one is given
func applySSHSessionToken(token string) {
	if token != "" {
		setSessionToken(token)
	}
}

// SSH owner nonce the launch recorded, empty when none
func SSHOwnerNonce() string {
	webStateMu.RLock()
	defer webStateMu.RUnlock()
	return sshOwnerNonce
}

// Install directory of the running binary, the runtime an SSH launch marks
func runtimePurelib() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := pypath.Resolve(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(resolved), nil
}

// Records the SSH owner nonce and marks the runtime install with a nonce file plus its directory identity
func applySSHOwnerNonce(nonce string) {
	purelib, err := runtimePurelib()
	applySSHOwnerNonceAt(nonce, purelib, err)
}

// Records the nonce against the given runtime install directory, nothing marked when it is unknown
func applySSHOwnerNonceAt(nonce, purelib string, purelibErr error) {
	webStateMu.Lock()
	defer webStateMu.Unlock()
	sshOwnerNonce = nonce
	sshRuntimePurelib = nil
	sshRuntimeMarker = ""
	if nonce == "" || purelibErr != nil {
		return
	}
	marker := filepath.Join(purelib, ".icebath-ssh-runtime-"+nonce)
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0o666); err == nil {
		sshRuntimeMarker = marker
	}
	if dev, ino, err := statIdentity(purelib); err == nil {
		sshRuntimePurelib = &sshRuntimeIdentity{path: purelib, dev: dev, ino: ino}
	}
}

// Device and inode of a path following links, the identity os.stat reports
func statIdentity(path string) (uint64, uint64, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return 0, 0, err
	}
	return pyfs.LstatIdentity(resolved)
}

// Whether the runtime install the SSH launch marked is still the one on disk
func sshRuntimeIntact() bool {
	webStateMu.RLock()
	marker, ident := sshRuntimeMarker, sshRuntimePurelib
	webStateMu.RUnlock()
	if marker != "" {
		info, err := os.Stat(marker)
		return err == nil && info.Mode().IsRegular()
	}
	if ident == nil {
		return true
	}
	dev, ino, err := statIdentity(ident.path)
	if err != nil {
		return false
	}
	return dev == ident.dev && ino == ident.ino
}

// Session token header the request carries, the icebath name first and the hermes name plugins still send second
func presentedSessionHeader(h Headers) string {
	if v := h.Get(sessionHeaderName); v != "" {
		return v
	}
	return h.Get(legacySessionHeaderName)
}

// Whether the request carries the dashboard session token in its header or a legacy bearer authorization
func hasValidSessionToken(req *Request) bool {
	token := SessionToken()
	if h := presentedSessionHeader(req.Headers()); h != "" && subtle.ConstantTimeCompare([]byte(h), []byte(token)) == 1 {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(req.Headers().Get("authorization")), []byte("Bearer "+token)) == 1
}

// Authorizes a sensitive endpoint, a 401 HTTPException when neither a principal, the gate session nor the token admits it
func requireToken(req *Request) error {
	if v, ok := req.State().Lookup("native_http_principal"); ok && v != nil {
		return nil
	}
	if v, _ := req.State().Lookup("hosted_http_authenticated"); v == true {
		return nil
	}
	var ok bool
	if app := req.App(); app != nil && app.State.Bool("auth_required") {
		v, set := req.State().Lookup("session")
		ok = set && v != nil
	} else {
		ok = hasValidSessionToken(req)
	}
	if !ok {
		return NewHTTPException(401, "Unauthorized")
	}
	return nil
}

// Process cached stable install identity
func getInstallID(ctx context.Context) (string, error) {
	return installidentity.GetInstallID(ctx, installIDCache)
}

// Whether a voice list failure signature is new and should be logged, a nil signature clearing the latch
func voiceListErrorLoggedOnce(signature *string) bool {
	webStateMu.Lock()
	defer webStateMu.Unlock()
	if signature == nil {
		voiceListLastError = nil
		return false
	}
	if voiceListLastError != nil && *signature == *voiceListLastError {
		return false
	}
	s := *signature
	voiceListLastError = &s
	return true
}

// Dashboard process health, a rolling window of unhandled errors and the periodic self test result
type DashboardHealth struct {
	mu                 sync.Mutex
	windowSeconds      float64
	errorTimes         []float64
	lastErrorType      *string
	lastErrorPath      *string
	lastErrorAt        *float64
	selftestStatus     string
	selftestHTTPStatus *int
	selftestAt         *float64
}

// Health tracker over a window in seconds with no errors and an unknown self test
func NewDashboardHealth(windowSeconds float64) *DashboardHealth {
	return &DashboardHealth{windowSeconds: windowSeconds, selftestStatus: "unknown"}
}

func unixSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Records one unhandled error or 5xx with its type and path, the oldest dropped past 256
func (h *DashboardHealth) RecordError(excType, path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := unixSeconds()
	h.errorTimes = append(h.errorTimes, now)
	if len(h.errorTimes) > dashboardHealthErrorTimesLimit {
		h.errorTimes = h.errorTimes[len(h.errorTimes)-dashboardHealthErrorTimesLimit:]
	}
	h.lastErrorType, h.lastErrorPath, h.lastErrorAt = &excType, &path, &now
}

// Records a self test pass or failure with the HTTP status it saw
func (h *DashboardHealth) RecordSelftest(passed bool, httpStatus *int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.selftestStatus = "failing"
	if passed {
		h.selftestStatus = "ok"
	}
	h.selftestHTTPStatus = httpStatus
	now := unixSeconds()
	h.selftestAt = &now
}

// Errors inside the window, dropping the older ones
func (h *DashboardHealth) RecentErrorCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recentErrorCountLocked()
}

func (h *DashboardHealth) recentErrorCountLocked() int {
	cutoff := unixSeconds() - h.windowSeconds
	for len(h.errorTimes) > 0 && h.errorTimes[0] < cutoff {
		h.errorTimes = h.errorTimes[1:]
	}
	return len(h.errorTimes)
}

// Public component payload of status, error count, last error time and self test result
func (h *DashboardHealth) Snapshot() *pyjson.Object {
	h.mu.Lock()
	defer h.mu.Unlock()
	errors := h.recentErrorCountLocked()
	status := "ok"
	if errors > 0 || h.selftestStatus == "failing" {
		status = "degraded"
	}
	var lastErrorAt any
	if h.lastErrorAt != nil {
		lastErrorAt = *h.lastErrorAt
	}
	return pyjson.ObjectOf("status", status, "recent_unhandled_errors", errors, "last_error_at", lastErrorAt,
		"selftest", h.selftestStatus)
}

// Self test result, its HTTP status and when it ran
func (h *DashboardHealth) Selftest() (string, *int, *float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.selftestStatus, h.selftestHTTPStatus, h.selftestAt
}

// Type and path of the last recorded error, internal only
func (h *DashboardHealth) LastError() (*string, *string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastErrorType, h.lastErrorPath
}
