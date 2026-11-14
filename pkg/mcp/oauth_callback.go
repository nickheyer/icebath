package mcp

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/internal/pywebbrowser"
)

// OAuthNonInteractiveError reports an OAuth step that needs a person no session can reach
type OAuthNonInteractiveError struct{ Msg string }

func (e *OAuthNonInteractiveError) Error() string { return e.Msg }

type oauthInteractiveDisabledKey struct{}

type oauthInteractiveForcedKey struct{}

// Context in which OAuth never prompts on stdin nor starts a browser flow
func SuppressInteractiveOAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauthInteractiveDisabledKey{}, true)
}

// Context treated as interactive without a console, for GUI driven logins whose person is present
func ForceInteractiveOAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauthInteractiveForcedKey{}, true)
}

// Reports whether a person can type on stdin, the console API deciding on Windows
var stdinIsConsole = func() bool { return xterm.IsTerminal(os.Stdin.Fd()) }

// Reports whether the flow may expect a person, suppression winning over forcing
func isInteractive(ctx context.Context) bool {
	if disabled, _ := ctx.Value(oauthInteractiveDisabledKey{}).(bool); disabled {
		return false
	}
	if forced, _ := ctx.Value(oauthInteractiveForcedKey{}).(bool); forced {
		return true
	}
	return stdinIsConsole()
}

// Error naming the interactive login remedy unless the context is interactive
func raiseIfNonInteractive(ctx context.Context, lead string) error {
	if isInteractive(ctx) {
		return nil
	}
	return &OAuthNonInteractiveError{lead + " Run `icebath mcp login <server>` interactively to (re)authorize, then restart or reload the gateway."}
}

// Paste tokens that leave the login without authorizing
var oauthSkipTokens = map[string]bool{"skip": true, "cancel": true, "s": true, "n": true, "no": true, "q": true, "quit": true}

const oauthUserSkippedSentinel = "__icebath_user_skipped__"

// Stream the OAuth prompts and URL announcement are written to, swapped by tests
var oauthStderr io.Writer = os.Stderr

// Reads one stdin line without buffering past it, swapped by tests
var oauthStdinReadLine = func() (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 1 {
			b.WriteByte(buf[0])
			if buf[0] == '\n' {
				return b.String(), nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return b.String(), nil
			}
			return b.String(), err
		}
	}
}

// Reports whether opening a browser is likely to work
func canOpenBrowser() bool {
	if os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// Opens a URL with the platform browser registry, true when a browser launched, swapped by tests
var openBrowser = webbrowserOpen

// Launches the first browser the BROWSER variable or the platform registry names, any failure answering false
func webbrowserOpen(url string) bool {
	opened, err := pywebbrowser.Open(url, 0)
	return err == nil && opened
}

// Callback port reservations held from selection until the waiter adopts them
var (
	reservedMu        sync.Mutex
	reservedListeners = map[int]net.Listener{}
	reservedOrder     []int
)

const maxReservedSockets = 8

// Binds 127.0.0.1 on a port (0 for ephemeral) and parks it, false when a pinned port is taken
func bindReserved(port int) (int, bool, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		if port != 0 {
			return 0, false, nil
		}
		return 0, false, err
	}
	bound := ln.Addr().(*net.TCPAddr).Port
	reservedMu.Lock()
	defer reservedMu.Unlock()
	for len(reservedListeners) >= maxReservedSockets {
		stale := -1
		for _, p := range reservedOrder {
			if !isCIMDPort(p) {
				stale = p
				break
			}
		}
		if stale < 0 {
			break
		}
		if old, ok := reservedListeners[stale]; ok {
			old.Close()
		}
		dropReserved(stale)
	}
	if old, ok := reservedListeners[bound]; ok {
		old.Close()
		dropReserved(bound)
	}
	reservedListeners[bound] = ln
	reservedOrder = append(reservedOrder, bound)
	return bound, true, nil
}

// Forgets a reservation, the caller holding reservedMu
func dropReserved(port int) {
	delete(reservedListeners, port)
	for i, p := range reservedOrder {
		if p == port {
			reservedOrder = append(reservedOrder[:i], reservedOrder[i+1:]...)
			break
		}
	}
}

