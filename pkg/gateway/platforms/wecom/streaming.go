package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	AppCmdResponse                 = "aibot_respond_msg"
	StreamExpiredErrcode           = 846608
	StreamRequestExpiredErrcode    = 846604
	StreamNotSubscribedErrcode     = 846609
	StreamVersionConflictErrcode   = 6000
	MaxStreamContentLength         = 20480
	MaxIntermediateFrames          = 85
	StreamSafeDurationSeconds      = 330.0
	StreamKeepaliveIntervalSeconds = 120.0
	StreamKeepaliveEnabledDefault  = false
)

// Swappable reply ack timeout, the official REPLY_SEND_TIMEOUT_MS
var replyAckTimeout = 15 * time.Second

// WeComStreamExpiredError marks a dead stream or req_id reply flow that must fall back to aibot_send_msg
type WeComStreamExpiredError struct {
	Errcode any
	Errmsg  string
}

func (e *WeComStreamExpiredError) Error() string {
	msg := e.Errmsg
	if msg == "" {
		msg = "no detail"
	}
	return fmt.Sprintf("WeCom stream expired (errcode=%s): %s", pyrepr.Str(e.Errcode), msg)
}

// Asyncio future of one reply frame ack
type replyFuture struct {
	done      chan struct{}
	once      sync.Once
	payload   *pyjson.Object
	err       error
	cancelled bool
}

func newReplyFuture() *replyFuture { return &replyFuture{done: make(chan struct{})} }

func (f *replyFuture) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

func (f *replyFuture) finish(payload *pyjson.Object, err error, cancelled bool) {
	f.once.Do(func() {
		f.payload, f.err, f.cancelled = payload, err, cancelled
		close(f.done)
	})
}

// ReplyFrame is a frame awaiting its aibot_respond_msg ack
type ReplyFrame struct {
	Body    *pyjson.Object
	future  *replyFuture
	IsFinal bool
	SentAt  time.Time
}

// ReplyQueue tracks the pending ack of one req_id
type ReplyQueue struct {
	ReqID      string
	PendingAck *ReplyFrame
}

// StreamTurn is the per turn stream state so concurrent messages never share a stream
type StreamTurn struct {
	ChatID, ReqID, StreamID    string
	AccumulatedText            string
	Finalized, Seeded, Expired bool
	StartTime                  time.Time
	LastSentContent            string
	intermediateFramesSent     int
	keepalive                  *time.Timer
}

func newStreamTurn(chatID, reqID string) *StreamTurn {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return &StreamTurn{ChatID: chatID, ReqID: reqID, StreamID: "stream_" + hex.EncodeToString(b), StartTime: time.Now()}
}

// StreamHost is the adapter surface native streaming relies on
type StreamHost interface {
	Name() string
	RequireWS() error
	RequireReplyReqID(reqID string) (string, error)
	SendJSON(ctx context.Context, payload *pyjson.Object) error
	ReplyReqIDForMessage(messageID string) string
	LastChatReqID(chatID string) string
	RaiseForWeComError(response *pyjson.Object, action string) error
	EnqueueChatSend(ctx context.Context, chatID string, send func(ctx context.Context) (any, error), isControl bool) (any, error)
}

// Streamer is the native streaming state the adapter embeds with its stream config
type Streamer struct {
	Host                     StreamHost
	KeepaliveEnabled         bool
	KeepaliveIntervalSeconds float64
	SafeDurationSeconds      float64

	mu           sync.Mutex
	replyQueues  map[string]*ReplyQueue
	turnOrder    []string
	turns        map[string]*StreamTurn
	expiredChats map[string]bool
}

func (s *Streamer) init() {
	if s.replyQueues == nil {
		s.replyQueues, s.turns, s.expiredChats = map[string]*ReplyQueue{}, map[string]*StreamTurn{}, map[string]bool{}
	}
}

func obj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func streamOf(body *pyjson.Object) *pyjson.Object {
	if v, _ := body.Get("stream"); v != nil {
		if o, ok := v.(*pyjson.Object); ok {
			return o
		}
	}
	return pyjson.NewObject()
}

func getOr(o *pyjson.Object, key string, def any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	return def
}

func streamDesc(body *pyjson.Object) (string, string) {
	st := streamOf(body)
	return pyrepr.Str(getOr(st, "id", "N/A")), pyrepr.Str(getOr(st, "finish", "N/A"))
}

func elapsed(since time.Time) float64 {
	if since.IsZero() {
		return 0
	}
	return time.Since(since).Seconds()
}

