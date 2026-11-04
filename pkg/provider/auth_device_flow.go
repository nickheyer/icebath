package provider

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Console browsers the system opener would launch inside the terminal, hijacking the TTY
var consoleBrowserNames = map[string]bool{
	"w3m": true, "lynx": true, "links": true, "links2": true, "elinks": true, "www-browser": true, "browsh": true,
}

// Browser only remote IDEs and cloud shells that set neither SSH_CLIENT nor SSH_TTY
var remoteIDEEnvVars = []string{"CLOUD_SHELL", "CODESPACES", "CODESPACE_NAME", "GITPOD_WORKSPACE_ID", "REPL_ID", "STACKBLITZ"}

// Process environment reader the browser heuristics consult, swappable so tests control the shell
var oauthGetenv = os.Getenv

// Executable lookup the browser resolution uses, swappable so tests control the PATH
var oauthLookPath = exec.LookPath

// Browser opener every interactive flow goes through, swappable so tests never launch one
var openBrowserFn = openBrowserURL

// Sleep the pollers and retries use, swappable so tests run without waiting
var oauthSleep = time.Sleep

// Line prompt the yes or no questions read, swappable so the commands slice binds the terminal
var oauthPromptFn = func(prompt string) (string, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// True where a loopback OAuth redirect cannot reach the user's browser
func isRemoteSession() bool {
	if oauthGetenv("SSH_CLIENT") != "" || oauthGetenv("SSH_TTY") != "" {
		return true
	}
	for _, v := range remoteIDEEnvVars {
		if oauthGetenv(v) != "" {
			return true
		}
	}
	return false
}

func namesConsoleBrowser(value string) bool {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return false
	}
	return consoleBrowserNames[strings.ToLower(filepath.Base(fields[0]))]
}

func browserOnPath(names ...string) string {
	for _, name := range names {
		if _, err := oauthLookPath(name); err == nil {
			return name
		}
	}
	return ""
}

// Name of the browser the platform opener resolves in the stdlib registration order, empty when none
func resolvedBrowserName() string {
	if browserEnv := oauthGetenv("BROWSER"); browserEnv != "" {
		choices := strings.Split(browserEnv, string(os.PathListSeparator))
		for _, cmdline := range choices {
			if cmdline != "" {
				return cmdline
			}
		}
	}
	switch runtime.GOOS {
	case "darwin":
		return "MacOSX"
	case "windows":
		return "windows-default"
	}
	if oauthGetenv("DISPLAY") != "" || oauthGetenv("WAYLAND_DISPLAY") != "" {
		if found := browserOnPath("xdg-open", "gio"); found != "" {
			return found
		}
		desktop := strings.Split(oauthGetenv("XDG_CURRENT_DESKTOP"), ":")
		if (containsString(desktop, "GNOME") || oauthGetenv("GNOME_DESKTOP_SESSION_ID") != "") && browserOnPath("gvfs-open") != "" {
			return "gvfs-open"
		}
		if (containsString(desktop, "KDE") || oauthGetenv("KDE_FULL_SESSION") != "") && browserOnPath("kfmclient") != "" {
			return "kfmclient"
		}
		if found := browserOnPath("x-www-browser", "firefox", "iceweasel", "seamonkey", "mozilla-firefox", "mozilla", "kfm", "konqueror",
			"epiphany", "google-chrome", "chrome", "chromium", "chromium-browser", "opera", "microsoft-edge"); found != "" {
			return found
		}
	}
	if oauthGetenv("TERM") != "" {
		return browserOnPath("www-browser", "links", "elinks", "lynx", "w3m")
	}
	return ""
}

// True only when a graphical browser is likely to open rather than a console one taking the terminal
func canOpenGraphicalBrowser() bool {
	browserEnv := oauthGetenv("BROWSER")
	if browserEnv != "" && namesConsoleBrowser(browserEnv) {
		return false
	}
	if runtime.GOOS == "linux" {
		hasDisplay := oauthGetenv("DISPLAY") != "" || oauthGetenv("WAYLAND_DISPLAY") != ""
		if !hasDisplay && browserEnv == "" {
			return false
		}
	}
	candidate := resolvedBrowserName()
	if candidate == "" {
		return false
	}
	return !namesConsoleBrowser(candidate)
}