// Takes the parked listener of a port out of the reservations
func takeReserved(port int) net.Listener {
	reservedMu.Lock()
	defer reservedMu.Unlock()
	ln, ok := reservedListeners[port]
	if !ok {
		return nil
	}
	dropReserved(port)
	return ln
}

// Reports whether a port is parked
func isReserved(port int) bool {
	reservedMu.Lock()
	defer reservedMu.Unlock()
	_, ok := reservedListeners[port]
	return ok
}

// Picks an ephemeral callback port and keeps it bound
func reserveCallbackPort() (int, error) {
	port, _, err := bindReserved(0)
	return port, err
}

// Cached registration as a decoded object, nil when absent or not an object
func cachedClientInfo(storage *TokenStorage) *jsonObject {
	if storage == nil {
		return nil
	}
	obj, _ := readOAuthJSON(storage.ClientInfoPath()).(*jsonObject)
	return obj
}

// Https proxy URI and loopback callback port of the cached registration, skipping malformed entries
func cachedRedirect(storage *TokenStorage) (string, int) {
	info := cachedClientInfo(storage)
	if info == nil {
		return "", 0
	}
	uris, _ := info.get("redirect_uris").([]any)
	uri, port := "", 0
	for _, raw := range uris {
		text := pyStr(raw)
		parsed, err := pyURLParse(text)
		if err != nil {
			continue
		}
		hostname := netlocHostname(parsed.Netloc)
		p, hasPort, perr := netlocPort(parsed.Netloc)
		if perr != nil {
			continue
		}
		if uri == "" && parsed.Scheme == "https" && parsed.Netloc != "" {
			uri = text
		}
		loopback := parsed.Scheme == "http" && parsed.Path == "/callback" && (hostname == "127.0.0.1" || hostname == "localhost")
		if port == 0 && loopback && hasPort {
			port = p
		}
	}
	return uri, port
}

// AuthorizationCodeResult carries the redirect parameters, empty strings standing for absent ones
type AuthorizationCodeResult struct {
	Code, State, Iss string
}

// Code, state, error and issuer of a redirect query, empty when absent
func parseRedirectQuery(query string) (code, state, errText, iss string) {
	vals := map[string]string{}
	for _, kv := range pyParseQSL(query, false) {
		if _, seen := vals[kv[0]]; !seen {
			vals[kv[0]] = kv[1]
		}
	}
	return vals["code"], vals["state"], vals["error"], vals["iss"]
}

// callbackResult is the first terminal redirect of one flow, shared by the listener and the paste reader
type callbackResult struct {
	mu                        sync.Mutex
	authCode, state, err, iss string
	taken                     bool
	done                      chan struct{}
}

// Fresh result for one flow
func newCallbackResult() *callbackResult { return &callbackResult{done: make(chan struct{})} }

// Records the redirect when no terminal result exists yet, false when one already won
func (r *callbackResult) take(code, state, errText, iss string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.taken {
		return false
	}
	r.authCode, r.state, r.err, r.iss, r.taken = code, state, errText, iss, true
	close(r.done)
	return true
}

// Reports whether a terminal result exists
func (r *callbackResult) isTaken() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.taken
}

// Copy of the recorded values
func (r *callbackResult) snapshot() (code, state, errText, iss string, taken bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authCode, r.state, r.err, r.iss, r.taken
}

const pyHTTPErrorPage = `<!DOCTYPE HTML>
<html lang="en">
    <head>
        <meta charset="utf-8">
        <title>Error response</title>
    </head>
    <body>
        <h1>Error response</h1>
        <p>Error code: %d</p>
        <p>Message: %s.</p>
        <p>Error code explanation: %d - %s.</p>
    </body>
</html>
`

