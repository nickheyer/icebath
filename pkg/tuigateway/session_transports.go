package tuigateway

import (
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Leaf lock of session transport membership, held with the sessions lock at most above it
var sessionTransportMu sync.Mutex

// Login check and host owner subject of the gateway package, which installs both when it loads
var (
	isAuthenticatedIdentity func(identity any) bool
	hostOwnerSubject        func() string
)

// Installs the gateway functions that tell a signed in identity and name the host owner
func BindIdentity(isAuthenticated func(identity any) bool, ownerSubject func() string) {
	isAuthenticatedIdentity, hostOwnerSubject = isAuthenticated, ownerSubject
}

// Whether a transport is a real attached client, not the stdio sink, the parked sink or a closed peer
func transportIsLivePeer(transport Transport) bool {
	if transport == nil || transport == detachedWSTransport || transport == Transport(stdioTransport) {
		return false
	}
	switch transport.(type) {
	case *dropTransport, *StdioTransport:
		return false
	}
	return !transportIsDead(transport)
}

// Whether a live transport is the session transport or a peer of its fanout
func sessionTransportContains(session *Session, transport Transport) bool {
	if session.empty() || transport == nil || transportIsDead(transport) {
		return false
	}
	existing, _ := session.Get("transport").(Transport)
	if existing == transport {
		return true
	}
	fanout, ok := existing.(*FanoutTransport)
	return ok && fanout.Contains(transport)
}

// Live client transports attached to a session
func sessionLiveTransports(session *Session) []Transport {
	existing, _ := session.Get("transport").(Transport)
	peers := []Transport{existing}
	if fanout, ok := existing.(*FanoutTransport); ok {
		peers = fanout.Transports()
	}
	live := []Transport{}
	for _, peer := range peers {
		if transportIsLivePeer(peer) {
			live = append(live, peer)
		}
	}
	return live
}

// Whether a session keeps a live client other than the excluded transport
func sessionHasLiveTransport(session *Session, excluding Transport) bool {
	for _, peer := range sessionLiveTransports(session) {
		if peer != excluding {
			return true
		}
	}
	return false
}

// Owner a row created over a transport belongs to, the host owner subject for a signed in login, else nil
func transportAuthUserID(transport Transport) *string {
	var identity any
	if identified, ok := transport.(interface{ AuthIdentity() map[string]any }); ok {
		if fields := identified.AuthIdentity(); fields != nil {
			identity = fields
		}
	}
	if isAuthenticatedIdentity(identity) {
		subject := hostOwnerSubject()
		return &subject
	}
	return nil
}

// Login a session was created under, its stamped auth_user_id slot or else the login of its transport
func sessionAuthUserID(session *Session) *string {
	if value, ok := session.Lookup("auth_user_id"); ok {
		if userID, isString := value.(string); isString {
			return &userID
		}
		return nil
	}
	existing, _ := session.Get("transport").(Transport)
	return transportAuthUserID(existing)
}

// Whether a server request can be answered, false only when every attached WebSocket client is an old build
func sessionClientAnswersRequests(sid string) bool {
	clients := 0
	for _, peer := range sessionLiveTransports(SessionRecord(sid)) {
		if client, ok := peer.(*WSTransport); ok {
			clients++
			if AnswersRequests(client) {
				return true
			}
		}
	}
	return clients == 0
}

// Live WebSocket clients of a session that advertised answering server requests
func sessionAnsweringClients(sid string) []Transport {
	clients := []Transport{}
	for _, peer := range sessionLiveTransports(SessionRecord(sid)) {
		if client, ok := peer.(*WSTransport); ok && AnswersRequests(client) {
			clients = append(clients, peer)
		}
	}
	return clients
}

// Logs a second login sharing a session, the agent keeping the user id of its creator
func warnForeignLogin(session *Session, transport Transport) {
	attaching := transportAuthUserID(transport)
	if attaching == nil {
		return
	}
	creator := sessionAuthUserID(session)
	if creator != nil && *creator == *attaching {
		return
	}
	shown := "(none)"
	if creator != nil && *creator != "" {
		shown = *creator
	}
	serverLog().Warn(fmt.Sprintf("Session %s keeps the user id %s it was created with; a client logged in as %s attached",
		pyrepr.Str(session.Get("session_key")), shown, *attaching))
}

// Adds live peers to a session, a fanout argument flattened into its peers, true when one was attached
func attachSessionTransport(session *Session, transport Transport) bool {
	if session.empty() || transport == nil {
		return false
	}
	sessionTransportMu.Lock()
	defer sessionTransportMu.Unlock()
	return attachSessionTransportLocked(session, transport)
}

// Membership change of one attach with the transport lock held
func attachSessionTransportLocked(session *Session, transport Transport) bool {
	if session.empty() || transport == nil {
		return false
	}
	if fanout, ok := transport.(*FanoutTransport); ok {
		attached := false
		for _, peer := range fanout.Transports() {
			if attachSessionTransportLocked(session, peer) {
				attached = true
			}
		}
		return attached
	}
	existing, _ := session.Get("transport").(Transport)
	existingFanout, isFanout := existing.(*FanoutTransport)
	if transportIsDead(transport) {
		if isFanout {
			existingFanout.Detach(transport)
		}
		return false
	}
	if !transportIsLivePeer(transport) {
		if sessionHasLiveTransport(session, nil) {
			return false
		}
		session.Set("transport", transport)
		return true
	}
	if existing == transport {
		return true
	}
	if isFanout {
		if !existingFanout.Contains(transport) {
			warnForeignLogin(session, transport)
		}
		existingFanout.Attach(transport)
		return existingFanout.Contains(transport)
	}
	warnForeignLogin(session, transport)
	if transportIsLivePeer(existing) {
		session.Set("transport", NewFanoutTransport(existing, transport))
	} else {
		session.Set("transport", transport)
	}
	return true
}

// Removes a transport from a session and its viewers, true when another live client keeps the session from parking
func detachSessionTransport(session *Session, transport Transport) bool {
	if session.empty() {
		return false
	}
	sessionTransportMu.Lock()
	defer sessionTransportMu.Unlock()
	viewers, _ := session.Get("viewers").(map[Transport]float64)
	delete(viewers, transport)
	if fanout, ok := session.Get("transport").(*FanoutTransport); ok {
		fanout.Detach(transport)
		for viewer := range viewers {
			if !fanout.Contains(viewer) || transportIsDead(viewer) {
				delete(viewers, viewer)
			}
		}
	}
	return sessionHasLiveTransport(session, transport)
}

// Whether a transport is recorded as a viewer of a session
func sessionViewerContains(session *Session, transport Transport) bool {
	sessionTransportMu.Lock()
	defer sessionTransportMu.Unlock()
	viewers, _ := session.Get("viewers").(map[Transport]float64)
	_, ok := viewers[transport]
	return ok
}

// Detaches a transport from every session that holds it, returning the sessions left with no live client
func detachTransportFromSessions(transport Transport) []sessionEntry {
	attached := []sessionEntry{}
	for _, entry := range sessionEntries() {
		existing, _ := entry.session.Get("transport").(Transport)
		fanout, isFanout := existing.(*FanoutTransport)
		if existing == transport || isFanout && fanout.Contains(transport) || sessionViewerContains(entry.session, transport) {
			attached = append(attached, entry)
		}
	}
	clientless := []sessionEntry{}
	for _, entry := range attached {
		if !detachSessionTransport(entry.session, transport) {
			clientless = append(clientless, entry)
		}
	}
	return clientless
}
