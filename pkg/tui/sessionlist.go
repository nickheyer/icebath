package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One session of a listing of the background service
type listedSession struct {
	ID        string
	SessionID string
	Title     string
	Preview   string
	Status    string
	Running   bool
	Current   bool
	When      float64
	Count     float64
}

// Sessions of a listing, the background service's live or stored scope
func requestSessions(gw turnGateway, live bool, then func([]listedSession, error) tea.Cmd) tea.Cmd {
	scope := tuigateway.CanonicalListParamsScope("stored")
	if live {
		scope = "live"
	}
	params := tuigateway.CanonicalListParams{Scope: &scope, Limit: ptr(int64(200))}
	return call(gw, tuigateway.CanonicalSessionList, params, func(r *tuigateway.CanonicalListResult, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		out := make([]listedSession, len(r.Sessions))
		for i, s := range r.Sessions {
			out[i] = listedSession{ID: s.ID, SessionID: s.SessionID, Title: s.Title, Preview: val(s.Preview), Running: s.Running, When: val(s.StartedAt), Count: float64(s.MessageCount)}
		}
		return then(out, nil)
	})
}
