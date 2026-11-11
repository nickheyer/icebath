package tuigateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Model that accepts every value, standing for the catalog entries of the topic declarations
type acceptingModel struct{}

func (acceptingModel) ModelValidate(any) *ValidationError { return nil }

type wireFrame struct{ event, id string }

// Records request frames and events in wire order with every session answerable
func wireSinks(t *testing.T) *[]wireFrame {
	t.Helper()
	for _, name := range []string{"sudo", "approval"} {
		contractsMu.RLock()
		_, declared := ServerRequests[name]
		contractsMu.RUnlock()
		if !declared {
			ServerRequestOf(name, acceptingModel{}, acceptingModel{}, "")
		}
	}
	var mu sync.Mutex
	frames := &[]wireFrame{}
	savedWrite, savedEmit, savedAnswerable, _ := currentSinks()
	BindSinks(func(frame *pyjson.Object) {
		mu.Lock()
		defer mu.Unlock()
		*frames = append(*frames, wireFrame{"request", frame.GetOr("id", "").(string)})
	}, func(event, sid string, payload map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		*frames = append(*frames, wireFrame{event, payload["id"].(string)})
	}, func(string) bool { return true }, nil)
	ResetForTests()
	t.Cleanup(func() {
		ResetForTests()
		BindSinks(savedWrite, savedEmit, savedAnswerable, nil)
	})
	return frames
}

func wireEvents(frames []wireFrame) []string {
	out := []string{}
	for _, f := range frames {
		out = append(out, f.event)
	}
	return out
}

func TestCancelRacingTheRequestWriteReachesTheWireAfterTheRequest(t *testing.T) {
	wire := wireSinks(t)
	writeRequest, emit, answerable, _ := currentSinks()
	cancelled := []int{}
	BindSinks(func(frame *pyjson.Object) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			sid := "s1"
			cancelled = append(cancelled, Cancel(&sid, "interrupted", nil))
		}()
		<-done
		writeRequest(frame)
	}, emit, answerable, nil)

	timeout := 10.0
	result, err := Send(context.Background(), "sudo", "s1", pyjson.NewObject(), &timeout, nil)
	if err != nil || result != nil {
		t.Fatalf("result = %v err = %v", result, err)
	}
	if len(cancelled) != 1 || cancelled[0] != 1 {
		t.Fatalf("cancelled = %v", cancelled)
	}
	events := wireEvents(*wire)
	if len(events) != 2 || events[0] != "request" || events[1] != "request.cancel" {
		t.Fatalf("wire = %v", *wire)
	}
	if (*wire)[0].id != (*wire)[1].id {
		t.Fatalf("ids differ: %v", *wire)
	}
	if open := OpenRequests("s1"); len(open) != 0 {
		t.Fatalf("open = %v", open)
	}
}

func TestCancelAfterTheWriteIsEmittedOnce(t *testing.T) {
	wire := wireSinks(t)
	pending, err := SendAsync("approval", "s1", pyjson.ObjectOf("request_id", "r1", "command", "rm", "description", ""), func(*pyjson.Object) {})
	if err != nil {
		t.Fatal(err)
	}
	sid := "s1"
	if n := Cancel(&sid, "interrupted", nil); n != 1 {
		t.Fatalf("cancelled = %d", n)
	}
	pending("resolved")
	events := wireEvents(*wire)
	if len(events) != 2 || events[0] != "request" || events[1] != "request.cancel" {
		t.Fatalf("wire = %v", *wire)
	}
}

// Compact JSON of an object for comparisons
func objText(t *testing.T, o *pyjson.Object) string {
	t.Helper()
	text, err := pyjson.Dumps(o)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestSettlementWinsOverALaterCancel(t *testing.T) {
	for _, c := range []struct {
		method   string
		qids     []string
		settle   func(req *ServerRequest) bool
		expected string
	}{
		{"sudo", nil, func(req *ServerRequest) bool {
			return ResolveResponse(pyjson.ObjectOf("id", req.ID, "result", pyjson.ObjectOf("value", "yes")), nil)
		}, `{"value": "yes"}`},
		{"clarify", []string{"q1"}, func(req *ServerRequest) bool {
			remaining, ok, err := LockAnswer(req.ID, "q1", "yes")
			return err == nil && ok && len(remaining) == 0
		}, `{"answers": {"q1": "yes"}, "outcome": "submitted"}`},
	} {
		wireSinks(t)
		req := newServerRequest("s1", c.method, pyjson.NewObject(), c.qids, nil)
		requestsMu.Lock()
		openRequests = append(openRequests, req)
		requestsMu.Unlock()
		if !c.settle(req) {
			t.Fatalf("%s: the settlement was refused", c.method)
		}
		sid := "s1"
		if n := Cancel(&sid, "interrupted", nil); n != 0 {
			t.Fatalf("%s: cancel withdrew %d", c.method, n)
		}
		if !req.answered || objText(t, req.result) != c.expected {
			t.Fatalf("%s: answered = %v result = %s", c.method, req.answered, objText(t, req.result))
		}
		select {
		case <-req.done:
		default:
			t.Fatalf("%s: the waiter was not released", c.method)
		}
	}
}

func TestTheRuleItself(t *testing.T) {
	locked := pyjson.ObjectOf("q0", "blue")
	kept := `{"answers": {"q0": "blue"}, "outcome": "cancelled"}`
	if got := objText(t, settleBatch(locked, nil, "cancelled")); got != kept {
		t.Fatalf("ended cancelled = %s", got)
	}
	if got := objText(t, settleBatch(locked, pyjson.NewObject(), "")); got != kept {
		t.Fatalf("empty frame = %s", got)
	}
	if got := objText(t, settleBatch(locked, nil, "")); got != kept {
		t.Fatalf("error frame = %s", got)
	}
	bogus := pyjson.ObjectOf("answers", pyjson.NewObject(), "outcome", "bogus")
	if got := objText(t, settleBatch(locked, bogus, "")); got != `{"answers": {"q0": "blue"}, "outcome": "submitted"}` {
		t.Fatalf("bogus outcome = %s", got)
	}
	if settleBatch(locked, nil, "timed_out").GetOr("answers", nil).(*pyjson.Object) == locked {
		t.Fatal("the locked answers were aliased into the result")
	}
}

func TestSendReturnsAnAnswerCommittedAfterTheDeadlineExpired(t *testing.T) {
	wireSinks(t)
	writeRequest, _, answerable, _ := currentSinks()
	var cancels []map[string]any
	BindSinks(writeRequest, func(_, _ string, payload map[string]any) { cancels = append(cancels, payload) }, answerable, nil)
	realWait := requestWait
	t.Cleanup(func() { requestWait = realWait })
	requestWait = func(ctx context.Context, done <-chan struct{}, deadline <-chan time.Time) bool {
		interrupted := realWait(ctx, done, deadline)
		requestsMu.Lock()
		rid := openRequests[0].ID
		requestsMu.Unlock()
		if !ResolveResponse(pyjson.ObjectOf("id", rid, "result", pyjson.ObjectOf("value", "yes")), nil) {
			t.Error("the late response was not routed")
		}
		return interrupted
	}
	timeout := 0.001
	result, err := Send(context.Background(), "sudo", "s1", pyjson.NewObject(), &timeout, nil)
	if err != nil || result == nil || objText(t, result) != `{"value": "yes"}` {
		t.Fatalf("result = %v err = %v", result, err)
	}
	if len(cancels) != 0 || OpenRequestCount() != 0 {
		t.Fatalf("cancels = %v open = %d", cancels, OpenRequestCount())
	}
}
