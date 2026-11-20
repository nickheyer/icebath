package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One session of a listing, from the authority's list, the gateway's live list or its stored list
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

// Sessions of a listing, the authority's live or stored scope on a canonical gateway, else the live sessions or the stored ones
func requestSessions(gw turnGateway, live bool, currentSid string, then func([]listedSession, error) tea.Cmd) tea.Cmd {
	if gw.canonical() {
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
				out[i] = listedSession{ID: s.ID, SessionID: s.SessionID, Title: s.Title, Running: s.Running, When: val(s.StartedAt), Count: float64(s.MessageCount)}
			}
			return then(out, nil)
		})
	}
	if live {
		params := tuigateway.SessionActiveListParams{}
		if currentSid != "" {
			params.CurrentSessionID = &currentSid
		}
		return call(gw, tuigateway.SessionActiveList, params, func(r *tuigateway.SessionActiveListResult, err error) tea.Cmd {
			if err != nil {
				return then(nil, err)
			}
			out := make([]listedSession, len(r.Sessions))
			for i, s := range r.Sessions {
				out[i] = listedSession{ID: s.ID, SessionID: s.SessionKey, Title: s.Title, Preview: s.Preview, Status: string(s.Status), Current: s.Current,
					When: firstWhen(s.LastActive, s.StartedAt), Count: float64(s.MessageCount)}
			}
			return then(out, nil)
		})
	}
	return call(gw, tuigateway.SessionList, tuigateway.SessionListParams{Limit: ptr(int64(200))}, func(r *tuigateway.SessionListResult, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		out := make([]listedSession, len(r.Sessions))
		for i, s := range r.Sessions {
			out[i] = listedSession{ID: s.ID, Title: s.Title, Preview: s.Preview, When: s.StartedAt, Count: float64(s.MessageCount)}
		}
		return then(out, nil)
	})
}

// Last activity of a live session, its start when it was never active
func firstWhen(lastActive, startedAt float64) float64 {
	if lastActive != 0 {
		return lastActive
	}
	return startedAt
}