// Clears the frame when still pending and drops its queue once empty, caller holds the lock
func (s *Streamer) releasePending(queue *ReplyQueue, reqID string, frame *ReplyFrame) {
	if queue.PendingAck == frame {
		queue.PendingAck = nil
	}
	if queue.PendingAck == nil {
		delete(s.replyQueues, reqID)
	}
}

// Sends aibot_respond_msg with per req_id ack tracking, finals drain the pending ack then await their own
func (s *Streamer) sendReplyQueued(ctx context.Context, reqID string, body *pyjson.Object, isFinal, skipIfPending bool) (*pyjson.Object, error) {
	if err := s.Host.RequireWS(); err != nil {
		return nil, err
	}
	normalized, err := s.Host.RequireReplyReqID(reqID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.init()
	queue, ok := s.replyQueues[normalized]
	if !ok {
		queue = &ReplyQueue{ReqID: normalized}
		s.replyQueues[normalized] = queue
	}
	if skipIfPending && queue.PendingAck != nil {
		s.mu.Unlock()
		return obj("skipped", true, "errcode", int64(0), "errmsg", "pending_ack"), nil
	}
	if isFinal && queue.PendingAck != nil {
		s.mu.Unlock()
		s.drainPendingAck(ctx, queue, normalized)
		s.mu.Lock()
	}
	frame := &ReplyFrame{Body: body, future: newReplyFuture(), IsFinal: isFinal, SentAt: time.Now()}
	s.replyQueues[normalized] = queue
	queue.PendingAck = frame
	s.mu.Unlock()
	id, finish := streamDesc(body)
	slog.Debug(fmt.Sprintf("[%s] _send_reply_queued: req_id=%s is_final=%s skip_if_pending=%s stream_id=%s finish=%s content_len=%d",
		s.Host.Name(), normalized, pyrepr.Str(isFinal), pyrepr.Str(skipIfPending), id, finish, pystr.Len(pyrepr.Str(getOr(streamOf(body), "content", "")))))
	if err := s.Host.SendJSON(ctx, obj("cmd", AppCmdResponse, "headers", obj("req_id", normalized), "body", body)); err != nil {
		s.mu.Lock()
		s.releasePending(queue, normalized, frame)
		s.mu.Unlock()
		frame.future.finish(nil, nil, true)
		return nil, err
	}
	if !isFinal {
		return obj("errcode", int64(0), "errmsg", "sent_nonblocking"), nil
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
		if frame.future.err != nil {
			return nil, frame.future.err
		}
		if frame.future.cancelled {
			return nil, context.Canceled
		}
		return frame.future.payload, nil
	case <-timer.C:
		frame.future.finish(nil, nil, true)
		slog.Warn(fmt.Sprintf("[%s] Final frame ack timeout (req_id=%s) — treating as delivered (matches official wecom-openclaw-plugin behaviour). No fallback send.", s.Host.Name(), normalized))
		return obj("errcode", int64(0), "errmsg", "ack_timeout_assumed_delivered", "ack_pending", true), nil
	case <-ctx.Done():
		frame.future.finish(nil, nil, true)
		return nil, ctx.Err()
	}
}

// Waits a bounded time for the pending intermediate ack without cancelling it, then clears it
func (s *Streamer) drainPendingAck(ctx context.Context, queue *ReplyQueue, reqID string) {
	s.mu.Lock()
	pending := queue.PendingAck
	s.mu.Unlock()
	id, finish := streamDesc(pending.Body)
	slog.Debug(fmt.Sprintf("[%s] _send_reply_queued: final waiting for pending ack drain — req_id=%s pending_stream_id=%s pending_finish=%s pending_sent_at=%.1fs_ago",
		s.Host.Name(), reqID, id, finish, elapsed(pending.SentAt)))
	timer := time.NewTimer(replyAckTimeout)
	defer timer.Stop()
	select {
	case <-pending.future.done:
	case <-ctx.Done():
	case <-timer.C:
		slog.Warn(fmt.Sprintf("[%s] Reply ack timeout waiting for pending (req_id=%s) — pending_stream_id=%s pending_finish=%s elapsed=%.1fs. Possible causes: ack cmd filtered, ack req_id mismatch, or WeCom did not ack.",
			s.Host.Name(), reqID, id, finish, elapsed(pending.SentAt)))
	}
	s.mu.Lock()
	queue.PendingAck = nil
	s.mu.Unlock()
}

// ResolveReplyAck completes the pending ack of a req_id and reports whether one was pending
func (s *Streamer) ResolveReplyAck(reqID string, payload *pyjson.Object) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	queue, ok := s.replyQueues[reqID]
	if !ok || queue.PendingAck == nil {
		return false
	}
	frame := queue.PendingAck
	if !frame.future.isDone() {
		body, _ := getOr(payload, "body", nil).(*pyjson.Object)
		if body == nil {
			body = pyjson.NewObject()
		}
		slog.Debug(fmt.Sprintf("[%s] _resolve_reply_ack: resolved req_id=%s is_final=%s elapsed=%.2fs errcode=%s",
			s.Host.Name(), reqID, pyrepr.Str(frame.IsFinal), elapsed(frame.SentAt), pyrepr.Str(getOr(body, "errcode", "N/A"))))
		frame.future.finish(payload, nil, false)
	}
	s.releasePending(queue, reqID, frame)
	return true
}

