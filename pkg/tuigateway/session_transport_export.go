package tuigateway

import "slices"

// One live session with its id, the registry entry a transport detach returns
type SessionEntry struct {
	SID     string
	Session *Session
}

// Detaches a transport from every session holding it, returning the sessions left with no live client
func DetachTransportFromSessions(transport Transport) []SessionEntry {
	entries := detachTransportFromSessions(transport)
	out := make([]SessionEntry, len(entries))
	for i, entry := range entries {
		out[i] = SessionEntry{entry.sid, entry.session}
	}
	return out
}

// Whether a session still has a live client other than the excluded transport, under the transport lock
func SessionHasLiveTransportExcluding(session *Session, excluding Transport) bool {
	sessionTransportMu.Lock()
	defer sessionTransportMu.Unlock()
	return sessionHasLiveTransport(session, excluding)
}

// Drops one viewer registration of a session
func DropSessionViewer(session *Session, transport Transport) {
	sessionTransportMu.Lock()
	defer sessionTransportMu.Unlock()
	if viewers, ok := session.Get("viewers").(map[Transport]float64); ok {
		delete(viewers, transport)
	}
}

// Records a transport as a viewer of a session at the given wall clock time
func SetSessionViewer(session *Session, transport Transport, at float64) {
	sessionTransportMu.Lock()
	defer sessionTransportMu.Unlock()
	viewers, ok := session.Get("viewers").(map[Transport]float64)
	if !ok || viewers == nil {
		viewers = map[Transport]float64{}
		session.Set("viewers", viewers)
	}
	viewers[transport] = at
}

// Viewer transports of a session from the oldest registration to the most recent
func SessionViewersByAge(session *Session) []Transport {
	sessionTransportMu.Lock()
	defer sessionTransportMu.Unlock()
	viewers, _ := session.Get("viewers").(map[Transport]float64)
	out := make([]Transport, 0, len(viewers))
	for transport := range viewers {
		out = append(out, transport)
	}
	slices.SortStableFunc(out, func(a, b Transport) int {
		switch {
		case viewers[a] < viewers[b]:
			return -1
		case viewers[a] > viewers[b]:
			return 1
		}
		return 0
	})
	return out
}
