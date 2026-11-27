package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/httpx"
)

// Callback ports held from the moment a provider chose them until a sign in listens on them
var (
	reservedMu sync.Mutex
	reserved   = map[int]net.Listener{}
	// Client ID metadata document ports this process took, never given back
	cimdTaken []int
)

// Holds a loopback port, a free one for zero, for a sign in to listen on later
func Reserve(port int) (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return 0, err
	}
	port = listener.Addr().(*net.TCPAddr).Port
	reservedMu.Lock()
	defer reservedMu.Unlock()
	if old := reserved[port]; old != nil {
		old.Close()
	}
	reserved[port] = listener
	return port, nil
}

// Listener of a port held for a sign in, else one opened now
func listen(host string, port int) (net.Listener, error) {
	reservedMu.Lock()
	listener := reserved[port]
	delete(reserved, port)
	reservedMu.Unlock()
	if listener != nil && (host == "127.0.0.1" || host == "localhost") {
		return listener, nil
	}
	if listener != nil {
		listener.Close()
	}
	return net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// A client ID metadata document port: a free one of those it declares, else one this process took before
func cimdPort() (int, bool) {
	reservedMu.Lock()
	taken := append([]int(nil), cimdTaken...)
	reservedMu.Unlock()
	for _, port := range cimdPorts {
		if slices.Contains(taken, port) {
			continue
		}
		if _, err := Reserve(port); err == nil {
			reservedMu.Lock()
			cimdTaken = append(cimdTaken, port)
			reservedMu.Unlock()
			return port, true
		}
	}
	if len(taken) > 0 {
		return taken[0], true
	}
	return 0, false
}

// Words a pasted answer may be to give up the sign in
var skipWords = map[string]bool{"skip": true, "cancel": true, "s": true, "n": true, "no": true, "q": true, "quit": true}

// The sign in was given up on purpose
var ErrSkipped = errors.New("You skipped the sign-in.")

// One interactive sign in: the link the person opens, and the browser coming back to the loopback listener or pasted by hand
type Login struct {
	ready  chan struct{}
	back   chan url.Values
	ended  chan struct{}
	end    sync.Once
	mu     sync.Mutex
	link   string
	device struct{ link, code string }
	// Redirect URI the browser comes back to
	redirect string
	// Why it ended, nil while it runs and after it succeeded
	err error
}

func NewLogin() *Login {
	return &Login{ready: make(chan struct{}), back: make(chan url.Values, 1), ended: make(chan struct{})}
}

// Signer of a provider's flow: publishes the link, listens on a loopback redirect URI and waits for the browser or a paste
func (l *Login) Authorize(ctx context.Context, link, redirect string) (url.Values, error) {
	stop, err := l.receive(redirect)
	if err != nil {
		return nil, err
	}
	defer stop()
	l.mu.Lock()
	l.link, l.redirect = link, redirect
	l.mu.Unlock()
	l.publish()
	select {
	case query := <-l.back:
		return query, nil
	case <-l.ended:
		l.mu.Lock()
		defer l.mu.Unlock()
		return nil, cmp.Or(l.err, ErrSkipped)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Shows a device sign in: the link to open on any device and the code to enter there
func (l *Login) ShowDevice(link, code string) {
	l.mu.Lock()
	l.device.link, l.device.code = link, code
	l.mu.Unlock()
	l.publish()
}

func (l *Login) publish() {
	select {
	case <-l.ready:
	default:
		close(l.ready)
	}
}

// Link of the sign in once there is one: the authorization link, or the device link and its code
func (l *Login) Link(ctx context.Context) (link, code string, err error) {
	select {
	case <-l.ready:
	case <-l.ended:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.device.link != "" {
		return l.device.link, l.device.code, nil
	}
	if l.link == "" {
		return "", "", cmp.Or(l.err, errors.New("The sign-in ended before the server gave a link to open."))
	}
	return l.link, "", nil
}

// Redirect URI the browser comes back to, empty before the link
func (l *Login) Redirect() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.redirect
}

// Takes where the browser was sent, pasted: the whole address, its query, or a word that gives up the sign in
func (l *Login) Deliver(pasted string) error {
	pasted = strings.TrimSpace(pasted)
	if skipWords[strings.ToLower(pasted)] {
		l.Fail(ErrSkipped)
		return nil
	}
	query := pasted
	if _, after, found := strings.Cut(pasted, "?"); found {
		query = after
	}
	values, err := url.ParseQuery(strings.TrimPrefix(query, "?"))
	if err != nil || values.Get("code") == "" && values.Get("error") == "" {
		return errors.New("The address you pasted holds no code or error. Copy the whole address that the browser ended on.")
	}
	select {
	case l.back <- values:
		return nil
	default:
		return errors.New("This sign-in already got its answer.")
	}
}

// Ends the sign in with the error, the first one kept
func (l *Login) Fail(err error) {
	l.end.Do(func() {
		l.mu.Lock()
		l.err = err
		l.mu.Unlock()
		close(l.ended)
	})
}

// Closed once the sign in was given up or failed
func (l *Login) Ended() <-chan struct{} { return l.ended }

const callbackPage = `<html><head><meta charset="utf-8"><title>icebath</title></head><body>%s</body></html>`

// Starts the loopback listener of a redirect URI on this machine, a no op for any other redirect URI, which only a paste can answer
func (l *Login) receive(redirect string) (func(), error) {
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.Port() == "" || !httpx.Loopback(u.Hostname()) {
		return func() {}, nil
	}
	port, _ := strconv.Atoi(u.Port())
	listener, err := listen(u.Hostname(), port)
	if err != nil {
		return nil, fmt.Errorf("Could not wait for the browser to come back on %s: %w", u.Host, err)
	}
	taken := false
	var mu sync.Mutex
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		query := r.URL.Query()
		code, failure := query.Get("code"), query.Get("error")
		if code == "" && failure == "" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, callbackPage, "<h2>Nothing is here</h2>")
			return
		}
		mu.Lock()
		first := !taken
		taken = true
		mu.Unlock()
		switch {
		case !first:
			fmt.Fprintf(w, callbackPage, "<h2>You are already signed in</h2><p>You can close this tab and go back to icebath.</p>")
			return
		case code != "":
			fmt.Fprintf(w, callbackPage, "<h2>You are signed in</h2><p>You can close this tab and go back to icebath.</p>")
		default:
			fmt.Fprintf(w, callbackPage, "<h2>The sign-in failed</h2><p>The server said: "+html.EscapeString(failure)+"</p>")
		}
		select {
		case l.back <- query:
		default:
		}
	})}
	go server.Serve(listener)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		server.Shutdown(ctx)
		cancel()
	}, nil
}
