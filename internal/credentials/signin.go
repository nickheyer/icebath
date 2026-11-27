package credentials

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What a sign in that runs in the front end talks to the person through: the app's sign in page, or the terminal of a command
type Prompter struct {
	Ctx context.Context
	// What the sign in says as it goes
	Out io.Writer
	// Asks for a line of text, masked when secret; false once the person gave up
	Ask func(label string, secret bool) (string, bool)
	// Shows the address to open and the code to type there, the code empty when there is none
	Show func(link, code string)
	// Opens an address in a browser of this computer
	Open func(link string) error
	// Whether the sign in may open the browser by itself
	Browser bool
	// How long one step may wait, zero for the flow's own
	Timeout time.Duration
	// Flags of the sign in by name, such as browser for a Codex browser sign in or region for MiniMax's
	Options map[string]string
	// Settings of the provider the account is of, by their names below it, as the front end read them
	Settings map[string]string
}

// Flag of the sign in by name, trimmed
func (p *Prompter) Option(name string) string {
	return strings.TrimSpace(p.Options[name])
}

// Timeout of the sign in, else the flow's own
func (p *Prompter) Wait(fallback time.Duration) time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return fallback
}

// Says one line
func (p *Prompter) Say(format string, args ...any) {
	fmt.Fprintf(p.Out, format+"\n", args...)
}

// Fields an account keeps after a sign in, by their names below it, and who it signed in as
type Signed struct {
	Fields map[string]any
	// Such as an email, which names an account no name was asked for
	Identity string
}

// Way to sign an account of a kind in that runs in the front end, which the plugin's app and cli hooks offer
type SignIn struct {
	// Kind of provider it signs in, by the name of its spec
	Spec string
	// Shown as the way to connect, such as "ChatGPT account"
	Title string
	// Name auth add --type takes it by: oauth for a sign in, api-key for a key given some other way
	Type string
	Run  func(p *Prompter) (Signed, error)
}

// Ways of signing in by --type
const (
	OAuth  = "oauth"
	APIKey = "api-key"
)

// Variables that mark a session whose browser runs on another machine, so a sign in can't open one nor be reached by its redirect
var remote = []string{"SSH_CLIENT", "SSH_TTY", "CLOUD_SHELL", "CODESPACES", "CODESPACE_NAME", "GITPOD_WORKSPACE_ID", "REPL_ID", "STACKBLITZ"}

// Whether this runs where the person's browser is not
func Remote() bool {
	for _, name := range remote {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// Tells the person where to go and what code to type there, and opens the address when the sign in may
func (p *Prompter) Instructions(link, code string) {
	p.Say("")
	p.Say("To continue:")
	p.Say("  1. Open: %s", link)
	if code != "" {
		p.Say("  2. If prompted, enter code: %s", code)
	}
	if p.Show != nil {
		p.Show(link, code)
	}
	p.OpenBrowser(link, "  (Opened browser for verification)", "  Could not open browser automatically — use the URL above.")
}

// Opens the address in this computer's browser when the sign in may and one is here, saying how that went
func (p *Prompter) OpenBrowser(link, opened, failed string) {
	if !p.Browser || Remote() || p.Open == nil {
		return
	}
	if err := p.Open(link); err != nil {
		p.Say("%s", failed)
		return
	}
	p.Say("%s", opened)
}

// Local server a browser sign in sends its answer back to
type Callback struct {
	// Address the provider redirects the browser to
	Redirect string
	listener net.Listener
	server   *http.Server
	once     sync.Once
	answer   chan url.Values
}

// Listens on the loopback port for the redirect to the path, refusing when the port is taken with the code given. Port 0 takes any
func Listen(host string, port int, path, name, busy string) (*Callback, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, &AuthError{Code: busy, Message: fmt.Sprintf("Could not listen for the sign in's answer on %s:%d: %v", host, port, err), Err: err}
	}
	c := &Callback{listener: listener, answer: make(chan url.Values, 1)}
	c.Redirect = fmt.Sprintf("http://localhost:%d%s", listener.Addr().(*net.TCPAddr).Port, path)
	c.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			http.Error(w, "Not found.", http.StatusNotFound)
			return
		}
		query := r.URL.Query()
		outcome := "received"
		if query.Get("error") != "" {
			outcome = "failed"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><h1>%s authorization %s.</h1>You can close this tab.</body></html>", html.EscapeString(name), outcome)
		c.once.Do(func() { c.answer <- query })
	})}
	go c.server.Serve(listener)
	return c, nil
}

// Waits for the browser to come back, at most the timeout, and stops listening. The answer's code once its state is the one sent, for a
// sign-in that sent one
func (c *Callback) Wait(ctx context.Context, timeout time.Duration, state, provider string) (string, error) {
	defer c.Close()
	timer := time.NewTimer(max(timeout, 5*time.Second))
	defer timer.Stop()
	var query url.Values
	select {
	case <-ctx.Done():
		return "", errors.Join(ErrCancelled, ctx.Err())
	case <-timer.C:
		return "", Failed(provider, "callback_timeout", "Authorization timed out waiting for the local callback.", false, nil)
	case query = <-c.answer:
	}
	if failure := query.Get("error"); failure != "" {
		return "", Failed(provider, "authorization_denied", "Authorization failed: "+strs.Filled(query.Get("error_description"), failure), false, nil)
	}
	if state != "" && subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
		return "", Failed(provider, "state_mismatch", "Authorization callback state mismatch — the redirect did not come from this login. Aborting.", false, nil)
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		return "", Failed(provider, "no_code", "Authorization callback did not carry a code.", false, nil)
	}
	return code, nil
}