// user at hostname for the SSH tunnel hint, placeholders keeping the syntax valid
func sshUserAtHost() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "<this-host>"
	}
	user := oauthGetenv("USER")
	if user == "" {
		user = oauthGetenv("LOGNAME")
	}
	if user == "" {
		user = "<user>"
	}
	return user + "@" + hostname
}

func pkceCodeVerifier(length int) string {
	raw := make([]byte, length)
	fillRandom(raw)
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	if len(verifier) > 128 {
		verifier = verifier[:128]
	}
	return verifier
}

func pkceCodeChallenge(codeVerifier string) string {
	sum := sha256.Sum256([]byte(codeVerifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// loopbackCallbackResult is the query the RFC 8252 loopback redirect delivered
type loopbackCallbackResult struct {
	mu     sync.Mutex
	values map[string]any
}

func newLoopbackCallbackResult() *loopbackCallbackResult {
	return &loopbackCallbackResult{values: map[string]any{"code": nil, "state": nil, "error": nil, "error_description": nil}}
}

func (r *loopbackCallbackResult) snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{}
	for k, v := range r.values {
		out[k] = v
	}
	return out
}

func (r *loopbackCallbackResult) landed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return anyString(r.values["code"]) != "" || anyString(r.values["error"]) != ""
}

// Handler for a loopback redirect accepting only a GET on the expected path, the result filled in
func newLoopbackCallbackHandler(expectedPath, displayName string) (http.Handler, *loopbackCallbackResult) {
	result := newLoopbackCallbackResult()
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != expectedPath {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Not found."))
			return
		}
		params := req.URL.Query()
		result.mu.Lock()
		for key := range result.values {
			if vals, ok := params[key]; ok && len(vals) > 0 {
				result.values[key] = vals[0]
			} else {
				result.values[key] = nil
			}
		}
		failed := anyString(result.values["error"]) != ""
		result.mu.Unlock()
		outcome := "received"
		if failed {
			outcome = "failed"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "<html><body><h1>%s authorization %s.</h1>You can close this tab.</body></html>", displayName, outcome)
	})
	return handler, result
}

// loopbackServer is a bound loopback listener and the HTTP server that will drain it
type loopbackServer struct {
	listener net.Listener
	server   *http.Server
}

// Port the listener owns
func (s *loopbackServer) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// Binds the loopback listener up front so the redirect URI names a port already owned
func bindLoopbackCallbackServer(host string, port int, handler http.Handler, errFn func(message, code string) *AuthError, bindFailedCode string) (*loopbackServer, error) {
	lc := net.ListenConfig{Control: reuseAddrControl}
	listener, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, errFn(fmt.Sprintf("Could not bind callback server on %s:%d: %v", host, port, err), bindFailedCode)
	}
	return &loopbackServer{listener: listener, server: &http.Server{Handler: handler}}, nil
}

// Serves until the redirect lands in result or the deadline passes, always closing the listener
func serveLoopbackCallback(server *loopbackServer, result *loopbackCallbackResult, timeout time.Duration, errFn func(message, code string) *AuthError, timeoutCode string) (map[string]any, error) {
	done := make(chan struct{})
	go func() {
		_ = server.server.Serve(server.listener)
		close(done)
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.server.Shutdown(ctx)
		_ = server.listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if result.landed() {
			return result.snapshot(), nil
		}
		oauthSleep(100 * time.Millisecond)
	}
	return nil, errFn("Authorization timed out waiting for the local callback.", timeoutCode)
}

var loopbackCallbackHosts = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true}

// Prints the SSH tunnel hint when a loopback redirect flow runs on a remote host
func printLoopbackSSHHint(out io.Writer, redirectURI, docsURL string) {
	if !isRemoteSession() {
		return
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return
	}
	host, port := parsed.Hostname(), parsed.Port()
	if !loopbackCallbackHosts[host] || port == "" {
		return
	}
	divider := strings.Repeat("-", 60)
	fmt.Fprintf(out, "\n%s\nRemote session detected — SSH tunnel required\n%s\n"+
		"Icebath is waiting for the OAuth callback on %s\n"+
		"but your browser is on a different machine. Run this command\n"+
		"in a NEW terminal on your local machine BEFORE opening the URL:\n\n"+
		"  ssh -N -L %s:127.0.0.1:%s %s\n\n"+
		"Then open the authorize URL above in your local browser.\n", divider, divider, redirectURI, port, port, sshUserAtHost())
	if docsURL != "" {
		fmt.Fprintf(out, "Provider docs:      %s\n", docsURL)
	}
	fmt.Fprintf(out, "SSH/jump-box guide: %s\n%s\n\n", OAuthOverSSHDocsURL, divider)
}

