package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
)

// Shared stores of the process, one refcounted generation per resolved path, retired generations draining until their last holder lets go
type registry struct {
	mu          sync.Mutex
	generations map[string]*generation
	retired     map[*Store]*generation
	opening     map[string]chan struct{}
	tearingDown map[string]*teardownBarrier
	lifecycle   map[string]*sync.Mutex
}

// One shared store, its refcount and the file identity it opened
type generation struct {
	path          string
	store         *Store
	identity      fileIdentity
	identityKnown bool
	refs          int
	retired       bool
}

// Every admitted but unfinished physical close of one path, lifted only by the last one to settle
type teardownBarrier struct {
	done    chan struct{}
	pending int
}

// Empty registry
func newRegistry() registry {
	return registry{
		generations: map[string]*generation{}, retired: map[*Store]*generation{}, opening: map[string]chan struct{}{},
		tearingDown: map[string]*teardownBarrier{}, lifecycle: map[string]*sync.Mutex{},
	}
}

// Lifecycle mutex serializing opens and closes of path (caller holds mu)
func (r *registry) lifecycleLocked(path string) *sync.Mutex {
	l := r.lifecycle[path]
	if l == nil {
		l = &sync.Mutex{}
		r.lifecycle[path] = l
	}
	return l
}

// Registers one pending physical close of path (caller holds mu)
func (r *registry) admitTeardownLocked(path string) *teardownBarrier {
	b := r.tearingDown[path]
	if b == nil {
		b = &teardownBarrier{done: make(chan struct{})}
		r.tearingDown[path] = b
	}
	b.pending++
	return b
}

// Settles one admitted close, the last one lifting the barrier of path
func (r *registry) finishTeardown(path string, b *teardownBarrier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b.pending--; b.pending > 0 {
		return
	}
	if r.tearingDown[path] == b {
		delete(r.tearingDown, path)
	}
	close(b.done)
}

// Closes a shared store under the lifecycle mutex of path, then settles its barrier slot
func (e *Engine) teardownGeneration(ctx context.Context, path string, s *Store, b *teardownBarrier) {
	r := &e.shared
	r.mu.Lock()
	l := r.lifecycleLocked(path)
	r.mu.Unlock()
	l.Lock()
	closeShared(ctx, path, s)
	l.Unlock()
	if b != nil {
		r.finishTeardown(path, b)
	}
}

// Physically closes a store the registry owned, a lost generation left unsettled reported as the data at risk it is
func closeShared(ctx context.Context, path string, s *Store) {
	s.shared.Store(false)
	err := s.closeHandle(context.WithoutCancel(ctx))
	var ce *CaptureError
	switch {
	case err == nil:
	case errors.As(err, &ce):
		slog.Error(fmt.Sprintf("state store for %s did not settle at close: %s", path, errText(err)))
	default:
		slog.Debug(fmt.Sprintf("Error closing shared state store for %s: %s", path, errText(err)))
	}
}

