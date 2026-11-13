package photon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Durable event domains and the resumable stream retry ladder
const (
	streamMessages     = "messages"
	streamPolls        = "polls"
	streamGroups       = "groups"
	streamInitialDelay = 500 * time.Millisecond
	streamMaxDelay     = 30 * time.Second
	liveBufferLimit    = 1000
	persistentFailures = 5
	recoverTimeout     = 30 * time.Second
)

var (
	errLiveEnded    = errors.New("live stream ended")
	errLiveOverflow = fmt.Errorf("live stream buffer exceeded %d events during catch-up", liveBufferLimit)
)

// Catch-up refused by the server for its resume cursor
type cursorRejectedError struct{ cause error }

func (e *cursorRejectedError) Error() string { return "server rejected resume cursor: " + e.cause.Error() }
func (e *cursorRejectedError) Unwrap() error { return e.cause }

// Receiver of one inbound message at a time, blocking until it is handled
type messageSink func(ctx context.Context, m *smsg) error

// Last delivered sequence of every line and domain, kept across session restarts for gap free catch-up
type cursorStore struct {
	mu      sync.Mutex
	cursors map[string]string
}

func newCursorStore() *cursorStore { return &cursorStore{cursors: map[string]string{}} }

func (c *cursorStore) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursors[key]
}

func (c *cursorStore) set(key, cursor string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cursor == "" {
		delete(c.cursors, key)
		return
	}
	c.cursors[key] = cursor
}

// Resumable streams of every line, members added and retired as the line set changes
type streamGroup struct {
	ctx     context.Context
	sink    messageSink
	health  *streamHealth
	cursors *cursorStore

	mu      sync.Mutex
	closed  bool
	members map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func newStreamGroup(ctx context.Context, sink messageSink, health *streamHealth, cursors *cursorStore) *streamGroup {
	return &streamGroup{ctx: ctx, sink: sink, health: health, cursors: cursors, members: map[string]context.CancelFunc{}}
}

// Starts the message and poll streams of a line, plus group events for dedicated lines
func (g *streamGroup) attach(l *line) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.members[l.key] != nil {
		return
	}
	ctx, cancel := context.WithCancel(g.ctx)
	g.members[l.key] = cancel
	kinds := []string{streamMessages, streamPolls}
	if l.key != sharedPhone {
		kinds = append(kinds, streamGroups)
	}
	for _, kind := range kinds {
		r := &resumableStream{l: l, kind: kind, g: g, id: uuid.NewString(), label: streamLabel(kind, l)}
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			r.run(ctx)
		}()
	}
}

