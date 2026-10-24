package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// Session label naming the profile the session runs on
const profileLabel = "profile"

// Session open in the shell with the stores that log into it and the endpoint its runs go to
type Live struct {
	Session  *session.Session
	Facts    *facts.Store
	Missions *mission.Store
	Endpoint Endpoint
}

// Wraps a session in stores restored from its records, binds it to the endpoint and registers it
func (a *App) live(sess *session.Session, ep Endpoint) (*Live, error) {
	l := &Live{Session: sess, Facts: facts.New(), Missions: mission.New(), Endpoint: ep}
	if err := l.Facts.Restore(sess); err != nil {
		return nil, err
	}
	if err := l.Missions.Restore(sess); err != nil {
		return nil, err
	}
	l.Facts.Attach(sess)
	l.Missions.Attach(sess)
	if sess.Model() != ep.Model {
		sess.SetModel(ep.Model)
	}
	if sess.Labels()[profileLabel] != ep.Profile.Name {
		sess.Label(profileLabel, ep.Profile.Name)
	}
	id := sess.ID()
	l.Facts.OnChange(func(c facts.Change) { a.send(FactMsg{Session: id, Change: c}) })
	l.Missions.OnChange(func(c mission.Change) { a.send(MissionMsg{Session: id, Mission: c.Mission, Op: c.Op}) })
	a.mu.Lock()
	a.lives = append(a.lives, l)
	a.mu.Unlock()
	return l, nil
}

// Endpoint a stored session resumes on, its labeled profile and model, else the default
func (a *App) endpointFor(ctx context.Context, sess *session.Session) (Endpoint, error) {
	name := sess.Labels()[profileLabel]
	if name == "" {
		return a.Primary, nil
	}
	if name == a.Primary.Profile.Name {
		return withModel(a.Primary, sess.Model()), nil
	}
	if _, ok := a.FindProfile(name); !ok {
		return a.Primary, nil
	}
	return a.open(ctx, Target{Profile: name, Model: sess.Model()})
}

// Endpoint with the model swapped for another the same profile serves, looked up in the catalog
func withModel(ep Endpoint, id string) Endpoint {
	if id == "" {
		return ep
	}
	ep.Model = id
	if known, ok := profiles.Describe(id); ok {
		ep.Catalog = known
	} else {
		ep.Catalog = profiles.Model{ID: id, Vendor: ep.Profile.Vendor}
	}
	ep.Catalog.ID = id
	return ep
}

// Endpoint the runs of a session go to, the default for a session that is not open
func (a *App) endpointOf(sess *session.Session) Endpoint {
	if l := a.LiveOf(sess); l != nil {
		return l.Endpoint
	}
	return a.Primary
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
	ep, err := a.endpointFor(ctx, sess)
	if err != nil {
		return nil, err
	}
	return a.live(sess, ep)
}

// Opens a fresh session on the default endpoint beside the ones already open
func (a *App) Create() (*Live, error) {
	return a.live(session.New(), a.Primary)
}

// Opens a fork of an open session cut at n entries on the same endpoint and writes it to the store
func (a *App) Fork(l *Live, n int) (*Live, error) {
	if a.Busy(l) {
		return nil, errors.New("harness: an agent is still working")
	}
	f := l.Session.Fork()
	f.Truncate(n)
	nl, err := a.live(f, l.Endpoint)
	if err != nil {
		return nil, err
	}
	a.Saver.Flush(f)
	return nl, nil
}

// Changes the model of an open session, moving the default along when the session is on the default profile
func (a *App) SetModelOn(l *Live, id string) {
	l.Endpoint = withModel(l.Endpoint, id)
	l.Session.SetModel(id)
	if l.Endpoint.Profile.Name == a.Primary.Profile.Name {
		a.Primary = withModel(a.Primary, id)
		a.Model = a.chain(a.Primary)
		a.Config.Model = id
	}
	if l.Session == a.Session {
		a.Meter.SetCatalog(l.Endpoint.Catalog, l.Endpoint.Profile.Name)
	}
}

// Moves an open session to an opened endpoint, which becomes the default for new sessions
func (a *App) SetEndpointOn(l *Live, name string, ep Endpoint) error {
	if a.Busy(l) {
		return errors.New("harness: an agent is still working")
	}
	l.Endpoint = ep
	l.Session.SetModel(ep.Model)
	l.Session.Label(profileLabel, ep.Profile.Name)
	a.Primary = ep
	a.Model = a.chain(ep)
	a.Config.Profile, a.Config.Model = name, ep.Model
	if l.Session == a.Session {
		a.Meter.SetCatalog(ep.Catalog, ep.Profile.Name)
	}
	return nil
}

// Reports whether the profile is the default or any open session runs on it
func (a *App) ProfileInUse(name string) bool {
	if a.Primary.Profile.Name == name {
		return true
	}
	for _, l := range a.Lives() {
		if l.Endpoint.Profile.Name == name {
			return true
		}
	}
	return false
}

// Writes an open session that holds anything and forgets it, selecting another when it was the selected one
func (a *App) Release(l *Live) error {
	if a.Busy(l) {
		return errors.New("harness: an agent is still working")
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

// Makes an open session the one hooks, the meter and the headless runner work on
func (a *App) Select(l *Live) {
	a.Session, a.Facts, a.Missions = l.Session, l.Facts, l.Missions
	a.Control.Missions = l.Missions
	a.Meter.SetCatalog(l.Endpoint.Catalog, l.Endpoint.Profile.Name)
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

// Opens an empty session under a stored id whose file is gone, on the default endpoint
func (a *App) Revive(id string) (*Live, error) {
	if !session.ValidID(id) {
		return nil, fmt.Errorf("harness: %q is not a session id", id)
	}
	if l := a.LiveByID(id); l != nil {
		return l, nil
	}
	now := time.Now().UTC()
	return a.live(session.FromHeader(session.Header{ID: id, Created: now, Updated: now}, nil, nil), a.Primary)
}