// Handler answering the redirect, ignoring queryless follow ups and keeping the first terminal result
func makeCallbackHandler() (http.Handler, *callbackResult) {
	result := newCallbackResult()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpLogger().Debug(fmt.Sprintf("OAuth callback: %s %s", r.Method, r.URL.Path))
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "text/html;charset=utf-8")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusNotImplemented)
			if r.Method != http.MethodHead {
				msg := html.EscapeString(fmt.Sprintf("Unsupported method (%s)", pyReprStr(r.Method)))
				fmt.Fprintf(w, pyHTTPErrorPage, 501, msg, 501, "Server does not support this operation")
			}
			return
		}
		code, state, errText, iss := parseRedirectQuery(r.URL.RawQuery)
		status := http.StatusOK
		var body string
		switch {
		case code == "" && errText == "":
			status, body = http.StatusNotFound, "<h2>Not Found</h2>"
		case !result.take(code, state, errText, iss):
			body = "<h2>Authorization already received</h2><p>You can close this tab and return to Icebath.</p>"
		case code != "":
			body = "<h2>Authorization Successful</h2><p>You can close this tab and return to Icebath.</p>"
		default:
			body = fmt.Sprintf("<h2>Authorization Failed</h2><p>Error: %s</p>", pyHTMLEscape(errText))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		io.WriteString(w, "<html><body>"+body+"</body></html>")
	})
	return h, result
}

// Escapes like Python html.escape with quotes
func pyHTMLEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(s)
}

// Reads one pasted redirect, query or skip word from stdin into the shared result
func pasteCallbackReader(result *callbackResult) {
	line, err := oauthStdinReadLine()
	if err != nil {
		return
	}
	line = strings.TrimSpace(line)
	if line == "" || result.isTaken() {
		return
	}
	if oauthSkipTokens[strings.ToLower(line)] {
		if result.take("", "", oauthUserSkippedSentinel, "") {
			fmt.Fprintln(oauthStderr, "  OAuth skipped. Run `icebath mcp login <server>` later to authenticate, "+
				"or set ``enabled: false`` on that server in config.yaml to disable persistently.")
		}
		return
	}
	query := line
	if _, after, found := strings.Cut(line, "?"); found {
		query = after
	}
	code, state, errText, iss := parseRedirectQuery(strings.TrimPrefix(query, "?"))
	if code == "" && errText == "" {
		fmt.Fprintln(oauthStderr, "  Pasted input did not contain ``code=`` or ``error=`` — ignoring.")
		return
	}
	if !result.take(code, state, errText, iss) {
		return
	}
	if code != "" {
		fmt.Fprintln(oauthStderr, "  Got authorization code from paste — completing flow.")
	}
}

const sshHintProxy = "  Remote session detected. After you authorize, the provider redirects to\n" +
	"    %s\n" +
	"  which forwards to the callback listener on this machine — no SSH tunnel needed.\n"

const sshHintLoopback = "  Remote session detected. After you authorize, the provider redirects to\n" +
	"    http://%[1]s:%[2]d/callback\n" +
	"  which only the listener on THIS machine can receive. Two options:\n" +
	"\n" +
	"    1. Easiest — when your browser shows a connection error after\n" +
	"       authorizing, copy the full URL from the address bar and paste\n" +
	"       it at the prompt below. The pasted ``code=...&state=...`` is\n" +
	"       enough to complete the flow.\n" +
	"\n" +
	"    2. Or forward the port first in a separate terminal:\n" +
	"         ssh -N -L %[2]d:127.0.0.1:%[2]d <user>@<this-host>\n" +
	"       then open the URL above and let it redirect normally.\n" +
	"\n" +
	"  See: https://hermes-agent.nousresearch.com/docs/guides/oauth-over-ssh\n"

// Prints the authorization URL with any remote session hint and opens a browser when one can open
func announceAuthorizationURL(authorizationURL string, port int, redirectURI, redirectHost string) {
	fmt.Fprintf(oauthStderr, "\n  MCP OAuth: authorization required.\n  Open this URL in your browser:\n\n    %s\n\n", authorizationURL)
	if os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" {
		if redirectURI != "" {
			fmt.Fprintln(oauthStderr, fmt.Sprintf(sshHintProxy, redirectURI))
		} else if port != 0 {
			host := redirectHost
			if host == "" {
				host = "127.0.0.1"
			}
			fmt.Fprintln(oauthStderr, fmt.Sprintf(sshHintLoopback, host, port))
		}
	}
	var note string
	if !canOpenBrowser() {
		note = "Headless environment detected — open the URL manually."
	} else if openBrowser(authorizationURL) {
		note = "Browser opened automatically."
	} else {
		note = "Could not open browser — please open the URL manually."
	}
	fmt.Fprintf(oauthStderr, "  (%s)\n\n", note)
}

