package wecom

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Stream reply errcodes and frame limits of aibot_respond_msg
const (
	streamExpiredErrcode         = 846608
	streamRequestExpiredErrcode  = 846604
	streamVersionConflictErrcode = 6000
	maxStreamContentLength       = 20480
	maxIntermediateFrames        = 85
	defaultStreamSafeDuration    = 330.0
	defaultKeepaliveInterval     = 120.0
	replyAckTimeout              = 15 * time.Second
	thinkingSeed                 = "<think></think>"
)

// Stream or req_id reply flow WeCom expired, the caller falling back to a proactive send
type streamExpiredError struct {
	errcode int64
	errmsg  string
}

func (e *streamExpiredError) Error() string {
	msg := e.errmsg
	if msg == "" {
		msg = "no detail"
	}
	return fmt.Sprintf("WeCom stream expired (errcode=%d): %s", e.errcode, msg)
}

// Completion of one reply frame ack
type ackFuture struct {
	done    chan struct{}
	once    sync.Once
	payload map[string]any
	err     error
}

func newAckFuture() *ackFuture { return &ackFuture{done: make(chan struct{})} }

// Completes the ack once, later calls ignored
func (f *ackFuture) resolve(payload map[string]any, err error) {
	f.once.Do(func() {
		f.payload, f.err = payload, err
		close(f.done)
	})
}

func (f *ackFuture) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Reply frame awaiting its aibot_respond_msg ack
type replyFrame struct {
	body    map[string]any
	future  *ackFuture
	isFinal bool
	sentAt  time.Time
}

// Pending ack of one req_id, intermediates skipping while it is set and finals waiting on it
type replyQueue struct{ pendingAck *replyFrame }

// Stream state of one turn so concurrent messages never share a stream
type streamTurn struct {
	chatID, reqID, streamID    string
	accumulated                string
	finalized, seeded, expired bool
	start                      time.Time
	lastSent                   string
	intermediates              int
	keepalive                  *time.Timer
}

func newStreamTurn(chatID, reqID string) *streamTurn {
	return &streamTurn{chatID: chatID, reqID: reqID, streamID: "stream_" + randHex(6), start: time.Now()}
}

// Native msgtype stream replies of the adapter, per turn state, per req_id acks and the expiry defences
type Streamer struct {
	a                 *WeComAdapter
	keepaliveEnabled  bool
	keepaliveInterval time.Duration
	safeDuration      time.Duration

	mu           sync.Mutex
	replyQueues  map[string]*replyQueue
	turnOrder    []string
	turns        map[string]*streamTurn
	expiredChats map[string]bool
}

func newStreamer(a *WeComAdapter, keepaliveEnabled bool, keepaliveSeconds, safeSeconds float64) *Streamer {
	return &Streamer{a: a, keepaliveEnabled: keepaliveEnabled,
		keepaliveInterval: time.Duration(keepaliveSeconds * float64(time.Second)),
		safeDuration:      time.Duration(safeSeconds * float64(time.Second)),
		replyQueues:       map[string]*replyQueue{}, turns: map[string]*streamTurn{}, expiredChats: map[string]bool{}}
}

// Stream object of a reply body
func streamOf(body map[string]any) map[string]any { return objAt(body, "stream") }

// Seconds since a send time
func secondsSince(t time.Time) float64 { return time.Since(t).Seconds() }

// Clears the frame when still pending and drops its queue once empty, caller holds the lock
func (s *Streamer) releasePending(queue *replyQueue, reqID string, frame *replyFrame) {
	if queue.pendingAck == frame {
		queue.pendingAck = nil
	}
	if queue.pendingAck == nil && s.replyQueues[reqID] == queue {
		delete(s.replyQueues, reqID)
	}
}

