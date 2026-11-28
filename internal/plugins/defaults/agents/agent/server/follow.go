package server

import (
	"context"
	"maps"
	"slices"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Keeps what sessions hold of providers and accounts in step with them, as the provider host tells of each change: the provider and model
// a session picked for itself, the settings it runs with over its profile's, such as the account it is held to, and the retries armed
// for an account to reset. A session that named a provider or account removed drops that choice, one that named a renamed one takes the
// new name. What it changed, as phrases such as 2 sessions
func (a *Agent) Follow(ctx context.Context, cfg *config.Config, change provider.Change) ([]string, error) {
	var said []string
	store, err := a.store()
	if err != nil {
		return said, err
	}
	profile := cfg.Profile()
	touched := map[string]bool{}
	a.mu.Lock()
	runs := maps.Clone(a.runs)
	a.mu.Unlock()
	for id, r := range runs {
		r.mu.Lock()
		if r.profile != profile {
			r.mu.Unlock()
			continue
		}
		picked := change.Account == "" && r.provider == change.Provider
		if picked && change.To == "" {
			r.provider, r.model = "", ""
		} else if picked {
			r.provider = change.To
		}
		var kept *sessions.Overrides
		if r.overrides != nil {
			if values, moved := provider.Carry(cfg, r.overrides.Values, change); moved {
				kept = &sessions.Overrides{Values: values, Bare: r.overrides.Bare}
				r.overrides = kept
			}
		}
		r.mu.Unlock()
		if kept != nil {
			if err := store.Update(ctx, id, kept.Change()); err != nil {
				return a.followed(touched, 0), err
			}
		}
		if picked || kept != nil {
			touched[id] = true
			a.changed(r)
		}
	}
	listed, _, err := store.List(ctx, &sessionsv1.Filter{Profile: profile, IncludeArchived: true, IncludeHidden: true, IncludeChildren: true,
		IncludeEmpty: true, IncludeOpen: true, IncludePinned: true})
	if err != nil {
		return a.followed(touched, 0), err
	}
	for _, session := range listed {
		overrides := sessions.OverridesOf(session)
		if runs[session.GetId()] != nil || overrides == nil {
			continue
		}
		values, moved := provider.Carry(cfg, overrides.Values, change)
		if !moved {
			continue
		}
		if err := store.Update(ctx, session.GetId(), (&sessions.Overrides{Values: values, Bare: overrides.Bare}).Change()); err != nil {
			return a.followed(touched, 0), err
		}
		touched[session.GetId()] = true
	}
	return append(said, a.followed(touched, a.followWaits(change))...), nil
}

// Retries armed for an account to reset, in step with the change: those of a provider or account removed dropped, those of a renamed one
// carried over. How many were dropped
func (a *Agent) followWaits(change provider.Change) int {
	var dropped []string
	a.storeWaits(func(waits []armedWait) []armedWait {
		var kept []armedWait
		for _, w := range waits {
			mine := w.Provider == change.Provider && (change.Account == "" || w.Login == change.Account)
			switch {
			case !mine:
				kept = append(kept, w)
			case change.To == "":
				dropped = append(dropped, w.Session)
			case change.Account == "":
				w.Provider = change.To
				kept = append(kept, w)
			default:
				w.Login = change.To
				kept = append(kept, w)
			}
		}
		return kept
	})
	for _, id := range dropped {
		a.waits.mu.Lock()
		timer := a.waits.timers[id]
		delete(a.waits.timers, id)
		a.waits.mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
	}
	return len(dropped)
}

// Phrases of what following a change touched: the sessions, and the retries dropped
func (a *Agent) followed(touched map[string]bool, dropped int) []string {
	var said []string
	if len(touched) > 0 {
		said = append(said, strs.Count(len(touched), "session"))
	}
	switch {
	case dropped == 1:
		said = append(said, "a wait for an account to reset")
	case dropped > 1:
		said = append(said, strs.Count(dropped, "wait")+" for accounts to reset")
	}
	return slices.Clip(said)
}