// Redirect handler bound to one flow's port, publishing to a dashboard flow or announcing the URL
func makeRedirectHandler(port int, redirectURI, redirectHost string) func(ctx context.Context, authorizationURL string) error {
	return func(ctx context.Context, authorizationURL string) error {
		if flow := dashboardOAuthFlowFrom(ctx); flow != nil {
			return flow.PublishAuthorizationURL(ctx, authorizationURL)
		}
		if err := raiseIfNonInteractive(ctx, "MCP OAuth requires browser authorization but no interactive session is available (non-interactive/background context)."); err != nil {
			return err
		}
		announceAuthorizationURL(authorizationURL, port, redirectURI, redirectHost)
		return nil
	}
}

// callbackServer is the loopback listener serving one flow's redirect
type callbackServer struct {
	srv *http.Server
	ln  net.Listener
	wg  sync.WaitGroup
}

// Serves the callback on a port, adopting its parked reservation, already in use naming the remedy
func startCallbackServer(port int, handler http.Handler) (*callbackServer, error) {
	ln := takeReserved(port)
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return nil, &OAuthNonInteractiveError{fmt.Sprintf("OAuth callback port %d is already in use (%v). Close any other in-progress login, "+
				"or set a free `oauth.redirect_port` in the server config, then retry.", port, err)}
		}
	}
	cs := &callbackServer{srv: &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}, ln: ln}
	cs.wg.Add(1)
	go func() {
		defer cs.wg.Done()
		_ = cs.srv.Serve(ln)
	}()
	return cs, nil
}

// Stops serving and frees the port before returning
func (cs *callbackServer) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if cs.srv.Shutdown(ctx) != nil {
		cs.srv.Close()
	}
	cs.ln.Close()
	cs.wg.Wait()
}

// Turns a filled or empty result into the callback value or the matching error
func callbackOutcome(result *callbackResult, cimdURL string) (*AuthorizationCodeResult, error) {
	code, state, errText, iss, taken := result.snapshot()
	if errText == oauthUserSkippedSentinel {
		return nil, &OAuthNonInteractiveError{"user_skipped"}
	}
	if errText != "" {
		return nil, fmt.Errorf("OAuth authorization failed: %s", errText)
	}
	if !taken || code == "" {
		hint := ""
		if cimdURL != "" {
			hint = " If the browser showed an invalid-client error instead of an approval prompt, the authorization " +
				fmt.Sprintf("server rejected Icebath's Client ID Metadata Document (%s); set ``cimd: false`` under that ", cimdURL) +
				"server's ``oauth:`` block in config.yaml to authorize via dynamic client registration instead."
		}
		return nil, &OAuthNonInteractiveError{"OAuth callback timed out — no authorization code received. Ensure you completed the browser authorization flow." + hint}
	}
	return &AuthorizationCodeResult{Code: code, State: state, Iss: iss}, nil
}

// Callback waiter bound to one flow's port, the dashboard answering when no loopback port is pinned
func makeCallbackWaiter(port int, cimdURL string, timeout time.Duration) func(ctx context.Context) (*AuthorizationCodeResult, error) {
	return func(ctx context.Context) (*AuthorizationCodeResult, error) {
		flow := dashboardOAuthFlowFrom(ctx)
		if flow != nil && port == 0 {
			code, state, iss, err := flow.WaitForCallback(ctx, 300*time.Second)
			if err != nil {
				return nil, err
			}
			return &AuthorizationCodeResult{Code: code, State: state, Iss: iss}, nil
		}
		if err := raiseIfNonInteractive(ctx, "OAuth callback requires an interactive session but none is available (non-interactive/background "+
			"context); skipping browser authorization without binding a callback listener."); err != nil {
			return nil, err
		}
		handler, result := makeCallbackHandler()
		server, err := startCallbackServer(port, handler)
		if err != nil {
			return nil, err
		}
		defer server.shutdown()
		if isInteractive(ctx) && flow == nil {
			fmt.Fprintln(oauthStderr, "\n  Or paste the redirect URL here (or the ``?code=...&state=...`` portion) and press Enter. "+
				"Type ``skip`` + Enter to continue without this server:")
			go pasteCallbackReader(result)
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-result.done:
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return callbackOutcome(result, cimdURL)
	}
}
