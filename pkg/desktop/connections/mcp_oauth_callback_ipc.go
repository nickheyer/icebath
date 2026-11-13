package connections

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	mcpOAuthDefaultWaitMs   = 5 * 60 * 1000
	mcpOAuthMaxPending      = 8
	mcpOAuthMaxWaitMs       = 15 * 60 * 1000
	mcpOAuthMinWaitMs       = 1000
	mcpOAuthCallbackDoneDoc = `<!doctype html><meta charset="utf-8"><title>Authorization received</title>` +
		`<body style="font:15px system-ui;margin:3rem;text-align:center">` +
		`<h2>&#10003; Authorization received</h2>` +
		`<p>You can close this window and return to Icebath.</p>` +
		`<script>setTimeout(()=>window.close(),800)</script>`
)

var mcpOAuthCallbackHit = regexp.MustCompile(`[?&](code|error)=`)

// McpOAuthCallbackResult is the code, state, issuer or error a loopback OAuth redirect carried
type McpOAuthCallbackResult struct {
	Code  *string `json:"code"`
	Error *string `json:"error"`
	Iss   *string `json:"iss"`
	State *string `json:"state"`
}

// McpOAuthListener is the id and loopback redirect URI of a bound callback listener
type McpOAuthListener struct {
	ID          string `json:"id"`
	RedirectURI string `json:"redirectUri"`
}

type mcpOAuthPending struct {
	result  *McpOAuthCallbackResult
	server  *http.Server
	settled bool
	waiters []chan McpOAuthCallbackResult
}

// One shot loopback listeners of MCP OAuth against a remote backend, keyed by id
type mcpOAuthListeners struct {
	mu      sync.Mutex
	pending map[string]*mcpOAuthPending
	nextID  int
}

func mcpOAuthFailure(message string) McpOAuthCallbackResult {
	return McpOAuthCallbackResult{Error: &message}
}

func (l *mcpOAuthListeners) settleLocked(id string, result McpOAuthCallbackResult) {
	entry := l.pending[id]
	if entry == nil || entry.settled {
		return
	}
	entry.settled = true
	entry.result = &result
	go entry.server.Shutdown(context.Background())
	waiters := entry.waiters
	entry.waiters = nil
	for _, w := range waiters {
		w <- result
	}
}

func (l *mcpOAuthListeners) settle(id string, result McpOAuthCallbackResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.settleLocked(id, result)
}

// Text Node gives a failed listen on the loopback address
func listenErrorText(err error) string {
	code := "UNKNOWN"
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if c, ok := listenErrnoCodes[errno]; ok {
			code = c
		}
	}
	desc := map[string]string{
		"EADDRINUSE": "address already in use", "EADDRNOTAVAIL": "address not available", "EACCES": "permission denied",
		"EMFILE": "too many open files", "ENFILE": "file table overflow", "ENOBUFS": "no buffer space available",
		"EPERM": "operation not permitted", "EAFNOSUPPORT": "address family not supported", "EINVAL": "invalid argument",
		"UNKNOWN": "unknown error",
	}[code]
	return "listen " + code + ": " + desc + " 127.0.0.1"
}

// McpOauthListen binds a one shot loopback listener whose redirect carries the OAuth code back to the renderer
func (s *Service) McpOauthListen() (McpOAuthListener, error) {
	l := &s.local.mcpOAuth
	l.mu.Lock()
	if l.pending == nil {
		l.pending = map[string]*mcpOAuthPending{}
	}
	if len(l.pending) >= mcpOAuthMaxPending {
		l.mu.Unlock()
		return McpOAuthListener{}, errors.New("Too many MCP OAuth listeners are already pending")
	}
	l.nextID++
	id := strconv.Itoa(l.nextID)
	l.mu.Unlock()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(mcpOAuthCallbackDoneDoc)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mcpOAuthCallbackDoneDoc))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		raw := r.RequestURI
		if raw == "" {
			raw = "/"
		}
		if !mcpOAuthCallbackHit.MatchString(raw) {
			return
		}
		var result McpOAuthCallbackResult
		parsed, err := parseURLRef(raw, "http://127.0.0.1")
		if err != nil {
			result = mcpOAuthFailure("unparseable callback URL")
		} else {
			params := parseSearchParams(parsed.Query())
			field := func(name string) *string {
				if v, ok := params.get(name); ok {
					return &v
				}
				return nil
			}
			result = McpOAuthCallbackResult{Code: field("code"), Error: field("error"), Iss: field("iss"), State: field("state")}
		}
		l.settle(id, result)
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return McpOAuthListener{}, errors.New(listenErrorText(err))
	}
	go server.Serve(listener)
	port := listener.Addr().(*net.TCPAddr).Port
	l.mu.Lock()
	l.pending[id] = &mcpOAuthPending{server: server}
	l.mu.Unlock()
	return McpOAuthListener{ID: id, RedirectURI: "http://127.0.0.1:" + strconv.Itoa(port) + "/callback"}, nil
}

// McpOauthWait answers the redirect of a listener once it arrives or the clamped timeout passes
func (s *Service) McpOauthWait(id any, timeoutMs any) McpOAuthCallbackResult {
	l := &s.local.mcpOAuth
	key := ""
	if jsTruthy(id) {
		key = jsString(id)
	}
	l.mu.Lock()
	entry := l.pending[key]
	if entry == nil {
		l.mu.Unlock()
		return mcpOAuthFailure("listener not found")
	}
	if entry.result != nil {
		result := *entry.result
		delete(l.pending, jsString(id))
		l.mu.Unlock()
		return result
	}
	timeout := ecmaToNumber(timeoutMs)
	if math.IsNaN(timeout) || timeout == 0 {
		timeout = mcpOAuthDefaultWaitMs
	}
	timeout = math.Min(math.Max(timeout, mcpOAuthMinWaitMs), mcpOAuthMaxWaitMs)
	done := make(chan McpOAuthCallbackResult, 1)
	entry.waiters = append(entry.waiters, done)
	l.mu.Unlock()
	timer := time.AfterFunc(time.Duration(timeout*float64(time.Millisecond)), func() {
		l.settle(jsString(id), mcpOAuthFailure("timeout waiting for OAuth callback"))
	})
	result := <-done
	timer.Stop()
	l.mu.Lock()
	delete(l.pending, jsString(id))
	l.mu.Unlock()
	return result
}

// McpOauthCancel tears a listener down without waiting, settling its waiters as cancelled
func (s *Service) McpOauthCancel(id any) bool {
	l := &s.local.mcpOAuth
	key := ""
	if jsTruthy(id) {
		key = jsString(id)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.pending[key]
	if entry == nil {
		return true
	}
	if !entry.settled {
		l.settleLocked(key, mcpOAuthFailure("cancelled"))
	}
	delete(l.pending, key)
	return true
}
