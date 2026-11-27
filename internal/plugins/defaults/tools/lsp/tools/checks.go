package tools

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

// What a write may spend waiting for its verdict, and the verdicts kept by file
const (
	writeBudget = 80 * time.Millisecond
	inlineGrace = 80 * time.Millisecond
	verdictCap  = 256
)

// How long a write may still wait for its verdict, the grace when its start is not known
func inlineWait(started time.Time) time.Duration {
	if started.IsZero() {
		return inlineGrace
	}
	return max(0, min(inlineGrace, time.Until(started.Add(writeBudget))))
}

// A note not yet delivered, with the text before the edit its verdict was diffed against
type carried struct {
	text   string
	before *string
}

// Verdict of one check; without a verdict none came in the budget
type result struct {
	diags   []diagnostic
	verdict bool
	before  *string
	// A later check of the same session on the file took its place
	superseded bool
	// Notes the check took over and did not settle, handed back
	carry []carried
}

// One check under way
type inflight struct {
	done       chan struct{}
	result     *result
	err        error
	before     *string
	carry      []carried
	superseded bool
}

func (f *inflight) finished() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// The verdict of a file last judged, by the digest of the text
type verdict struct {
	path   string
	digest string
	diags  []diagnostic
}

// The checks of a service: the last verdicts, the newest check of each file and of each session on it, and the newest text written
type checks struct {
	mu       sync.Mutex
	verdicts *list.List
	index    map[string]*list.Element
	tail     map[string]*inflight
	owned    map[[2]string]*inflight
	newest   map[string]*string
}

func newChecks() *checks {
	return &checks{verdicts: list.New(), index: map[string]*list.Element{}, tail: map[string]*inflight{}, owned: map[[2]string]*inflight{},
		newest: map[string]*string{}}
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(asDiskRead(text)))
	return hex.EncodeToString(sum[:])
}

// Keeps the verdict on a text as the newest, the oldest let go past the cap
func (c *checks) remember(path, text string, diags []diagnostic) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.index[path]; ok {
		c.verdicts.Remove(e)
	}
	c.index[path] = c.verdicts.PushBack(&verdict{path, digest(text), append([]diagnostic{}, diags...)})
	for c.verdicts.Len() > verdictCap {
		front := c.verdicts.Front()
		delete(c.index, front.Value.(*verdict).path)
		c.verdicts.Remove(front)
	}
}

// Verdict kept on exactly this text, false when the one kept judged another
func (c *checks) recall(path, text string) ([]diagnostic, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.index[path]
	if !ok {
		return nil, false
	}
	v := e.Value.(*verdict)
	if v.digest != digest(text) {
		return nil, false
	}
	return append([]diagnostic{}, v.diags...), true
}

func (c *checks) newestFor(path string) *string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.newest[path]
}

// A check started, read once it ends
type future struct {
	done      chan struct{}
	result    *result
	err       error
	cancelled bool
}

// Verdict within the timeout, ready false while the check still runs
func (f *future) wait(timeout time.Duration) (*result, bool, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-f.done:
	case <-t.C:
		select {
		case <-f.done:
		default:
			return nil, false, nil
		}
	}
	if f.cancelled {
		return nil, true, context.Canceled
	}
	return f.result, true, f.err
}

// Runs fn once the check ends, whether it was cancelled
func (f *future) then(fn func(res *result, cancelled bool, err error)) {
	go func() {
		<-f.done
		fn(f.result, f.cancelled, f.err)
	}()
}

// Starts the check of a written file in the background, nil once the service has stopped
func (s *service) startCheck(path string, before, after *string, owner string, carry []carried) *future {
	if s.ctx.Err() != nil {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	s.checks.mu.Lock()
	if after != nil {
		s.checks.newest[abs] = after
	} else {
		delete(s.checks.newest, abs)
	}
	s.checks.mu.Unlock()
	f := &future{done: make(chan struct{})}
	carry = append([]carried{}, carry...)
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		defer close(f.done)
		res, err := s.serialized(s.ctx, abs, before, after, owner, carry)
		if err != nil && cancelled(err) {
			f.cancelled = true
			return
		}
		f.result, f.err = res, err
	}()
	return f
}

