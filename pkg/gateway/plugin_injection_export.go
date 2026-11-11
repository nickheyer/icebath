package gateway

import "sort"

// Live session of an authority with whether a viewer was subscribed when the snapshot was taken
type LiveSessionView struct {
	SessionID  string
	Live       *LiveSession
	Subscribed bool
}

// Live sessions the authority holds in session id order, read under the authority lock
func (a *SessionAuthority) LiveSessionViews() []LiveSessionView {
	a.mu.Lock()
	views := make([]LiveSessionView, 0, len(a.Sessions))
	for sid, live := range a.Sessions {
		views = append(views, LiveSessionView{SessionID: sid, Live: live, Subscribed: live != nil && len(live.Subscribers) > 0})
	}
	a.mu.Unlock()
	sort.Slice(views, func(i, j int) bool { return views[i].SessionID < views[j].SessionID })
	return views
}

// Python exception class an error stands for, empty when it carries none
func PyErrorKind(err error) string { return pyErrorKind(err) }

// Whether the loop was closed so scheduling onto it fails
func (l *CallbackLoop) IsClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}