// Sends aibot_respond_msg with per req_id ack tracking, finals draining the pending ack then awaiting their own
func (s *Streamer) sendReplyQueued(ctx context.Context, reqID string, body map[string]any, isFinal, skipIfPending bool) (map[string]any, error) {
	if err := s.a.requireWS(); err != nil {
		return nil, err
	}
	normalized := strings.TrimSpace(reqID)
	if normalized == "" {
		return nil, errors.New("reply_req_id is required")
	}
	s.mu.Lock()
	queue, ok := s.replyQueues[normalized]
	if !ok {
		queue = &replyQueue{}
		s.replyQueues[normalized] = queue
	}
	if skipIfPending && queue.pendingAck != nil {
		s.mu.Unlock()
		return map[string]any{"skipped": true, "errcode": int64(0), "errmsg": "pending_ack"}, nil
	}
	pending := queue.pendingAck
	s.mu.Unlock()
	if isFinal && pending != nil {
		s.drainPendingAck(ctx, queue, normalized, pending)
	}
	frame := &replyFrame{body: body, future: newAckFuture(), isFinal: isFinal, sentAt: time.Now()}
	s.mu.Lock()
	s.replyQueues[normalized] = queue
	queue.pendingAck = frame
	s.mu.Unlock()
	stream := streamOf(body)
	slog.Debug(fmt.Sprintf("[%s] _send_reply_queued: req_id=%s is_final=%t skip_if_pending=%t stream_id=%s finish=%s content_len=%d",
		s.a.Name(), normalized, isFinal, skipIfPending, textOf(stream["id"]), textOf(stream["finish"]), len([]rune(textOf(stream["content"])))))
	if err := s.a.sendJSON(map[string]any{"cmd": cmdRespond, "headers": map[string]any{"req_id": normalized}, "body": body}); err != nil {
		s.mu.Lock()
		s.releasePending(queue, normalized, frame)
		s.mu.Unlock()
		frame.future.resolve(nil, err)
		return nil, err
	}
	if !isFinal {
		return map[string]any{"errcode": int64(0), "errmsg": "sent_nonblocking"}, nil
	}
	defer func() {
		s.mu.Lock()
		s.releasePending(queue, normalized, frame)
		s.mu.Unlock()
	}()
	timer := time.NewTimer(replyAckTimeout)
	defer timer.Stop()
	select {
	case <-frame.future.done:
		return frame.future.payload, frame.future.err
	case <-timer.C:
		slog.Warn(fmt.Sprintf("[%s] Final frame ack timeout (req_id=%s) — treating as delivered, the frame already rendered; no fallback send", s.a.Name(), normalized))
		return map[string]any{"errcode": int64(0), "errmsg": "ack_timeout_assumed_delivered", "ack_pending": true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Waits a bounded time for the pending intermediate ack, then clears it either way
func (s *Streamer) drainPendingAck(ctx context.Context, queue *replyQueue, reqID string, pending *replyFrame) {
	stream := streamOf(pending.body)
	id, finish := textOf(stream["id"]), textOf(stream["finish"])
	slog.Debug(fmt.Sprintf("[%s] _send_reply_queued: final waiting for pending ack drain — req_id=%s pending_stream_id=%s pending_finish=%s pending_sent_at=%.1fs_ago",
		s.a.Name(), reqID, id, finish, secondsSince(pending.sentAt)))
	timer := time.NewTimer(replyAckTimeout)
	defer timer.Stop()
	select {
	case <-pending.future.done:
	case <-ctx.Done():
	case <-timer.C:
		slog.Warn(fmt.Sprintf("[%s] Reply ack timeout waiting for pending (req_id=%s) — pending_stream_id=%s pending_finish=%s elapsed=%.1fs. Possible causes: ack cmd filtered, ack req_id mismatch, or WeCom did not ack.",
			s.a.Name(), reqID, id, finish, secondsSince(pending.sentAt)))
	}
	s.mu.Lock()
	if queue.pendingAck == pending {
		queue.pendingAck = nil
	}
	s.mu.Unlock()
}

// Completes the pending ack of a req_id, true when one was pending
func (s *Streamer) resolveReplyAck(reqID string, payload map[string]any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, ok := s.replyQueues[reqID]
	if !ok || queue.pendingAck == nil {
		return false
	}
	frame := queue.pendingAck
	if !frame.future.isDone() {
		code := "N/A"
		if v, present := objAt(payload, "body")["errcode"]; present {
			code = textOf(v)
		}
		slog.Debug(fmt.Sprintf("[%s] _resolve_reply_ack: resolved req_id=%s is_final=%t elapsed=%.2fs errcode=%s",
			s.a.Name(), reqID, frame.isFinal, secondsSince(frame.sentAt), code))
		frame.future.resolve(payload, nil)
	}
	s.releasePending(queue, reqID, frame)
	return true
}

// Fails every pending ack with the error and forgets all queues
func (s *Streamer) failReplyQueues(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.replyQueues {
		if q.pendingAck != nil {
			q.pendingAck.future.resolve(nil, err)
		}
	}
	s.replyQueues = map[string]*replyQueue{}
}

// Fails pending acks and retires every turn with its keepalive timer
func (s *Streamer) reset(err error) {
	s.failReplyQueues(err)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.turns {
		cancelKeepalive(t)
	}
	s.turns, s.turnOrder, s.expiredChats = map[string]*streamTurn{}, nil, map[string]bool{}
}

// Lets a chat whose stream expired open new turns again after a fresh inbound req_id
func (s *Streamer) clearExpired(chatID string) {
	s.mu.Lock()
	delete(s.expiredChats, chatID)
	s.mu.Unlock()
}

// Whether the chat's last stream expired
func (s *Streamer) isExpired(chatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiredChats[chatID]
}

// Whether the chat has an unfinalized turn owning its req_id
func (s *Streamer) hasActiveTurn(chatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeTurnFor(chatID) != nil
}

// Stops the keepalive timer of a turn
func cancelKeepalive(turn *streamTurn) {
	if turn.keepalive != nil {
		turn.keepalive.Stop()
		turn.keepalive = nil
	}
}

// Registry key of a turn, its turn id or the req_id it locked to
func turnKey(chat, turnID, reqID string) string {
	if turnID == "" {
		turnID = reqID
	}
	return chat + ":" + turnID
}

// Drops a turn from the registry, caller holds the lock
func (s *Streamer) popTurn(key string) {
	if _, ok := s.turns[key]; !ok {
		return
	}
	delete(s.turns, key)
	for i, k := range s.turnOrder {
		if k == key {
			s.turnOrder = append(s.turnOrder[:i], s.turnOrder[i+1:]...)
			break
		}
	}
}

// Stores a turn keeping first registration order, caller holds the lock
func (s *Streamer) putTurn(key string, turn *streamTurn) {
	if _, ok := s.turns[key]; !ok {
		s.turnOrder = append(s.turnOrder, key)
	}
	s.turns[key] = turn
}

// Cancels the keepalive and drops the turn, caller holds the lock
func (s *Streamer) retireTurn(turn *streamTurn, turnID string) {
	cancelKeepalive(turn)
	s.popTurn(turnKey(turn.chatID, turnID, turn.reqID))
}

// Retires the turn and blocks new turns of its chat until a fresh req_id arrives, caller holds the lock
func (s *Streamer) expireTurn(turn *streamTurn, turnID string) {
	turn.expired = true
	s.retireTurn(turn, turnID)
	s.expiredChats[turn.chatID] = true
}

// Oldest unfinalized turn of the chat, caller holds the lock
func (s *Streamer) activeTurnFor(chatID string) *streamTurn {
	for _, k := range s.turnOrder {
		if t := s.turns[k]; t.chatID == chatID && !t.finalized {
			return t
		}
	}
	return nil
}

// Arms the keepalive timer once when enabled and the turn is live, caller holds the lock
func (s *Streamer) armKeepalive(turn *streamTurn, turnID string) {
	if !s.keepaliveEnabled || turn.finalized || turn.expired || turn.keepalive != nil {
		return
	}
	turn.keepalive = time.AfterFunc(s.keepaliveInterval, func() { s.onKeepaliveFire(turn, turnID) })
}

func (s *Streamer) onKeepaliveFire(turn *streamTurn, turnID string) {
	s.mu.Lock()
	turn.keepalive = nil
	live := !turn.finalized && !turn.expired
	s.mu.Unlock()
	if live {
		s.keepaliveSend(context.Background(), turn, turnID)
	}
}

// Resends the accumulated text as finish false to refresh the window, retiring the turn on expiry
func (s *Streamer) keepaliveSend(ctx context.Context, turn *streamTurn, turnID string) {
	s.mu.Lock()
	if turn.finalized || turn.expired || turn.intermediates >= maxIntermediateFrames {
		s.mu.Unlock()
		return
	}
	content := turn.accumulated
	if strings.TrimSpace(content) == "" {
		s.armKeepalive(turn, turnID)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	_, err := s.sendStreamReply(ctx, turn.reqID, turn.streamID, content, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired *streamExpiredError
	switch {
	case errors.As(err, &expired):
		s.expireTurn(turn, turnID)
		return
	case err != nil:
		slog.Debug(fmt.Sprintf("[%s] keep-alive send failed (chat=%s, turn=%s): %v", s.a.Name(), turn.chatID, turn.streamID, err))
	default:
		turn.lastSent = content
	}
	s.armKeepalive(turn, turnID)
}

// Content cut to the UTF-8 byte limit without splitting a character
func truncateStreamContent(content string, limit int) string {
	if len(content) <= limit {
		return content
	}
	cut := limit
	for cut > 0 && !utf8RuneStart(content[cut]) {
		cut--
	}
	return content[:cut]
}

// Whether the byte begins a UTF-8 sequence
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Sends one stream frame, intermediates non blocking and the final awaiting its ack to detect expiry
func (s *Streamer) sendStreamReply(ctx context.Context, reqID, streamID, content string, finish bool) (map[string]any, error) {
	truncated := truncateStreamContent(content, maxStreamContentLength)
	if len(truncated) != len(content) {
		slog.Warn(fmt.Sprintf("[%s] Stream content truncated for stream_id=%s", s.a.Name(), streamID))
	}
	body := map[string]any{"msgtype": "stream", "stream": map[string]any{"id": streamID, "finish": finish, "content": truncated}}
	if !finish {
		return s.sendReplyQueued(ctx, reqID, body, false, true)
	}
	response, err := s.sendReplyQueued(ctx, reqID, body, true, false)
	if err != nil {
		return nil, err
	}
	code, _ := errcodeOf(response)
	switch code {
	case streamExpiredErrcode, streamRequestExpiredErrcode:
		return nil, &streamExpiredError{errcode: code, errmsg: textOf(response["errmsg"])}
	case streamVersionConflictErrcode:
		slog.Info(fmt.Sprintf("[%s] finalize hit errcode 6000 (version conflict) — bubble already replaced by a newer frame; treating as delivered.", s.a.Name()))
		return response, nil
	}
	if err := raiseForWeComError(response, "send stream reply"); err != nil {
		return nil, err
	}
	return response, nil
}

// Seeds, updates or finalizes the turn keyed by turn id, false telling the caller to fall back to a plain send
func (s *Streamer) SendStreamFrame(ctx context.Context, text string, finalize bool, chatID, replyTo, turnID string) (bool, error) {
	chat := strings.TrimSpace(chatID)
	if chat == "" {
		slog.Warn(fmt.Sprintf("[%s] send_stream_frame: chat_id required", s.a.Name()))
		return false, nil
	}
	if turnID == "" && s.isExpired(chat) {
		return false, nil
	}
	if !finalize {
		return s.sendStreamFrameInner(ctx, text, chat, replyTo, false, turnID)
	}
	result, err := s.a.sendQueue().EnqueueChatSend(ctx, chat, func(qctx context.Context) (any, error) {
		return s.sendStreamFrameInner(qctx, text, chat, replyTo, true, turnID)
	}, true)
	if err != nil {
		return false, err
	}
	sent, _ := result.(bool)
	return sent, nil
}

// Finds or creates the turn, nil when unavailable, a new turn locking to the req_id it was created with, caller holds the lock
func (s *Streamer) locateTurn(chat, replyTo string, finalize bool, turnID string) *streamTurn {
	name := s.a.Name()
	if turnID != "" {
		if turn := s.turns[chat+":"+turnID]; turn != nil {
			return turn
		}
		if finalize {
			slog.Debug(fmt.Sprintf("[%s] send_stream_frame: cannot finalize non-existent turn (turn_id=%s, chat=%s)", name, turnID, chat))
			return nil
		}
	} else if existing := s.activeTurnFor(chat); existing != nil {
		slog.Debug(fmt.Sprintf("[%s] send_stream_frame: reusing existing turn %s for chat %s", name, existing.streamID, chat))
		return existing
	}
	suffix := ""
	if turnID != "" {
		suffix = " (turn_id=" + turnID + ")"
	}
	if s.expiredChats[chat] {
		slog.Debug(fmt.Sprintf("[%s] send_stream_frame: chat %s is expired, cannot create new turn%s", name, chat, suffix))
		return nil
	}
	reqID := s.a.cachedReplyReqID(chat, replyTo)
	if reqID == "" {
		slog.Debug(fmt.Sprintf("[%s] send_stream_frame: no req_id available for chat %s%s", name, chat, suffix))
		return nil
	}
	key := turnKey(chat, turnID, reqID)
	var turn *streamTurn
	if turnID == "" {
		turn = s.turns[key]
	}
	if turn == nil {
		turn = newStreamTurn(chat, reqID)
	}
	s.putTurn(key, turn)
	desc := "req_id=" + reqID
	if turnID != "" {
		desc = "turn_id=" + turnID + ", req_id=" + reqID
	}
	slog.Debug(fmt.Sprintf("[%s] send_stream_frame: created new turn %s (%s) for chat %s", name, turn.streamID, desc, chat))
	return turn
}

// Sends the finish frame, or declines it once the stream outlived its safe duration so a plain send delivers instead
func (s *Streamer) finalizeTurn(ctx context.Context, turn *streamTurn, text, chat, turnID string) (bool, error) {
	s.mu.Lock()
	if !s.keepaliveEnabled {
		if age := time.Since(turn.start); age >= s.safeDuration {
			slog.Info(fmt.Sprintf("[%s] Stream age %.0fs >= safe duration %.0fs for chat %s — declining finalize frame, falling back to proactive send.",
				s.a.Name(), age.Seconds(), s.safeDuration.Seconds(), chat))
			s.expireTurn(turn, turnID)
			s.mu.Unlock()
			return false, nil
		}
	}
	cancelKeepalive(turn)
	finalText := text
	if text != "" && text == turn.lastSent {
		finalText = text + "​"
	}
	s.mu.Unlock()
	if _, err := s.sendStreamReply(ctx, turn.reqID, turn.streamID, finalText, true); err != nil {
		return false, err
	}
	s.mu.Lock()
	turn.finalized = true
	s.popTurn(turnKey(chat, turnID, turn.reqID))
	s.mu.Unlock()
	return true, nil
}

// Stream unavailable for this frame, the caller falling back to a plain send
var errDecline = errors.New("stream unavailable")

// Runs one frame of a turn, dropping failed intermediates and retiring the turn when a final frame fails
func (s *Streamer) sendStreamFrameInner(ctx context.Context, text, chat, replyTo string, finalize bool, turnID string) (bool, error) {
	var turn *streamTurn
	err := func() error {
		s.mu.Lock()
		turn = s.locateTurn(chat, replyTo, finalize, turnID)
		if turn == nil || turn.expired {
			s.mu.Unlock()
			return errDecline
		}
		seed := !turn.seeded && !turn.finalized
		s.mu.Unlock()
		if seed {
			if _, err := s.sendStreamReply(ctx, turn.reqID, turn.streamID, thinkingSeed, false); err != nil {
				return err
			}
			s.mu.Lock()
			turn.seeded = true
			s.armKeepalive(turn, turnID)
			s.mu.Unlock()
			if text == "" && !finalize {
				return nil
			}
		}
		if finalize {
			sent, err := s.finalizeTurn(ctx, turn, text, chat, turnID)
			if err == nil && !sent {
				return errDecline
			}
			return err
		}
		s.mu.Lock()
		turn.accumulated = text
		if turn.intermediates >= maxIntermediateFrames || text == turn.lastSent {
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		if _, err := s.sendStreamReply(ctx, turn.reqID, turn.streamID, text, false); err != nil {
			return err
		}
		s.mu.Lock()
		turn.intermediates++
		turn.lastSent = text
		s.mu.Unlock()
		return nil
	}()
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errDecline):
		return false, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	}
	name := s.a.Name()
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired *streamExpiredError
	if errors.As(err, &expired) {
		if !finalize {
			slog.Info(fmt.Sprintf("[%s] Intermediate stream frame expired (errcode=%d) for chat %s — dropping frame, stream stays live", name, expired.errcode, chat))
			return true, nil
		}
		slog.Info(fmt.Sprintf("[%s] Stream expired (errcode=%d) for chat %s — switching to proactive send", name, expired.errcode, chat))
		s.expireTurn(turn, turnID)
		return false, nil
	}
	if !finalize {
		slog.Info(fmt.Sprintf("[%s] Intermediate stream frame failed (chat=%s): %v — dropping frame, stream stays live", name, chat, err))
		return true, nil
	}
	slog.Warn(fmt.Sprintf("[%s] Stream frame failed (chat=%s): %v", name, chat, err))
	s.retireTurn(turn, turnID)
	return false, nil
}

// Stream frames work in DMs and groups alike, groups only needing a cached inbound req_id
func (s *Streamer) SupportsNativeStreaming(chatType string, metadata map[string]any) bool {
	return true
}