// FailReplyQueues fails every pending ack with the error and forgets all queues
func (s *Streamer) FailReplyQueues(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.replyQueues {
		if q.PendingAck != nil && !q.PendingAck.future.isDone() {
			q.PendingAck.future.finish(nil, err, false)
		}
	}
	s.replyQueues = map[string]*ReplyQueue{}
}

func (s *Streamer) resolveStreamReqID(chatID, replyTo string) string {
	if id := s.Host.ReplyReqIDForMessage(replyTo); id != "" {
		return id
	}
	return s.Host.LastChatReqID(pystr.Strip(chatID))
}

func cancelKeepalive(turn *StreamTurn) {
	handle := turn.keepalive
	turn.keepalive = nil
	if handle != nil {
		handle.Stop()
	}
}

func turnKey(chat, turnID, reqID string) string {
	if turnID == "" {
		turnID = reqID
	}
	return chat + ":" + turnID
}

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

func (s *Streamer) putTurn(key string, turn *StreamTurn) {
	if _, ok := s.turns[key]; !ok {
		s.turnOrder = append(s.turnOrder, key)
	}
	s.turns[key] = turn
}

// Cancels the keepalive and drops the turn from the registry, caller holds the lock
func (s *Streamer) retireTurn(turn *StreamTurn, turnID string) {
	cancelKeepalive(turn)
	s.popTurn(turnKey(turn.ChatID, turnID, turn.ReqID))
}

func (s *Streamer) expireTurn(turn *StreamTurn, turnID string) {
	turn.Expired = true
	s.retireTurn(turn, turnID)
	s.expiredChats[turn.ChatID] = true
}

func (s *Streamer) findActiveTurnForChat(chatID string) *StreamTurn {
	for _, k := range s.turnOrder {
		if t := s.turns[k]; t.ChatID == chatID && !t.Finalized {
			return t
		}
	}
	return nil
}

// Arms the keepalive timer once when enabled and the turn is live, caller holds the lock
func (s *Streamer) armKeepalive(turn *StreamTurn, turnID string) {
	if !s.KeepaliveEnabled || turn.Finalized || turn.Expired || turn.keepalive != nil {
		return
	}
	turn.keepalive = time.AfterFunc(time.Duration(s.KeepaliveIntervalSeconds*float64(time.Second)), func() { s.onKeepaliveFire(turn, turnID) })
}

func (s *Streamer) onKeepaliveFire(turn *StreamTurn, turnID string) {
	s.mu.Lock()
	turn.keepalive = nil
	live := !(turn.Finalized || turn.Expired)
	s.mu.Unlock()
	if live {
		go s.keepaliveSend(context.Background(), turn, turnID)
	}
}

