package failover

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

type fake struct {
	err    error
	text   string
	midway bool
	calls  int
}

func (f *fake) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	f.calls++
	if f.err != nil {
		return protocol.Response{}, f.err
	}
	return protocol.Response{Content: []protocol.Part{protocol.Text{Text: f.text}}}, nil
}

func (f *fake) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		f.calls++
		if f.err != nil && !f.midway {
			yield(protocol.Event{}, f.err)
			return
		}
		if f.text == "" {
			return
		}
		if !yield(protocol.Event{Type: protocol.EventStart}, nil) {
			return
		}
		if f.midway {
			yield(protocol.Event{}, f.err)
			return
		}
		yield(protocol.Event{Type: protocol.EventDone, Response: protocol.Response{Content: []protocol.Part{protocol.Text{Text: f.text}}}}, nil)
	}
}

func TestCompleteFallsOverOnRetryableFailures(t *testing.T) {
	ctx := context.Background()
	down := &fake{err: &protocol.Error{Status: 503}}
	up := &fake{text: "ok"}
	var failed []int
	var served []int
	c := Chain{Models: []Model{down, up}, OnFail: func(i int, err error) { failed = append(failed, i) }, OnServe: func(i int) { served = append(served, i) }}
	resp, err := c.Complete(ctx, protocol.Request{})
	if err != nil || resp.Content[0].(protocol.Text).Text != "ok" || len(failed) != 1 || failed[0] != 0 {
		t.Fatalf("%v %+v %v", err, resp, failed)
	}
	if len(served) != 1 || served[0] != 1 {
		t.Fatalf("the model that answered must be reported, got %v", served)
	}
	bad := &fake{err: &protocol.Error{Status: 400, Message: "bad request"}}
	c = Chain{Models: []Model{bad, up}}
	up.calls = 0
	if _, err := c.Complete(ctx, protocol.Request{}); err == nil || !strings.Contains(err.Error(), "bad request") || up.calls != 0 {
		t.Fatalf("client errors must not fail over: %v %d", err, up.calls)
	}
	both := Chain{Models: []Model{down, &fake{err: errors.New("dial tcp")}}}
	if _, err := both.Complete(ctx, protocol.Request{}); err == nil || !strings.Contains(err.Error(), "model 0") || !strings.Contains(err.Error(), "model 1: dial tcp") {
		t.Fatalf("%v", err)
	}
	if _, err := (Chain{}).Complete(ctx, protocol.Request{}); err == nil {
		t.Fatal("empty chain completed")
	}
}

func TestStreamFallsOverOnlyBeforeEvents(t *testing.T) {
	ctx := context.Background()
	down := &fake{err: &protocol.Error{Status: 529}}
	silent := &fake{}
	up := &fake{text: "ok"}
	var served []int
	c := Chain{Models: []Model{down, silent, up}, OnServe: func(i int) { served = append(served, i) }}
	resp, err := protocol.Collect(c.Stream(ctx, protocol.Request{}))
	if err != nil || resp.Content[0].(protocol.Text).Text != "ok" || silent.calls != 1 {
		t.Fatalf("%v %+v", err, resp)
	}
	if len(served) != 1 || served[0] != 2 {
		t.Fatalf("stream must report the model whose events were served once, got %v", served)
	}
	midway := &fake{text: "partial", midway: true, err: errors.New("connection reset")}
	up.calls = 0
	c = Chain{Models: []Model{midway, up}}
	if _, err := protocol.Collect(c.Stream(ctx, protocol.Request{})); err == nil || !strings.Contains(err.Error(), "connection reset") || up.calls != 0 {
		t.Fatalf("failures after events must surface: %v %d", err, up.calls)
	}
	bad := &fake{err: &protocol.Error{Status: 401}}
	c = Chain{Models: []Model{bad, up}}
	if _, err := protocol.Collect(c.Stream(ctx, protocol.Request{})); err == nil || up.calls != 0 {
		t.Fatalf("%v %d", err, up.calls)
	}
	if _, err := protocol.Collect((Chain{}).Stream(ctx, protocol.Request{})); err == nil {
		t.Fatal("empty chain streamed")
	}
	if _, err := protocol.Collect(Chain{Models: []Model{silent}}.Stream(ctx, protocol.Request{})); err == nil || !strings.Contains(err.Error(), "without events") {
		t.Fatalf("%v", err)
	}
	stopped := 0
	for range (Chain{Models: []Model{up}}).Stream(ctx, protocol.Request{}) {
		stopped++
		break
	}
	if stopped != 1 {
		t.Fatal("early break must stop the stream")
	}
}