// Stops listening
func (c *Callback) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.server.Shutdown(ctx)
}

// Tells a person on a remote session how to tunnel the redirect to this machine before opening the address
func (p *Prompter) Tunnel(redirect string) {
	if !Remote() {
		return
	}
	parsed, err := url.Parse(redirect)
	if err != nil || parsed.Port() == "" {
		return
	}
	host, _ := os.Hostname()
	user := strs.Filled(os.Getenv("USER"), os.Getenv("LOGNAME"), "<user>")
	target := user + "@" + strs.Filled(host, "<this-host>")
	port := parsed.Port()
	divider := strings.Repeat("-", 60)
	p.Say("\n%s\nRemote session detected — SSH tunnel required\n%s", divider, divider)
	p.Say("Icebath is waiting for the OAuth callback on %s\nbut your browser is on a different machine. Run this command\n"+
		"in a NEW terminal on your local machine BEFORE opening the URL:\n\n  ssh -N -L %s:127.0.0.1:%s %s\n\nThen open the authorize URL above in your local browser.", redirect, port, port, target)
	p.Say("Through a jump box: ssh -N -L %s:127.0.0.1:%s -J <jump-user>@<jump-host> %s\n%s\n", port, port, target, divider)
}

// One RFC 8628 token poll: what to post, how long the code lives and how often to ask
type Poll struct {
	Provider string
	Post     func() (*Reply, error)
	Expires  time.Duration
	Interval time.Duration
	// Checks the answer that granted the sign in
	Granted func(payload map[string]any) error
}

// Polls for the token of a device code: pending and slow_down pace it, an edge that throttles is waited out, any other error ends it
func (p Poll) Run(ctx context.Context) (map[string]any, error) {
	deadline := time.Now().Add(max(p.Expires, time.Second))
	interval := max(p.Interval, time.Second)
	backoff, unavailable := time.Duration(0), 0
	for time.Now().Before(deadline) {
		reply, err := p.Post()
		if err != nil {
			if ctx.Err() != nil {
				return nil, errors.Join(ErrCancelled, ctx.Err())
			}
			return nil, err
		}
		// GitHub answers a pending code with 200 and the error in its body
		if granted, _ := reply.JSON(); reply.Status == http.StatusOK && (Text(granted["error"]) == "" || Text(granted["access_token"]) != "") {
			payload, err := reply.JSON()
			if err != nil {
				return nil, err
			}
			if p.Granted != nil {
				if err := p.Granted(payload); err != nil {
					return nil, err
				}
			}
			return payload, nil
		}
		payload, err := reply.JSON()
		if err != nil {
			status := reply.Status
			if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 || status == http.StatusForbidden && reply.Header.Get("x-vercel-mitigated") != "" {
				unavailable = status
				if wait, ok := reply.RetryAfter(); ok {
					backoff = min(max(interval, wait), time.Minute)
				} else {
					backoff = min(max(backoff*2, interval*2, 5*time.Second), time.Minute)
				}
				if err := Sleep(ctx, min(backoff, time.Until(deadline))); err != nil {
					return nil, err
				}
				continue
			}
			return nil, Failed(p.Provider, "device_token_failed", fmt.Sprintf("Device code token polling returned HTTP %d with a body that is not JSON.", status), false, nil)
		}
		backoff, unavailable = 0, 0
		switch code := Text(payload["error"]); code {
		case "authorization_pending":
			if err := Sleep(ctx, min(interval, time.Until(deadline))); err != nil {
				return nil, err
			}
			continue
		case "slow_down":
			interval = min(interval+5*time.Second, 30*time.Second)
			if asked, ok := config.Whole(payload["interval"]); ok && asked > 0 {
				interval = time.Duration(asked) * time.Second
			}
			if err := Sleep(ctx, interval); err != nil {
				return nil, err
			}
			continue
		case "expired_token":
			return nil, Failed(p.Provider, Expired, "The device code expired before the sign in was approved.", false, nil)
		case "access_denied":
			return nil, Failed(p.Provider, "access_denied", "Authorization was denied.", false, nil)
		default:
			return nil, Failed(p.Provider, "device_token_failed", "Device code token polling failed: "+strs.Filled(Text(payload["error_description"]), code, reply.Text(300)), false, nil)
		}
	}
	message := "Timed out waiting for device authorization."
	if unavailable != 0 {
		message += fmt.Sprintf(" The token endpoint answered HTTP %d.", unavailable)
	}
	return nil, Failed(p.Provider, Expired, message, false, nil)
}
