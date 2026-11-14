package tool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrPreparationCancelled = errors.New("Terminal approval preparation cancelled; command was not started")
	errPreparationTimeout   = errors.New("Terminal approval preparation timed out; commands were not started")
)

const preparationPoll = 100 * time.Millisecond

type preparationKey struct{}

type Preparation struct {
	batch       *prepBatch
	preparing   atomic.Bool
	ready       chan struct{}
	readyOnce   sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func PreparationFrom(ctx context.Context) (*Preparation, bool) {
	p, ok := ctx.Value(preparationKey{}).(*Preparation)
	return p, ok && p != nil
}

func (p *Preparation) Preparing() bool { return p.preparing.Load() }

func (p *Preparation) Published() {
	p.readyOnce.Do(func() { close(p.ready) })
}

func (p *Preparation) Err() error { return p.batch.err() }

func (p *Preparation) FailureSeen() bool {
	p.batch.mu.Lock()
	defer p.batch.mu.Unlock()
	return p.batch.failure
}

func (p *Preparation) OnClose(fn func()) error {
	b := p.batch
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancelled || b.turn.Err() != nil {
		return ErrPreparationCancelled
	}
	b.closers = append(b.closers, fn)
	return nil
}

func (p *Preparation) releaseSlot() {
	p.releaseOnce.Do(func() { close(p.release) })
}

type prepSlot struct {
	p       *parsed
	call    *Call
	prep    *Preparation
	claimed bool
	started bool
	done    chan struct{}
	m       *managed
	cancel  context.CancelCauseFunc
	takeErr error
}

type prepBatch struct {
	r         *run
	turn      context.Context
	auth      *authGate
	mu        sync.Mutex
	cancelled bool
	cancelCh  chan struct{}
	failure   bool
	closers   []func()
	slots     []*prepSlot
}

func (b *prepBatch) err() error {
	b.mu.Lock()
	cancelled := b.cancelled
	b.mu.Unlock()
	if cancelled || b.turn.Err() != nil {
		return ErrPreparationCancelled
	}
	return nil
}

func (r *run) preparedBatch(ctx context.Context, ps []*parsed) *prepBatch {
	if !r.b.PrepareAhead || len(ps) < 2 {
		return nil
	}
	seen := map[string]bool{}
	for _, p := range ps {
		id := CanonicalCallID(p.tc.ID)
		if !capable(p) || id == "" || seen[id] {
			return nil
		}
		seen[id] = true
	}
	pb := &prepBatch{r: r, turn: ctx, auth: r.newAuthGate(), cancelCh: make(chan struct{})}
	for _, p := range ps {
		pr := &Preparation{batch: pb, ready: make(chan struct{}), release: make(chan struct{})}
		pb.slots = append(pb.slots, &prepSlot{p: p, call: r.callFor(p), prep: pr, done: make(chan struct{})})
	}
	if ctx.Err() != nil || r.err != nil {
		return pb
	}
	if err := pb.start(); err != nil {
		pb.close()
		if errors.Is(err, errPreparationTimeout) {
			r.b.Interrupt.Stop("terminal batch preparation timeout")
		}
	}
	return pb
}

func (b *prepBatch) start() error {
	r := b.r
	for _, s := range b.slots {
		if err := b.err(); err != nil {
			return err
		}
		wctx, cancel := context.WithCancelCause(context.WithValue(b.turn, preparationKey{}, s.prep))
		b.mu.Lock()
		s.cancel, s.started = cancel, true
		b.mu.Unlock()
		opts := pipeOpts{index: s.p.index, call: s.call, entry: s.p.entry, scope: s.p.scope, authGate: b.auth, dispatch: r.sequentialDispatch}
		go func(s *prepSlot) {
			defer close(s.done)
			defer s.prep.Published()
			s.m = r.pipeline(wctx, s.p.args, opts)
		}(s)
		if err := b.awaitReady(s); err != nil {
			return err
		}
	}
	return nil
}

func (b *prepBatch) awaitReady(s *prepSlot) error {
	r := b.r
	started := time.Now()
	baseline := b.auth.excluded()
	tick := time.NewTicker(preparationPoll)
	defer tick.Stop()
	for {
		select {
		case <-s.prep.ready:
			return nil
		case <-tick.C:
		}
		if err := b.err(); err != nil {
			return err
		}
		elapsed := time.Since(started) - (b.auth.excluded() - baseline)
		if r.seqOn && elapsed >= r.seq {
			return errPreparationTimeout
		}
	}
}

func (b *prepBatch) take(id string) *prepSlot {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.slots {
		if s.call.ID != id || s.claimed {
			continue
		}
		s.claimed = true
		if b.cancelled || b.turn.Err() != nil || !s.started {
			s.takeErr = ErrPreparationCancelled
			return s
		}
		s.prep.releaseSlot()
		return s
	}
	return nil
}

func (b *prepBatch) markFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failure = true
}

func (b *prepBatch) close() {
	b.mu.Lock()
	if b.cancelled {
		b.mu.Unlock()
		return
	}
	b.cancelled = true
	close(b.cancelCh)
	closers := b.closers
	b.closers = nil
	slots := b.slots
	b.mu.Unlock()
	for _, fn := range closers {
		fn()
	}
	for _, s := range slots {
		s.prep.releaseSlot()
		if !s.started || s.cancel == nil {
			continue
		}
		select {
		case <-s.done:
			s.cancel(context.Canceled)
		default:
			s.cancel(&Interrupted{})
		}
	}
}

func (r *run) prepare(ctx context.Context, o pipeOpts, args Args) error {
	pr, ok := PreparationFrom(ctx)
	if !ok {
		return o.entry.Tool.Prepare(ctx, o.call, args)
	}
	if err := pr.Err(); err != nil {
		return err
	}
	pr.preparing.Store(true)
	err := o.entry.Tool.Prepare(ctx, o.call, args)
	pr.preparing.Store(false)
	pr.Published()
	if err != nil {
		return err
	}
	select {
	case <-pr.release:
	case <-pr.batch.cancelCh:
	case <-ctx.Done():
	}
	return pr.Err()
}
