package harness

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

const (
	// Session label holding the tuning as JSON
	tuningLabel = "tuning"
	// Session label naming the session a sub-agent serves
	parentLabel = "parent"
)

// How a session departs from the defaults, the base prompt it sends and what its requests ask of the endpoint
type Tuning struct {
	// Base prompt sent in place of the default while Custom is set, empty sending none
	System string `json:"system,omitempty"`
	Custom bool   `json:"custom,omitempty"`
	// Leaves the environment block about the machine and session out of the system prompt
	NoContext bool `json:"no_context,omitempty"`
	// Asks the endpoint to add no prompt or tools of its own
	Bare bool `json:"bare,omitempty"`
	// Keeps the session off disk and asks the endpoint to keep nothing
	Ghost bool `json:"ghost,omitempty"`
}

// Tuning the session carries, the defaults while it carries none
func TuningOf(s *session.Session) Tuning {
	var t Tuning
	if raw := s.Labels()[tuningLabel]; raw != "" {
		json.Unmarshal([]byte(raw), &t)
	}
	return t
}

// Directives the tuning asks of the endpoint
func (t Tuning) Directives() protocol.Directives {
	return protocol.Directives{Ghost: t.Ghost, Bare: t.Bare}
}

// Applies the tuning to an open session, one turning ghost losing its stored copy and those of its sub-agents
func (a *App) Tune(ctx context.Context, l *Live, t Tuning) error {
	was := TuningOf(l.Session).Ghost
	if t == (Tuning{}) {
		l.Session.Label(tuningLabel, "")
	} else {
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		l.Session.Label(tuningLabel, string(data))
	}
	if t.Ghost {
		if was {
			return nil
		}
		return a.forget(ctx, l.Session)
	}
	a.Saver.Touch(l.Session)
	return nil
}

// Deletes the stored copies of the session and of every sub-agent session that served it
func (a *App) forget(ctx context.Context, s *session.Session) error {
	if a.Store == nil {
		return nil
	}
	infos, err := a.Store.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, in := range infos {
		if in.ID == s.ID() || in.Labels[parentLabel] == s.ID() {
			errs = append(errs, a.Store.Delete(ctx, in.ID))
		}
	}
	return errors.Join(errs...)
}

// Reports whether nothing of the session may reach disk, by its own ghost or that of the session it serves
func (a *App) Ghosted(s *session.Session) bool {
	if TuningOf(s).Ghost {
		return true
	}
	if id := s.Labels()[parentLabel]; id != "" {
		if l := a.LiveByID(id); l != nil {
			return TuningOf(l.Session).Ghost
		}
	}
	return false
}

// Base prompt every session starts from, the conduct and the configured instructions
func (a *App) DefaultBase() string { return Base(a.System) }

// Environment block the client adds for a run on the model as the machine stands now
func (a *App) EnvironmentFor(ctx context.Context, model string) string {
	return Environment(GatherFacts(a.Shell.Dir(), a.Root, model, mission.IDFrom(ctx) != ""))
}

// System prompt of the session on the model, its own base if set and the environment unless left out
func (a *App) PromptFor(ctx context.Context, s *session.Session, model string) string {
	t := TuningOf(s)
	base, env := a.DefaultBase(), ""
	if t.Custom {
		base = t.System
	}
	if !t.NoContext {
		env = a.EnvironmentFor(ctx, model)
	}
	return Compose(base, env)
}

// Context whose requests ask what the session tuning asks at the moment each one is sent
func (a *App) Directed(ctx context.Context, s *session.Session) context.Context {
	return protocol.WithDirectives(ctx, func() protocol.Directives { return TuningOf(s).Directives() })
}