// Resends the accumulated text as finish false to refresh the window, retiring the turn on expiry
func (s *Streamer) keepaliveSend(ctx context.Context, turn *StreamTurn, turnID string) {
	s.mu.Lock()
	if turn.Finalized || turn.Expired || turn.intermediateFramesSent >= MaxIntermediateFrames {
		s.mu.Unlock()
		return
	}
	content := turn.AccumulatedText
	if pystr.Strip(content) == "" {
		s.armKeepalive(turn, turnID)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	_, err := s.sendStreamReply(ctx, turn.ReqID, turn.StreamID, content, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if expired, ok := err.(*WeComStreamExpiredError); ok && expired != nil {
		s.expireTurn(turn, turnID)
		return
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] keep-alive send failed (chat=%s, turn=%s): %v", s.Host.Name(), turn.ChatID, turn.StreamID, err))
		s.armKeepalive(turn, turnID)
		return
	}
	turn.LastSentContent = content
	s.armKeepalive(turn, turnID)
}

// Truncates to the UTF-8 byte limit dropping a split trailing character
func truncateStreamContent(content string, limit int) (string, error) {
	if err := pystr.StrictUTF8EncodeError(content); err != nil {
		return "", err
	}
	if len(content) <= limit {
		return content, nil
	}
	return strings.ToValidUTF8(content[:limit], ""), nil
}

// Sends one stream frame, intermediates non blocking and the final awaiting its ack to detect expiry
func (s *Streamer) sendStreamReply(ctx context.Context, reqID, streamID, content string, finish bool) (*pyjson.Object, error) {
	truncated, err := truncateStreamContent(content, MaxStreamContentLength)
	if err != nil {
		return nil, err
	}
	if pystr.Len(content) != pystr.Len(truncated) {
		slog.Warn(fmt.Sprintf("[%s] Stream content truncated for stream_id=%s", s.Host.Name(), streamID))
	}
	body := obj("msgtype", "stream", "stream", obj("id", streamID, "finish", finish, "content", truncated))
	if !finish {
		return s.sendReplyQueued(ctx, reqID, body, false, true)
	}
	response, err := s.sendReplyQueued(ctx, reqID, body, true, false)
	if err != nil {
		return nil, err
	}
	errcode := getOr(response, "errcode", int64(0))
	if pyjson.Equal(errcode, int64(StreamExpiredErrcode)) || pyjson.Equal(errcode, int64(StreamRequestExpiredErrcode)) {
		errmsg := getOr(response, "errmsg", nil)
		msg := ""
		if pyrepr.Truthy(errmsg) {
			msg = pyrepr.Str(errmsg)
		}
		return nil, &WeComStreamExpiredError{Errcode: errcode, Errmsg: msg}
	}
	if pyjson.Equal(errcode, int64(StreamVersionConflictErrcode)) {
		slog.Info(fmt.Sprintf("[%s] finalize hit errcode 6000 (version conflict) — bubble already replaced by a newer frame; treating as delivered.", s.Host.Name()))
		return response, nil
	}
	if err := s.Host.RaiseForWeComError(response, "send stream reply"); err != nil {
		return nil, err
	}
	return response, nil
}

// SendStreamFrame seeds, updates or finalizes the turn keyed by turn id and reports false when the caller must fall back
func (s *Streamer) SendStreamFrame(ctx context.Context, text string, finalize bool, chatID, replyTo, turnID string) (bool, error) {
	chat := pystr.Strip(chatID)
	if chat == "" {
		slog.Warn(fmt.Sprintf("[%s] send_stream_frame: chat_id required", s.Host.Name()))
		return false, nil
	}
	s.mu.Lock()
	s.init()
	expired := s.expiredChats[chat]
	s.mu.Unlock()
	if turnID == "" && expired {
		return false, nil
	}
	if !finalize {
		return s.sendStreamFrameInner(ctx, text, chat, replyTo, finalize, turnID)
	}
	result, err := s.Host.EnqueueChatSend(ctx, chat, func(ctx context.Context) (any, error) {
		return s.sendStreamFrameInner(ctx, text, chat, replyTo, finalize, turnID)
	}, true)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(result), nil
}

// Finds or creates the turn, nil when unavailable, caller holds the lock
func (s *Streamer) locateTurn(chat, replyTo string, finalize bool, turnID string) *StreamTurn {
	name := s.Host.Name()
	if turnID != "" {
		if turn := s.turns[chat+":"+turnID]; turn != nil {
			return turn
		}
		if finalize {
			slog.Debug(fmt.Sprintf("[%s] send_stream_frame: cannot finalize non-existent turn (turn_id=%s, chat=%s)", name, turnID, chat))
			return nil
		}
	} else if existing := s.findActiveTurnForChat(chat); existing != nil {
		slog.Debug(fmt.Sprintf("[%s] send_stream_frame: reusing existing turn %s for chat %s", name, existing.StreamID, chat))
		return existing
	}
	suffix := ""
	if turnID != "" {
		suffix = " (turn_id=" + turnID + ")"
	}
	reqID := ""
	if !s.expiredChats[chat] {
		reqID = s.resolveStreamReqID(chat, replyTo)
	}
	if reqID == "" {
		if s.expiredChats[chat] {
			slog.Debug(fmt.Sprintf("[%s] send_stream_frame: chat %s is expired, cannot create new turn%s", name, chat, suffix))
		} else {
			slog.Debug(fmt.Sprintf("[%s] send_stream_frame: no req_id available for chat %s%s", name, chat, suffix))
		}
		return nil
	}
	key := turnKey(chat, turnID, reqID)
	var turn *StreamTurn
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
	slog.Debug(fmt.Sprintf("[%s] send_stream_frame: created new turn %s (%s) for chat %s", name, turn.StreamID, desc, chat))
	return turn
}

// Sends the finish frame or declines it through the clock fallback once the stream outlived its safe duration
func (s *Streamer) finalizeTurn(ctx context.Context, turn *StreamTurn, text, chat, turnID string) (bool, error) {
	s.mu.Lock()
	if !s.KeepaliveEnabled {
		if age := time.Since(turn.StartTime).Seconds(); age >= s.SafeDurationSeconds {
			slog.Info(fmt.Sprintf("[%s] Stream age %.0fs >= safe duration %.0fs for chat %s — declining finalize frame, falling back to proactive send (Layer 2 clock fallback).",
				s.Host.Name(), age, s.SafeDurationSeconds, chat))
			s.expireTurn(turn, turnID)
			s.mu.Unlock()
			return false, nil
		}
	}
	cancelKeepalive(turn)
	finalText := text
	if text != "" && text == turn.LastSentContent {
		finalText = text + "\u200b"
	}
	s.mu.Unlock()
	if _, err := s.sendStreamReply(ctx, turn.ReqID, turn.StreamID, finalText, true); err != nil {
		return false, err
	}
	s.mu.Lock()
	turn.Finalized = true
	s.popTurn(turnKey(chat, turnID, turn.ReqID))
	s.mu.Unlock()
	return true, nil
}

// Runs one frame of a turn, dropping failed intermediates and retiring the turn when a final frame fails
func (s *Streamer) sendStreamFrameInner(ctx context.Context, text, chat, replyTo string, finalize bool, turnID string) (bool, error) {
	var turn *StreamTurn
	err := func() error {
		s.mu.Lock()
		s.init()
		turn = s.locateTurn(chat, replyTo, finalize, turnID)
		if turn == nil || turn.Expired {
			s.mu.Unlock()
			return errDecline
		}
		seed := !turn.Seeded && !turn.Finalized
		s.mu.Unlock()
		if seed {
			if _, err := s.sendStreamReply(ctx, turn.ReqID, turn.StreamID, "<think></think>", false); err != nil {
				return err
			}
			s.mu.Lock()
			turn.Seeded = true
			s.armKeepalive(turn, turnID)
			s.mu.Unlock()
			if text == "" && !finalize {
				return nil
			}
		}
		if finalize {
			ok, err := s.finalizeTurn(ctx, turn, text, chat, turnID)
			if err == nil && !ok {
				return errDecline
			}
			return err
		}
		s.mu.Lock()
		turn.AccumulatedText = text
		if turn.intermediateFramesSent >= MaxIntermediateFrames || text == turn.LastSentContent {
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		if _, err := s.sendStreamReply(ctx, turn.ReqID, turn.StreamID, text, false); err != nil {
			return err
		}
		s.mu.Lock()
		turn.intermediateFramesSent++
		turn.LastSentContent = text
		s.mu.Unlock()
		return nil
	}()
	if err == nil {
		return true, nil
	}
	if err == errDecline {
		return false, nil
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return false, err
	}
	name := s.Host.Name()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := err.(*WeComStreamExpiredError); ok {
		if !finalize {
			slog.Info(fmt.Sprintf("[%s] Intermediate stream frame expired (errcode=%d) for chat %s — dropping frame, stream stays live", name, StreamExpiredErrcode, chat))
			return true, nil
		}
		slog.Info(fmt.Sprintf("[%s] Stream expired (errcode=%d) for chat %s — switching to proactive send", name, StreamExpiredErrcode, chat))
		if turn == nil {
			s.expiredChats[chat] = true
		} else {
			s.expireTurn(turn, turnID)
		}
		return false, nil
	}
	if !finalize {
		slog.Info(fmt.Sprintf("[%s] Intermediate stream frame failed (chat=%s): %v — dropping frame, stream stays live", name, chat, err))
		return true, nil
	}
	slog.Warn(fmt.Sprintf("[%s] Stream frame failed (chat=%s): %v", name, chat, err))
	if turn != nil {
		s.retireTurn(turn, turnID)
	}
	return false, nil
}

var errDecline = errors.New("stream unavailable")

// SupportsNativeStreaming is true in DMs and groups alike
func (s *Streamer) SupportsNativeStreaming(chatType string, metadata map[string]any) bool {
	return true
}

// SendTyping does nothing because the seed frame already shows typing
func (s *Streamer) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}