// Shared writable store of path, the home state database when empty, refcounted, a replaced file retiring the old generation
func (e *Engine) Acquire(ctx context.Context, path string, d Database, so SessionOptions) (*Store, error) {
	raw, err := e.storePath(ctx, path)
	if err != nil {
		return nil, err
	}
	key := resolvePath(raw)
	site := callerSite()
	r := &e.shared
	for {
		var wait chan struct{}
		var opening chan struct{}
		var lifecycle *sync.Mutex
		r.mu.Lock()
		if g := r.generations[key]; g != nil {
			if cur, ok := statIdentity(key); ok && g.identityKnown && cur != g.identity {
				g.retired = true
				delete(r.generations, key)
				r.retired[g.store] = g
			} else {
				g.refs++
				r.mu.Unlock()
				return g.store, nil
			}
		}
		if b := r.tearingDown[key]; b != nil {
			wait = b.done
		} else if ch := r.opening[key]; ch != nil {
			wait = ch
		} else {
			opening = make(chan struct{})
			r.opening[key] = opening
			lifecycle = r.lifecycleLocked(key)
		}
		r.mu.Unlock()
		if wait != nil {
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		lifecycle.Lock()
		s, err := e.openStore(ctx, Options{Path: key, Database: d, Sessions: so}, sessionTables(so), site)
		var id fileIdentity
		idKnown := false
		if err == nil {
			s.shared.Store(true)
			id, idKnown = statIdentity(key)
		}
		lifecycle.Unlock()
		r.mu.Lock()
		if err != nil {
			r.finishOpeningLocked(key, opening)
			r.mu.Unlock()
			return nil, err
		}
		barrier := r.tearingDown[key]
		winner := s
		if barrier == nil {
			if existing := r.generations[key]; existing != nil {
				existing.refs++
				winner = existing.store
			} else {
				r.generations[key] = &generation{path: key, store: s, identity: id, identityKnown: idKnown, refs: 1}
			}
		}
		r.finishOpeningLocked(key, opening)
		r.mu.Unlock()
		if barrier != nil {
			e.teardownGeneration(ctx, key, s, nil)
			select {
			case <-barrier.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if winner != s {
			e.teardownGeneration(ctx, key, s, nil)
		}
		return winner, nil
	}
}

// Drops the construction marker of path and wakes its waiters (caller holds mu)
func (r *registry) finishOpeningLocked(path string, opening chan struct{}) {
	if r.opening[path] == opening {
		delete(r.opening, path)
	}
	close(opening)
}

// Releases one reference of a shared store, the last one tearing its generation down
func (e *Engine) releaseShared(ctx context.Context, s *Store) {
	r := &e.shared
	r.mu.Lock()
	g := r.retired[s]
	if g == nil {
		g = r.generations[s.path]
		if g == nil || g.store != s {
			r.mu.Unlock()
			return
		}
	}
	g.refs--
	var barrier *teardownBarrier
	if g.refs <= 0 {
		if g.retired {
			delete(r.retired, s)
		} else if r.generations[g.path] == g {
			delete(r.generations, g.path)
		}
		barrier = r.admitTeardownLocked(g.path)
	}
	r.mu.Unlock()
	if barrier != nil {
		e.teardownGeneration(ctx, g.path, s, barrier)
	}
}

// Closes every shared store regardless of refcount and returns how many, waiting for closes already under way
func (e *Engine) CloseAll(ctx context.Context) int {
	return e.sweepShared(ctx, func(string) bool { return true })
}

// Force closes every shared store under dir and returns how many, so a profile directory can go
func (e *Engine) CloseAllUnder(ctx context.Context, dir string) int {
	root := resolvePath(dir)
	return e.sweepShared(ctx, func(path string) bool { return underDir(resolvePath(path), root) })
}

// Retires and closes every generation whose path match selects, then waits for the matching closes already admitted
func (e *Engine) sweepShared(ctx context.Context, match func(path string) bool) int {
	r := &e.shared
	r.mu.Lock()
	var swept []*generation
	for _, g := range r.generations {
		if match(g.path) {
			swept = append(swept, g)
		}
	}
	for _, g := range r.retired {
		if match(g.path) {
			swept = append(swept, g)
		}
	}
	var active []*teardownBarrier
	for path, b := range r.tearingDown {
		if match(path) {
			active = append(active, b)
		}
	}
	barriers := map[string]*teardownBarrier{}
	for _, g := range swept {
		if barriers[g.path] == nil {
			barriers[g.path] = r.admitTeardownLocked(g.path)
		}
		g.retired = true
		if r.generations[g.path] == g {
			delete(r.generations, g.path)
		}
		delete(r.retired, g.store)
	}
	r.mu.Unlock()
	byPath := map[string][]*generation{}
	for _, g := range swept {
		byPath[g.path] = append(byPath[g.path], g)
	}
	for path, gens := range byPath {
		r.mu.Lock()
		l := r.lifecycleLocked(path)
		r.mu.Unlock()
		l.Lock()
		for _, g := range gens {
			closeShared(ctx, path, g.store)
		}
		l.Unlock()
		r.finishTeardown(path, barriers[path])
	}
	for _, b := range active {
		<-b.done
	}
	return len(swept)
}

// Descriptions of every other live shared generation this process holds on path, retired generations excluded
func (e *Engine) otherGenerations(path string, exclude *Store) []string {
	key := resolvePath(path)
	r := &e.shared
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, g := range r.generations {
		if g.store != exclude && g.path == key && !g.retired {
			out = append(out, fmt.Sprintf("in-process live state store generation (refcount %d)", g.refs))
		}
	}
	return out
}

// Runs fn over every live shared store with a reference pinned on each for the whole call
func (e *Engine) Borrow(ctx context.Context, fn func(stores []*Store) error) error {
	r := &e.shared
	r.mu.Lock()
	var borrowed []*Store
	for _, g := range r.generations {
		if !g.retired {
			g.refs++
			borrowed = append(borrowed, g.store)
		}
	}
	r.mu.Unlock()
	defer func() {
		for _, s := range slices.Backward(borrowed) {
			e.releaseShared(ctx, s)
		}
	}()
	return fn(borrowed)
}