// oauthVerify is the TLS verification an interactive OAuth client runs with
type oauthVerify struct {
	Insecure bool
	CABundle string
	TLS      *tls.Config
}

// Platform aware default verification, the resolved bundle pinned on macOS
func defaultOAuthVerify() oauthVerify {
	bundle := DefaultCABundlePath()
	if bundle == "" {
		return oauthVerify{TLS: PlatformTLSConfig()}
	}
	if runtime.GOOS == "darwin" {
		return oauthVerify{TLS: sharedTLSConfig(bundle, false)}
	}
	return oauthVerify{TLS: PlatformTLSConfig()}
}

// Verification from explicit flags, then the stored tls block, then the CA bundle env names
func resolveOAuthVerify(env PoolEnv, insecure *bool, caBundle string, authState map[string]any) oauthVerify {
	tlsState, _ := authState["tls"].(map[string]any)
	effectiveInsecure := false
	if insecure != nil {
		effectiveInsecure = *insecure
	} else if tlsState != nil {
		effectiveInsecure = truthyAny(tlsState["insecure"])
	}
	effectiveCA := caBundle
	if effectiveCA == "" && tlsState != nil {
		effectiveCA = anyString(tlsState["ca_bundle"])
	}
	for _, name := range []string{"ICEBATH_CA_BUNDLE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"} {
		if effectiveCA != "" {
			break
		}
		effectiveCA = GetEnvPreferDotenv(env, name)
	}
	if effectiveInsecure {
		return oauthVerify{Insecure: true, TLS: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	}
	if effectiveCA != "" {
		info, err := os.Stat(effectiveCA)
		if err != nil || info.IsDir() {
			env.log().Warn(fmt.Sprintf("CA bundle path does not exist: %s — falling back to default certificates", effectiveCA))
			return defaultOAuthVerify()
		}
		return oauthVerify{CABundle: effectiveCA, TLS: sharedTLSConfig(effectiveCA, false)}
	}
	return defaultOAuthVerify()
}

var proxyEnvKeysAll = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"}

var noProxyEnvKeys = []string{"NO_PROXY", "no_proxy"}

var (
	bracketedIPv6Re = regexp.MustCompile(`^\[([0-9A-Fa-f:.]+)\](?:[/:].*)?$`)
	cidrIPv6Re      = regexp.MustCompile(`^([0-9A-Fa-f:.]+)/\d+$`)
)

// The socks5 form of a socks proxy URL, other values trimmed and otherwise untouched
func normalizeProxyURL(proxyURL string) string {
	candidate := strings.TrimSpace(proxyURL)
	if strings.HasPrefix(strings.ToLower(candidate), "socks://") {
		return "socks5://" + candidate[len("socks://"):]
	}
	return candidate
}

// The bare IPv6 literal a bracketed or CIDR IPv6 NO_PROXY entry should become, empty otherwise
func bareIPv6Literal(entry string) string {
	candidate := strings.TrimSpace(entry)
	m := bracketedIPv6Re.FindStringSubmatch(candidate)
	if m == nil {
		m = cidrIPv6Re.FindStringSubmatch(candidate)
	}
	if m == nil {
		return ""
	}
	ip := net.ParseIP(m[1])
	if ip == nil || ip.To4() != nil {
		return ""
	}
	return m[1]
}

// NO_PROXY with bracketed and CIDR IPv6 entries rewritten to bare literals, byte stable otherwise
func sanitizeNoProxyEntries(noProxyValue string) string {
	entries := noProxySplitRe.Split(strings.TrimSpace(noProxyValue), -1)
	cleaned := entries[:0]
	for _, part := range entries {
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) == 0 || containsString(cleaned, "*") {
		return noProxyValue
	}
	rewritten := make([]string, 0, len(cleaned))
	changed := false
	for _, entry := range cleaned {
		bare := bareIPv6Literal(entry)
		if bare == "" {
			rewritten = append(rewritten, entry)
			continue
		}
		changed = true
		if !containsString(rewritten, bare) {
			rewritten = append(rewritten, bare)
		}
	}
	if !changed {
		return noProxyValue
	}
	return strings.Join(rewritten, ",")
}

// Rewrites the proxy env vars to canonical forms in place before a client reads them
func oauthNormalizeProxyEnvVars() {
	for _, key := range proxyEnvKeysAll {
		value := os.Getenv(key)
		if normalized := normalizeProxyURL(value); normalized != "" && normalized != value {
			_ = os.Setenv(key, normalized)
		}
	}
	for _, key := range noProxyEnvKeys {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		if sanitized := sanitizeNoProxyEntries(value); sanitized != value {
			_ = os.Setenv(key, sanitized)
		}
	}
}

// Transport adding default headers to every request it carries
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		if clone.Header.Get(k) == "" {
			clone.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(clone)
}

// Transport with the resolved verification, proxy settings read from the normalized environment
func oauthTransport(verify oauthVerify) *http.Transport {
	oauthNormalizeProxyEnvVars()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.TLSClientConfig = verify.TLS
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, FallbackDelay: 300 * time.Millisecond}).DialContext
	return transport
}