// Runs after any check of the file still under way, taking over the unfinished check of the same session
func (s *service) serialized(ctx context.Context, abs string, before, after *string, owner string, carry []carried) (res *result, err error) {
	c := s.checks
	baseline := before
	if len(carry) > 0 {
		baseline = carry[0].before
	}
	c.mu.Lock()
	tail := c.tail[abs]
	if tail != nil && tail.finished() {
		tail = nil
	}
	var prev *inflight
	if owner != "" {
		prev = c.owned[[2]string{owner, abs}]
	}
	if prev != nil && !prev.finished() {
		prev.superseded = true
		baseline = prev.before
		carry = append(append([]carried{}, prev.carry...), carry...)
	} else {
		prev = nil
	}
	me := &inflight{done: make(chan struct{}), before: baseline, carry: carry}
	c.tail[abs] = me
	if owner != "" {
		c.owned[[2]string{owner, abs}] = me
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.tail[abs] == me {
			delete(c.tail, abs)
			if after != nil && c.newest[abs] == after {
				delete(c.newest, abs)
			}
		}
		if owner != "" && c.owned[[2]string{owner, abs}] == me {
			delete(c.owned, [2]string{owner, abs})
		}
		me.result, me.err = res, err
		c.mu.Unlock()
		close(me.done)
	}()
	for _, w := range []*inflight{prev, tail} {
		if w == nil {
			continue
		}
		select {
		case <-w.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var earlier *result
	c.mu.Lock()
	if prev != nil && prev.err == nil && prev.result != nil && prev.result.verdict {
		earlier = prev.result
	}
	superseded := me.superseded
	c.mu.Unlock()
	if superseded {
		if earlier != nil {
			return &result{diags: earlier.diags, verdict: true, before: earlier.before, superseded: true}, nil
		}
		return &result{before: baseline, superseded: true}, nil
	}
	budget := s.budget(abs)
	type outcome struct {
		diags []diagnostic
		ok    bool
	}
	v, cerr := within(ctx, 2*budget+spawnCeiling, func(ctx context.Context) (outcome, error) {
		d, ok, err := s.run(ctx, abs, baseline, after, budget)
		return outcome{d, ok}, err
	})
	if cerr != nil {
		if cancelled(cerr) {
			return nil, cerr
		}
		s.m.logf(slog.LevelDebug, "LSP edit check failed for %s: %s", abs, cerr)
		s.breakFor(abs, cerr)
		v = outcome{}
	}
	switch {
	case v.ok:
		res = &result{diags: v.diags, verdict: true, before: baseline}
	case earlier != nil:
		res = &result{diags: earlier.diags, verdict: true, before: earlier.before}
	default:
		res = &result{before: baseline, carry: carry}
	}
	c.mu.Lock()
	res.superseded = me.superseded
	c.mu.Unlock()
	if res.superseded {
		res.carry = nil
	}
	return res, nil
}

// Verdict on the edit's own text, then, when it has diagnostics, the verdict on the text before it to leave out what was there already
func (s *service) run(ctx context.Context, abs string, before, after *string, budget time.Duration) ([]diagnostic, bool, error) {
	c, err := s.client(ctx, abs)
	if err != nil || c == nil {
		return nil, false, err
	}
	srv := s.serverFor(abs)
	id := "lsp"
	if srv != nil {
		id = srv.id
	}
	language := srv.languageFor(abs)
	sync := c.syncs()
	_, opened := c.synced(abs)
	if c.seeds && !opened && !sync {
		open := before
		if before == nil {
			open = after
		}
		version, err := c.openFile(ctx, abs, language, open)
		if err != nil {
			return nil, false, err
		}
		var seed []diagnostic
		seeded := false
		if version == 0 {
			if seed, seeded, err = c.awaitSeed(ctx, abs, budget); err != nil {
				return nil, false, err
			}
		}
		if before == nil {
			if err := s.resync(ctx, c, abs, language); err != nil {
				return nil, false, err
			}
			s.touch(c)
			return s.report(id, abs, seed, seeded)
		}
		if seeded {
			s.checks.remember(abs, *before, seed)
		}
	}
	version, err := c.openFile(ctx, abs, language, after)
	if err != nil {
		return nil, false, err
	}
	if err := c.saveFile(ctx, abs); err != nil {
		return nil, false, err
	}
	diags, ok, err := s.verdict(ctx, c, abs, version, budget, sync)
	if err != nil {
		return nil, false, err
	}
	s.touch(c)
	if !ok {
		if err := s.resync(ctx, c, abs, language); err != nil {
			return nil, false, err
		}
		return s.report(id, abs, nil, false)
	}
	synced, hasSynced := c.synced(abs)
	judged := diags
	if len(diags) > 0 && before != nil && hasSynced {
		baseline, cached := s.checks.recall(abs, *before)
		if !cached {
			baseline = []diagnostic{}
			baseVersion, err := c.openFile(ctx, abs, language, before)
			if err != nil {
				return nil, false, err
			}
			base, baseOK, err := s.verdict(ctx, c, abs, baseVersion, budget, sync)
			if err != nil {
				return nil, false, err
			}
			if baseOK {
				baseline = base
			}
		}
		diags = without(diags, shifted(baseline, lineShift(*before, synced)))
	}
	if hasSynced {
		s.checks.remember(abs, synced, judged)
	}
	if err := s.resync(ctx, c, abs, language); err != nil {
		return nil, false, err
	}
	return s.report(id, abs, diags, true)
}

// Leaves the server holding the newest text written to the file, read from disk when not known
func (s *service) resync(ctx context.Context, c *client, abs, language string) error {
	newest := s.checks.newestFor(abs)
	if newest == nil {
		if _, err := c.openFile(ctx, abs, language, nil); err != nil {
			if cancelled(err) {
				return err
			}
			s.m.logf(slog.LevelDebug, "LSP resync of %s failed", abs)
		}
		return nil
	}
	if text, ok := c.synced(abs); !ok || asDiskRead(*newest) != text {
		_, err := c.openFile(ctx, abs, language, newest)
		return err
	}
	return nil
}

// Diagnostics of the text synced at the version, from the synchronous request first where the server has one
func (s *service) verdict(ctx context.Context, c *client, abs string, version int, budget time.Duration, sync bool) ([]diagnostic, bool, error) {
	if sync {
		diags, ok, err := c.syncDiagnostics(ctx, abs, budget)
		if err != nil {
			if _, timedOut := errors.AsType[*timeoutError](err); timedOut {
				return nil, false, nil
			}
			return nil, false, err
		}
		if ok {
			return diags, true, nil
		}
	}
	fresh, err := c.awaitDiagnostics(ctx, abs, version, s.settings.waitMode, budget)
	if err != nil || !fresh {
		return nil, false, err
	}
	return c.diagnosticsFor(abs), true, nil
}

// Says how the check came out, and passes its verdict on
func (s *service) report(id, abs string, diags []diagnostic, ok bool) ([]diagnostic, bool, error) {
	switch {
	case !ok:
		s.m.logTimeout(id, abs, "fresh diagnostics")
	case len(diags) > 0:
		s.m.logDiagnostics(id, abs, len(diags))
	default:
		s.m.logClean(id, abs)
	}
	return diags, ok, nil
}
