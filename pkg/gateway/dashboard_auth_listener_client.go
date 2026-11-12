package gateway

import "sync"

// Portal client a listener signs in with in place of the configured one
type ListenerClient struct {
	ClientID  string
	PortalURL string
}

var (
	listenerClientMu  sync.Mutex
	listenerClientCur *ListenerClient
	listenerClientSwp func() func()
)

// Client and portal URL while a listener signs in with its own client, nil otherwise
func ListenerClientCurrent() *ListenerClient {
	listenerClientMu.Lock()
	defer listenerClientMu.Unlock()
	return listenerClientCur
}

// Records the sign in plugin's provider swap, returning its undo when called
func ListenerClientOffer(swap func() func()) {
	listenerClientMu.Lock()
	defer listenerClientMu.Unlock()
	listenerClientSwp = swap
}

// Signs in with the client until the undo runs, nil where the sign in plugin is absent
func ListenerClientUse(clientID, portalURL string) func() {
	listenerClientMu.Lock()
	swap := listenerClientSwp
	if swap == nil {
		listenerClientMu.Unlock()
		return nil
	}
	listenerClientCur = &ListenerClient{ClientID: clientID, PortalURL: portalURL}
	listenerClientMu.Unlock()
	var restore func()
	func() {
		defer func() {
			if r := recover(); r != nil {
				listenerClientMu.Lock()
				listenerClientCur = nil
				listenerClientMu.Unlock()
				panic(r)
			}
		}()
		restore = swap()
	}()
	return func() {
		listenerClientMu.Lock()
		listenerClientCur = nil
		listenerClientMu.Unlock()
		restore()
	}
}

// Signs Webapp browsers in with the Portal client Web Access registered until the undo runs, nil when none is registered
func UseOwnSignIn(webapp *WebappMount) func() {
	clientID, portal, registered := webappSignInClient(webapp.Home)
	if !registered {
		return nil
	}
	return ListenerClientUse(clientID, portal)
}
