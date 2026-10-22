package harness

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// Session open in the shell with the stores that log into it
type Live struct {
	Session  *session.Session
	Facts    *facts.Store
	Missions *mission.Store
}

// Wraps a session in stores restored from its records and registers it
func (a *App) live(sess *session.Session) (*Live, error) {
	l := &Live{Session: sess, Facts: facts.New(), Missions: mission.New()}
	if err := l.Facts.Restore(sess); err != nil {
		return nil, err
	}
	if err := l.Missions.Restore(sess); err != nil {
		return nil, err
	}
	l.Facts.Attach(sess)
	l.Missions.Attach(sess)
	id := sess.ID()
	l.Facts.OnChange(func(c facts.Change) { a.send(FactMsg{Session: id, Change: c}) })
	l.Missions.OnChange(func(c mission.Change) { a.send(MissionMsg{Session: id, Mission: c.Mission, Op: c.Op}) })
	a.mu.Lock()
	a.lives = append(a.lives, l)
	a.mu.Unlock()
	return l, nil
}

// Opens a stored session beside the ones already open, returning the open one when it already is
func (a *App) Open(ctx context.Context, id string) (*Live, error) {
	if l := a.LiveByID(id); l != nil {
		return l, nil
	}
	sess, err := a.Store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	return a.live(sess)
}

// Opens a fresh session on the primary model beside the ones already open
func (a *App) Create() (*Live, error) {
	sess := session.New()
	sess.SetModel(a.Primary.Model)
	return a.live(sess)
}

// Opens a fork of an open session cut at n entries and writes it to the store
func (a *App) Fork(l *Live, n int) (*Live, error) {
	if a.Busy(l) {
		return nil, errors.New("harness: a run is in progress")
	}
	f := l.Session.Fork()
	f.Truncate(n)
	nl, err := a.live(f)
	if err != nil {
		return nil, err
	}
	a.Saver.Flush(f)
	return nl, nil
}

// Writes an open session and forgets it, selecting another when it was the selected one
func (a *App) Release(l *Live) error {
	if a.Busy(l) {
		return errors.New("harness: a run is in progress")
	}
	a.Saver.Flush(l.Session)
	a.mu.Lock()
	kept := a.lives[:0]
	for _, have := range a.lives {
		if have != l {
			kept = append(kept, have)
		}
	}
	a.lives = kept
	next := len(kept) > 0 && a.Session == l.Session
	var first *Live
	if next {
		first = kept[0]
	}
	a.mu.Unlock()
	if first != nil {
		a.Select(first)
	}
	return nil
}

// Makes an open session the one hooks, model switches and the headless runner work on
func (a *App) Select(l *Live) {
	a.Session, a.Facts, a.Missions = l.Session, l.Facts, l.Missions
	a.Control.Missions = l.Missions
}

// Open sessions in open order
func (a *App) Lives() []*Live {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*Live(nil), a.lives...)
}

// Open session the app works on
func (a *App) Current() *Live {
	return a.LiveOf(a.Session)
}

// Open session wrapping sess, nil when it is not open
func (a *App) LiveOf(sess *session.Session) *Live {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.lives {
		if l.Session == sess {
			return l
		}
	}
	return nil
}

// Open session with the id, nil when it is not open
func (a *App) LiveByID(id string) *Live {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.lives {
		if l.Session.ID() == id {
			return l
		}
	}
	return nil
}

// Starts a run on an open session
func (a *App) RunOn(ctx context.Context, l *Live, missionID string, input []protocol.Part) (*Run, error) {
	ag := a.NewAgent("", l.Session)
	return a.Control.Start(ctx, ag, missionID, input)
}

// Reports whether a root run goes on the open session
func (a *App) Busy(l *Live) bool {
	for _, r := range a.Control.Runs() {
		if r.Running() && r.Parent == "" && r.Session == l.Session {
			return true
		}
	}
	return false
}

// Open session a run serves, found through the root of a child run
func (a *App) LiveOfRun(id string) *Live {
	for id != "" {
		r, ok := a.Control.Get(id)
		if !ok {
			return nil
		}
		if l := a.LiveOf(r.Session); l != nil {
			return l
		}
		id = r.Parent
	}
	return nil
}

// Facts store the calling run records into, the selected one outside a run
func (a *App) factsFor(ctx context.Context) *facts.Store {
	if l := a.LiveOfRun(RunFrom(ctx)); l != nil {
		return l.Facts
	}
	return a.Facts
}

// Mission store the calling run plans into, the selected one outside a run
func (a *App) missionsFor(ctx context.Context) *mission.Store {
	if l := a.LiveOfRun(RunFrom(ctx)); l != nil {
		return l.Missions
	}
	return a.Missions
}