func (g *streamGroup) detach(l *line) {
	g.mu.Lock()
	cancel := g.members[l.key]
	delete(g.members, l.key)
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Cancels every member stream and waits for them to finish
func (g *streamGroup) close() {
	g.mu.Lock()
	g.closed = true
	for key, cancel := range g.members {
		cancel()
		delete(g.members, key)
	}
	g.mu.Unlock()
	g.wg.Wait()
}

// Hands one surfaced message to the sink, a multi part bubble as its separate items
func (g *streamGroup) emit(ctx context.Context, m *smsg) error {
	values := []*smsg{m}
	if m.Content != nil && m.Content.Type == "group" {
		values = m.Content.Items
	}
	for _, v := range values {
		g.health.markHealthy()
		if err := g.sink(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

func streamLabel(kind string, l *line) string {
	phone := l.currentPhone()
	if phone != sharedPhone {
		phone = sanitizePhone(phone)
	}
	return "imessage." + kind + ":" + phone
}

// One live subscription that catches up from its cursor after every reconnect and never ends with an error
type resumableStream struct {
	l     *line
	kind  string
	g     *streamGroup
	id    string
	label string

	cursor      string
	delivered   map[string]struct{}
	failed      int
	lastRecover int
	delay       time.Duration
}

func (r *resumableStream) cursorKey() string { return r.l.key + "/" + r.kind }

func (r *resumableStream) run(ctx context.Context) {
	r.cursor = r.g.cursors.get(r.cursorKey())
	r.delivered = map[string]struct{}{}
	r.delay = streamInitialDelay
	for ctx.Err() == nil {
		phase := "live"
		var err error
		if r.cursor != "" {
			phase = "catch-up"
			err = r.catchUpThenLive(ctx)
		} else {
			err = r.consumeLive(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		delay := r.handleFailure(err, phase)
		r.maybeRecover(ctx)
		jittered := time.Duration(float64(delay) * (0.5 + rand.Float64()*0.5))
		timer := time.NewTimer(jittered)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Whether a frame carries an event this stream maps
func (r *resumableStream) wanted(fr *streamFrame) bool {
	switch r.kind {
	case streamMessages:
		return fr.Message != nil && fr.Message.Kind != ""
	case streamPolls:
		return fr.Poll != nil && fr.Poll.Kind != ""
	case streamGroups:
		return fr.Group != nil && fr.Group.Kind != ""
	}
	return false
}

func (r *resumableStream) item(ctx context.Context, fr *streamFrame) streamItem {
	switch r.kind {
	case streamPolls:
		return r.l.pollItem(ctx, fr.Poll, fr.Seq)
	case streamGroups:
		return r.l.groupItem(ctx, fr.Group, fr.Seq)
	}
	return r.l.messageItem(ctx, fr.Message, fr.Seq)
}

func numericCursor(cursor string) (uint64, bool) {
	if cursor == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(cursor, 10, 64)
	return v, err == nil
}

// Moves the cursor forward, never back, optionally forgetting the ids delivered under the old one
func (r *resumableStream) advance(cursor string, clear bool) {
	if cursor == "" || cursor == r.cursor {
		return
	}
	next, nextOK := numericCursor(cursor)
	current, currentOK := numericCursor(r.cursor)
	if nextOK && currentOK && next < current {
		return
	}
	r.cursor = cursor
	r.g.cursors.set(r.cursorKey(), cursor)
	if clear {
		r.delivered = map[string]struct{}{}
	}
}

func (r *resumableStream) deliver(ctx context.Context, item streamItem, resetRetry, clear bool) error {
	if _, seen := r.delivered[item.id]; !seen {
		for _, v := range item.values {
			if err := r.g.emit(ctx, v); err != nil {
				return err
			}
		}
	}
	r.advance(item.cursor, clear)
	r.delivered[item.id] = struct{}{}
	if resetRetry {
		r.noteRecovery()
	}
	return nil
}

func (r *resumableStream) noteRecovery() {
	r.delay, r.lastRecover = streamInitialDelay, 0
	if r.failed == 0 {
		return
	}
	r.l.sess.tele.log(logInfo, "spectrum.stream", "stream recovered", map[string]any{"spectrum.stream.id": r.id,
		"spectrum.stream.label": r.label, "spectrum.stream.attempt": r.failed})
	r.failed = 0
	r.g.health.markHealthy()
}

// Subscribes without a cursor and delivers every live event until the stream breaks
func (r *resumableStream) consumeLive(ctx context.Context) error {
	st, err := r.l.subscribe(ctx, r.kind)
	if err != nil {
		return err
	}
	defer st.close()
	for {
		b, err := st.recv()
		if errors.Is(err, io.EOF) {
			return errLiveEnded
		}
		if err != nil {
			return err
		}
		fr, err := decodeSubscribeFrame(r.kind, b)
		if err != nil {
			return err
		}
		r.g.health.noteFrame()
		if !fr.HasSeq || !r.wanted(fr) {
			continue
		}
		if err := r.deliver(ctx, r.item(ctx, fr), true, true); err != nil {
			return err
		}
	}
}

// Opens the live stream first, buffers it while the missed events replay, then flushes and stays live
func (r *resumableStream) catchUpThenLive(ctx context.Context) error {
	after, hasAfter := numericCursor(r.cursor)
	liveCtx, cancelLive := context.WithCancel(ctx)
	defer cancelLive()
	st, err := r.l.subscribe(liveCtx, r.kind)
	if err != nil {
		return err
	}
	defer st.close()
	var mu sync.Mutex
	buffering := true
	frames := make(chan *streamFrame, liveBufferLimit)
	liveErr := make(chan error, 1)
	go func() {
		for {
			b, err := st.recv()
			if errors.Is(err, io.EOF) {
				err = errLiveEnded
			}
			var fr *streamFrame
			if err == nil {
				fr, err = decodeSubscribeFrame(r.kind, b)
			}
			if err != nil {
				liveErr <- err
				return
			}
			r.g.health.noteFrame()
			if !fr.HasSeq || !r.wanted(fr) || (hasAfter && fr.Seq <= after) {
				continue
			}
			mu.Lock()
			if buffering {
				select {
				case frames <- fr:
					mu.Unlock()
					continue
				default:
					mu.Unlock()
					liveErr <- errLiveOverflow
					return
				}
			}
			mu.Unlock()
			select {
			case frames <- fr:
			case <-liveCtx.Done():
				return
			}
		}
	}()
	checkLive := func() error {
		select {
		case err := <-liveErr:
			return err
		default:
			return nil
		}
	}
	if hasAfter {
		if err := r.replay(ctx, after, checkLive); err != nil {
			return err
		}
	}
	lastID := ""
	for flushing := true; flushing; {
		if err := checkLive(); err != nil {
			return err
		}
		mu.Lock()
		select {
		case fr := <-frames:
			mu.Unlock()
			item := r.item(ctx, fr)
			if err := r.deliver(ctx, item, true, false); err != nil {
				return err
			}
			lastID = item.id
		default:
			buffering, flushing = false, false
			mu.Unlock()
		}
	}
	if lastID != "" {
		r.delivered = map[string]struct{}{lastID: {}}
	}
	r.noteRecovery()
	for {
		select {
		case err := <-liveErr:
			return err
		case fr := <-frames:
			if err := r.deliver(ctx, r.item(ctx, fr), true, true); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Replays every event of this domain after the cursor up to the catch-up head
func (r *resumableStream) replay(ctx context.Context, after uint64, checkLive func() error) error {
	wrap := func(err error) error {
		if re := asRPCError(err); re != nil && re.validation() {
			return &cursorRejectedError{cause: err}
		}
		return err
	}
	st, err := r.l.catchUp(ctx, after)
	if err != nil {
		return wrap(err)
	}
	defer st.close()
	for {
		b, err := st.recv()
		if errors.Is(err, io.EOF) {
			return checkLive()
		}
		if err != nil {
			return wrap(err)
		}
		if err := checkLive(); err != nil {
			return err
		}
		fr, err := decodeFrame(b)
		if err != nil {
			return err
		}
		if fr.Complete {
			head := strconv.FormatUint(fr.Head, 10)
			if err := r.deliver(ctx, streamItem{cursor: head, id: "catchup.complete:" + head}, false, false); err != nil {
				return err
			}
			return checkLive()
		}
		if !fr.HasSeq || !r.wanted(fr) {
			continue
		}
		if err := r.deliver(ctx, r.item(ctx, fr), false, false); err != nil {
			return err
		}
	}
}

// Counts a failure, logs it the way the stream health classifier reads it and returns the backoff
func (r *resumableStream) handleFailure(err error, phase string) time.Duration {
	r.failed++
	delay := r.delay
	r.delay = min(r.delay*2, streamMaxDelay)
	var rejected *cursorRejectedError
	kind := "transient"
	switch {
	case errors.As(err, &rejected):
		kind = "cursor-rejected"
	case r.failed >= persistentFailures:
		kind = "persistent"
	}
	attrs := withError(map[string]any{"spectrum.stream.id": r.id, "spectrum.stream.label": r.label, "spectrum.stream.phase": phase,
		"spectrum.stream.has_cursor": r.cursor != "", "spectrum.stream.cursor": r.cursor, "spectrum.stream.attempt": r.failed,
		"spectrum.stream.delay_ms": delay.Milliseconds(), "spectrum.stream.failure_kind": kind}, err)
	tele := r.l.sess.tele
	switch kind {
	case "cursor-rejected":
		r.cursor = ""
		r.g.cursors.set(r.cursorKey(), "")
		r.delivered = map[string]struct{}{}
		tele.log(logWarn, "spectrum.stream", "resume cursor rejected; accepting event gap and resuming live", attrs)
	case "persistent":
		tele.log(logError, "spectrum.stream", "stream persistently failing; still retrying", attrs)
		r.g.health.markDegraded(fmt.Sprintf("[spectrum.stream] stream persistently failing; still retrying (%s, attempt %d): %v", r.label, r.failed, err))
	default:
		tele.log(logWarn, "spectrum.stream", "stream interrupted; reconnecting", attrs)
		r.g.health.markRecovering(fmt.Sprintf("[spectrum.stream] stream interrupted; reconnecting (%s, attempt %d): %v", r.label, r.failed, err))
	}
	return delay
}

// Forces a token re-mint every five failures once the stream is persistently failing
func (r *resumableStream) maybeRecover(ctx context.Context) {
	if r.failed < persistentFailures || r.failed-r.lastRecover < persistentFailures {
		return
	}
	r.lastRecover = r.failed
	rctx, cancel := context.WithTimeout(ctx, recoverTimeout)
	defer cancel()
	attrs := map[string]any{"spectrum.stream.id": r.id, "spectrum.stream.label": r.label, "spectrum.stream.attempt": r.failed}
	if err := r.l.sess.forceRefresh(rctx); err != nil {
		r.l.sess.tele.log(logWarn, "spectrum.stream", "stream recover hook failed", withError(attrs, err))
		return
	}
	r.l.sess.tele.log(logInfo, "spectrum.stream", "stream recover hook ran", attrs)
}
