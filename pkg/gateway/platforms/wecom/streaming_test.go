package wecom

import (
	"context"
	"sync"
	"testing"
	"time"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

type fakeStreamHost struct {
	mu      sync.Mutex
	sent    []*jsonutil.Object
	onSend  func(reqID string, body *jsonutil.Object)
	lastReq map[string]string
}

func (h *fakeStreamHost) Name() string                               { return "WeCom" }
func (h *fakeStreamHost) RequireWS() error                           { return nil }
func (h *fakeStreamHost) RequireReplyReqID(r string) (string, error) { return r, nil }
func (h *fakeStreamHost) ReplyReqIDForMessage(string) string         { return "" }
func (h *fakeStreamHost) LastChatReqID(chat string) string           { return h.lastReq[chat] }
func (h *fakeStreamHost) RaiseForWeComError(*jsonutil.Object, string) error {
	return nil
}
func (h *fakeStreamHost) EnqueueChatSend(ctx context.Context, _ string, send func(context.Context) (any, error), _ bool) (any, error) {
	return send(ctx)
}
func (h *fakeStreamHost) SendJSON(_ context.Context, payload *jsonutil.Object) error {
	h.mu.Lock()
	h.sent = append(h.sent, payload)
	h.mu.Unlock()
	if h.onSend != nil {
		headers, _ := payload.Get("headers")
		reqID, _ := headers.(*jsonutil.Object).Get("req_id")
		body, _ := payload.Get("body")
		h.onSend(reqID.(string), body.(*jsonutil.Object))
	}
	return nil
}

func frameText(p *jsonutil.Object) (string, bool) {
	body, _ := p.Get("body")
	stream, _ := body.(*jsonutil.Object).Get("stream")
	content, _ := stream.(*jsonutil.Object).Get("content")
	finish, _ := stream.(*jsonutil.Object).Get("finish")
	return content.(string), finish.(bool)
}

func TestStreamTurnSeedUpdateFinalize(t *testing.T) {
	host := &fakeStreamHost{lastReq: map[string]string{"chat1": "req-1"}}
	s := &Streamer{Host: host, SafeDurationSeconds: StreamSafeDurationSeconds}
	host.onSend = func(reqID string, body *jsonutil.Object) {
		ack := jsonutil.NewObject()
		ack.Set("errcode", int64(0))
		s.ResolveReplyAck(reqID, ack)
	}
	ctx := context.Background()
	for _, step := range []struct {
		text     string
		finalize bool
	}{{"", false}, {"hello", false}, {"hello", false}, {"hello", true}} {
		ok, err := s.SendStreamFrame(ctx, step.text, step.finalize, " chat1 ", "", "t1")
		if err != nil || !ok {
			t.Fatalf("frame %q: %v %v", step.text, ok, err)
		}
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	var texts []string
	for _, p := range host.sent {
		text, _ := frameText(p)
		texts = append(texts, text)
	}
	if len(texts) != 3 || texts[0] != "<think></think>" || texts[1] != "hello" || texts[2] != "hello\u200b" {
		t.Fatalf("frames %q", texts)
	}
	if _, finish := frameText(host.sent[2]); !finish || len(s.turns) != 0 {
		t.Fatalf("final frame finish=%v turns=%d", finish, len(s.turns))
	}
}

func TestStreamFinalExpiredFallsBackAndMarksChat(t *testing.T) {
	host := &fakeStreamHost{lastReq: map[string]string{"c": "r"}}
	s := &Streamer{Host: host, SafeDurationSeconds: StreamSafeDurationSeconds}
	host.onSend = func(reqID string, body *jsonutil.Object) {
		ack := jsonutil.NewObject()
		stream, _ := body.Get("stream")
		if finish, _ := stream.(*jsonutil.Object).Get("finish"); finish == true {
			ack.Set("errcode", int64(StreamExpiredErrcode))
		}
		s.ResolveReplyAck(reqID, ack)
	}
	ctx := context.Background()
	if ok, _ := s.SendStreamFrame(ctx, "partial", false, "c", "", ""); !ok {
		t.Fatal("intermediate refused")
	}
	if ok, _ := s.SendStreamFrame(ctx, "done", true, "c", "", ""); ok {
		t.Fatal("expired finalize reported delivered")
	}
	if ok, _ := s.SendStreamFrame(ctx, "again", false, "c", "", ""); ok || !s.expiredChats["c"] {
		t.Fatal("expired chat accepted a new turn")
	}
}

func TestReplyQueueSkipsWhilePendingAndTimesOutFinal(t *testing.T) {
	orig := replyAckTimeout
	replyAckTimeout = 20 * time.Millisecond
	defer func() { replyAckTimeout = orig }()
	s := &Streamer{Host: &fakeStreamHost{}}
	ctx := context.Background()
	body := obj("msgtype", "stream", "stream", obj("id", "s", "finish", false, "content", "x"))
	if r, _ := s.sendReplyQueued(ctx, "q", body, false, true); r.Value("errmsg") != "sent_nonblocking" {
		t.Fatalf("first %v", r)
	}
	if r, _ := s.sendReplyQueued(ctx, "q", body, false, true); r.Value("errmsg") != "pending_ack" {
		t.Fatalf("second %v", r)
	}
	if r, _ := s.sendReplyQueued(ctx, "q", body, true, false); r.Value("errmsg") != "ack_timeout_assumed_delivered" || len(s.replyQueues) != 0 {
		t.Fatalf("final %v queues %d", r, len(s.replyQueues))
	}
	if got := truncateStreamContent("aé", 2); got != "a" {
		t.Fatalf("truncate %q", got)
	}
}