// JSON accepting client for the interactive OAuth endpoints
func oauthHTTPClient(env PoolEnv, timeout time.Duration, verify oauthVerify) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{Timeout: timeout, Transport: headerTransport{base: oauthTransport(verify), headers: map[string]string{"Accept": "application/json"}}}
}

// oauthHTTPResponse is a fully read response of one OAuth endpoint call
type oauthHTTPResponse struct {
	Status int
	Header http.Header
	Body   []byte
	URL    string
}

// Decoded JSON body, failing on malformed content
func (r *oauthHTTPResponse) JSON() (any, error) {
	var payload any
	if err := json.Unmarshal(r.Body, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// Decoded JSON object body, an empty object for non object JSON and an error for malformed content
func (r *oauthHTTPResponse) JSONObject() (map[string]any, error) {
	payload, err := r.JSON()
	if err != nil {
		return nil, err
	}
	obj, _ := payload.(map[string]any)
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}

// HTTPStatusError is a response whose status is an error, the raise for status of the stdlib
type HTTPStatusError struct {
	Status int
	URL    string
	Body   string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d for url '%s'", e.Status, e.URL)
}

// Error for a 4xx or 5xx response, nil otherwise
func (r *oauthHTTPResponse) RaiseForStatus() error {
	if r.Status >= 400 {
		return &HTTPStatusError{Status: r.Status, URL: r.URL, Body: string(r.Body)}
	}
	return nil
}

// Runs one request fully reading the body, bodies above the cap refused through the capError factory
func oauthDo(client *http.Client, req *http.Request, maxBody int64, capError func(host string) error) (*oauthHTTPResponse, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var reader io.Reader = resp.Body
	if maxBody > 0 {
		reader = io.LimitReader(resp.Body, maxBody+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if maxBody > 0 && int64(len(body)) > maxBody {
		return nil, capError(req.URL.Host)
	}
	return &oauthHTTPResponse{Status: resp.StatusCode, Header: resp.Header, Body: body, URL: req.URL.String()}, nil
}

// POSTs a form body
func oauthPostForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, headers map[string]string) (*oauthHTTPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return oauthDo(client, req, 0, nil)
}

// Requests a device code from the Portal, every RFC 8628 field required
func requestDeviceCode(ctx context.Context, client *http.Client, portalBaseURL, clientID, scope string) (map[string]any, error) {
	form := url.Values{"client_id": {clientID}}
	if scope != "" {
		form.Set("scope", scope)
	}
	resp, err := oauthPostForm(ctx, client, portalBaseURL+"/api/oauth/device/code", form, nil)
	if err != nil {
		return nil, err
	}
	if err := resp.RaiseForStatus(); err != nil {
		return nil, err
	}
	data, err := resp.JSONObject()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, field := range []string{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"} {
		if _, ok := data[field]; !ok {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Device code response missing fields: %s", strings.Join(missing, ", "))
	}
	return data, nil
}

// Actionable timeout text pointing at the Portal login page and the retry command
func nousDeviceAuthTimeoutMessage(portalBaseURL string) string {
	portal := strings.TrimRight(portalBaseURL, "/")
	if portal == "" {
		portal = DefaultNousPortalURL
	}
	return "Timed out waiting for device authorization.\n" +
		"  Portal sign-in is required before the device code can be approved.\n" +
		"  If the browser showed a CAPTCHA / 'You did not pass CAPTCHA' error,\n" +
		"  finish signing in at the Portal in a normal browser tab, then retry:\n" +
		"    icebath portal\n" +
		"  Portal login: " + portal + "/login"
}

// Prints the shared To continue block and optionally opens the browser
func printDeviceCodeInstructions(out io.Writer, verificationURL, userCode string, openBrowser bool, failureDash string, swallowOpenErrors bool) error {
	if failureDash == "" {
		failureDash = "--"
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "To continue:")
	fmt.Fprintf(out, "  1. Open: %s\n", verificationURL)
	fmt.Fprintf(out, "  2. If prompted, enter code: %s\n", userCode)
	if !openBrowser {
		return nil
	}
	err := openBrowserFn(verificationURL)
	if err != nil && !swallowOpenErrors {
		return err
	}
	if err == nil {
		fmt.Fprintln(out, "  (Opened browser for verification)")
	} else {
		fmt.Fprintf(out, "  Could not open browser automatically %s use the URL above.\n", failureDash)
	}
	return nil
}

// DeviceFlowCancelledError is the caller's cancelled hook firing while the token poll waited
type DeviceFlowCancelledError struct{}

func (*DeviceFlowCancelledError) Error() string { return "device flow cancelled" }

// Sleeps in ticks of at most one second under a cancel hook so a cancel ends the wait promptly
func cancellableSleep(d time.Duration, cancelled func() bool) error {
	if cancelled == nil {
		oauthSleep(d)
		return nil
	}
	wake := time.Now().Add(d)
	for !cancelled() {
		remaining := time.Until(wake)
		if remaining <= 0 {
			return nil
		}
		if remaining > time.Second {
			remaining = time.Second
		}
		oauthSleep(remaining)
	}
	return &DeviceFlowCancelledError{}
}

// deviceTokenPoll are the per provider pieces of the RFC 8628 polling loop
type deviceTokenPoll struct {
	post            func() (*oauthHTTPResponse, error)
	expiresIn       int
	pollInterval    int
	validateSuccess func(payload map[string]any) error
	onNonJSONError  func(resp *oauthHTTPResponse) error
	onError         func(resp *oauthHTTPResponse, payload map[string]any) error
	onTimeout       func(cause error) error
	cancelled       func() bool
}

// Polls the token endpoint, pending and slow down pacing the loop and edge mitigation backing off
func pollDeviceTokenGeneric(p deviceTokenPoll) (map[string]any, error) {
	expires := p.expiresIn
	if expires < 1 {
		expires = 1
	}
	deadline := time.Now().Add(time.Duration(expires) * time.Second)
	currentInterval := float64(p.pollInterval)
	edgeBackoff := 0.0
	unavailable := 0
	for time.Now().Before(deadline) {
		if p.cancelled != nil && p.cancelled() {
			return nil, &DeviceFlowCancelledError{}
		}
		resp, err := p.post()
		if err != nil {
			return nil, err
		}
		if resp.Status == 200 {
			payload, err := resp.JSONObject()
			if err != nil {
				return nil, err
			}
			if err := p.validateSuccess(payload); err != nil {
				return nil, err
			}
			return payload, nil
		}
		errorPayload, jsonErr := resp.JSONObject()
		if jsonErr != nil {
			status := resp.Status
			if status == 408 || status == 429 || status >= 500 || (status == 403 && resp.Header.Get("x-vercel-mitigated") != "") {
				unavailable = status
				if retryAfter, ok := RetryAfterFromHeaders(resp.Header); ok {
					edgeBackoff = minFloat(maxFloat(currentInterval, retryAfter.Seconds()), 60)
				} else {
					edgeBackoff = minFloat(maxFloat(edgeBackoff*2, maxFloat(currentInterval*2, 5)), 60)
				}
				wait := minFloat(edgeBackoff, time.Until(deadline).Seconds())
				if wait < 0 {
					wait = 0
				}
				if err := cancellableSleep(time.Duration(wait*float64(time.Second)), p.cancelled); err != nil {
					return nil, err
				}
				continue
			}
			if err := resp.RaiseForStatus(); err != nil {
				return nil, err
			}
			return nil, p.onNonJSONError(resp)
		}
		edgeBackoff, unavailable = 0, 0
		errorCode := anyString(errorPayload["error"])
		if errorCode == "authorization_pending" {
			wait := minFloat(currentInterval, maxFloat(0, time.Until(deadline).Seconds()))
			if err := cancellableSleep(time.Duration(wait*float64(time.Second)), p.cancelled); err != nil {
				return nil, err
			}
			continue
		}
		if errorCode == "slow_down" {
			currentInterval = minFloat(currentInterval+1, 30)
			if err := cancellableSleep(time.Duration(currentInterval*float64(time.Second)), p.cancelled); err != nil {
				return nil, err
			}
			continue
		}
		return nil, p.onError(resp, errorPayload)
	}
	var cause error
	if unavailable != 0 {
		cause = fmt.Errorf("token endpoint answered HTTP %d", unavailable)
	}
	return nil, p.onTimeout(cause)
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// Polls the Nous token endpoint until the user approves or the code expires
func pollForToken(ctx context.Context, client *http.Client, portalBaseURL, clientID, deviceCode string, expiresIn, pollInterval int, cancelled func() bool) (map[string]any, error) {
	interval := pollInterval
	if cap := int(DeviceAuthPollIntervalCap / time.Second); interval > cap {
		interval = cap
	}
	if interval < 1 {
		interval = 1
	}
	return pollDeviceTokenGeneric(deviceTokenPoll{
		post: func() (*oauthHTTPResponse, error) {
			return oauthPostForm(ctx, client, portalBaseURL+"/api/oauth/token", url.Values{
				"grant_type": {DeviceCodeGrantType}, "client_id": {clientID}, "device_code": {deviceCode}}, nil)
		},
		expiresIn:    expiresIn,
		pollInterval: interval,
		validateSuccess: func(payload map[string]any) error {
			if _, ok := payload["access_token"]; !ok {
				return errors.New("Token response did not include access_token")
			}
			return nil
		},
		onError: func(_ *oauthHTTPResponse, payload map[string]any) error {
			description := anyString(payload["error_description"])
			if description == "" {
				description = "Unknown authentication error"
			}
			return DeviceFlowError(anyString(payload["error"]), description, "")
		},
		onNonJSONError: func(*oauthHTTPResponse) error {
			return errors.New("Token endpoint returned a non-JSON error response")
		},
		onTimeout: func(cause error) error {
			return &DeviceCodeExpiredError{Message: nousDeviceAuthTimeoutMessage(portalBaseURL), Cause: cause}
		},
		cancelled: cancelled,
	})
}

// Asks a yes or no question, EOF or an interrupt counting as the default
func promptYesNo(prompt, def string) bool {
	answer, err := oauthPromptFn(prompt)
	if err != nil {
		answer = def
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if def == "y" {
		return answer == "" || answer == "y" || answer == "yes"
	}
	return answer == "y" || answer == "yes"
}

// Prints the login success block, the auth state line naming the store when asked
func printLoginSuccess(out io.Writer, h, providerID, configPath string, showAuthState bool) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Login successful!")
	if showAuthState {
		fmt.Fprintf(out, "  Auth state: %s/auth.json\n", home.Display(h))
	}
	fmt.Fprintf(out, "  Config updated: %s (model.provider=%s)\n", configPath, providerID)
}

// Offers to reuse still valid stored OAuth credentials, true when the user accepted them
func offerExistingOAuthCredentials(env PoolEnv, out io.Writer, providerID string, resolve func() (map[string]any, error),
	isExpiring func(string, time.Duration) bool, displayName, defaultBaseURL, expiredNotice string) (bool, error) {
	existing, err := resolve()
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return false, nil
		}
		return false, err
	}
	apiKey, _ := existing["api_key"].(string)
	if apiKey != "" && !isExpiring(apiKey, 60*time.Second) {
		fmt.Fprintf(out, "Existing %s credentials found in Icebath auth store.\n", displayName)
		if promptYesNo("Use existing credentials? [Y/n]: ", "y") {
			baseURL := anyString(existing["base_url"])
			if _, has := existing["base_url"]; !has {
				baseURL = defaultBaseURL
			}
			configPath, err := updateConfigForProvider(env, providerID, baseURL, "", false)
			if err != nil {
				return false, err
			}
			printLoginSuccess(out, envHome(env), providerID, configPath, false)
			return true, nil
		}
	} else if expiredNotice != "" {
		fmt.Fprintln(out, expiredNotice)
	}
	return false, nil
}
