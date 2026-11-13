package connections

import (
	"math"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	keepaliveReconnectDelay    = 2 * time.Second
	keepaliveMaxReconnectDelay = 30 * time.Second
)

type keepaliveEntry struct {
	failures       int
	reconnectTimer *time.Timer
	scope          string
	socket         *websocket.Conn
	baseURL        string
	token          string
}

// SSHIsolatedKeepaliveRegistry holds one desktop owned socket per published SSH backend so idle exit sees the owner
type SSHIsolatedKeepaliveRegistry struct {
	mu      sync.Mutex
	entries map[string]*keepaliveEntry
	log     func(string)
}

// NewSSHIsolatedKeepaliveRegistry is an empty registry logging through log
func NewSSHIsolatedKeepaliveRegistry(log func(string)) *SSHIsolatedKeepaliveRegistry {
	return &SSHIsolatedKeepaliveRegistry{entries: map[string]*keepaliveEntry{}, log: log}
}

func (r *SSHIsolatedKeepaliveRegistry) clearTimer(e *keepaliveEntry) {
	if e.reconnectTimer != nil {
		e.reconnectTimer.Stop()
		e.reconnectTimer = nil
	}
}

func (r *SSHIsolatedKeepaliveRegistry) closeSocket(e *keepaliveEntry) {
	socket := e.socket
	e.socket = nil
	if socket != nil {
		_ = socket.Close()
	}
}

// Redials after a capped exponential backoff while the entry is still registered, caller holds the lock
func (r *SSHIsolatedKeepaliveRegistry) scheduleReconnect(e *keepaliveEntry) {
	if r.entries[e.scope] != e || e.reconnectTimer != nil {
		return
	}
	delay := time.Duration(math.Min(float64(keepaliveReconnectDelay)*math.Pow(2, float64(e.failures)), float64(keepaliveMaxReconnectDelay)))
	e.failures++
	e.reconnectTimer = time.AfterFunc(delay, func() {
		r.mu.Lock()
		e.reconnectTimer = nil
		r.mu.Unlock()
		r.connect(e)
	})
}

// Opens the keepalive socket of an entry, a drop or failure scheduling the reconnect
func (r *SSHIsolatedKeepaliveRegistry) connect(e *keepaliveEntry) {
	r.mu.Lock()
	if r.entries[e.scope] != e {
		r.mu.Unlock()
		return
	}
	r.clearTimer(e)
	r.closeSocket(e)
	url, err := BuildGatewayWsURL(e.baseURL, e.token)
	if err != nil {
		if r.log != nil {
			r.log("[ssh] keep-alive WS failed to open for " + e.scope + ": " + err.Error())
		}
		r.scheduleReconnect(e)
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	go func() {
		dialer := websocket.Dialer{}
		socket, _, err := dialer.Dial(url, nil)
		r.mu.Lock()
		if err != nil {
			if r.entries[e.scope] == e && e.socket == nil {
				r.scheduleReconnect(e)
			}
			r.mu.Unlock()
			return
		}
		if r.entries[e.scope] != e || e.reconnectTimer != nil {
			r.mu.Unlock()
			_ = socket.Close()
			return
		}
		e.socket = socket
		e.failures = 0
		r.mu.Unlock()
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				break
			}
		}
		r.mu.Lock()
		if r.entries[e.scope] == e && e.socket == socket {
			e.socket = nil
			r.scheduleReconnect(e)
		}
		r.mu.Unlock()
	}()
}

// Arms the keepalive of a published scope, the empty scope included, replacing any earlier one
func (r *SSHIsolatedKeepaliveRegistry) Start(scope, baseURL, token string) {
	if baseURL == "" || token == "" {
		return
	}
	r.Stop(scope)
	e := &keepaliveEntry{scope: scope, baseURL: baseURL, token: token}
	r.mu.Lock()
	r.entries[scope] = e
	r.mu.Unlock()
	r.connect(e)
}

// Disarms a scope and closes its socket
func (r *SSHIsolatedKeepaliveRegistry) Stop(scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[scope]
	if e == nil {
		return
	}
	delete(r.entries, scope)
	r.clearTimer(e)
	r.closeSocket(e)
}

// Disarms every scope
func (r *SSHIsolatedKeepaliveRegistry) StopAll() {
	r.mu.Lock()
	scopes := make([]string, 0, len(r.entries))
	for scope := range r.entries {
		scopes = append(scopes, scope)
	}
	r.mu.Unlock()
	for _, scope := range scopes {
		r.Stop(scope)
	}
}
