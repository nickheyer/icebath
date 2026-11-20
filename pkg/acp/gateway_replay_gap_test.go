package acp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func terminalReceipt(t testing.TB, admission, reply string, reused bool) *pyjson.Object {
	result := pyjson.ObjectOf("final_response", reply, "completed", true)
	if reused {
		result.Set("response_reused", true)
	}
	return pyjson.ObjectOf("admission_id", admission, "status", "terminal", "outcome", "completed", "result", result)
}

func TestCompletionInDeliveryBeatsARecoveryError(t *testing.T) {
	delivering, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r := newRig(t, func(r *gatewayRig, ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "prompt.submit":
			r.link.put(eventFrame(t, "session.replay_gap", `{}`, "s", "", 0))
			until(t, "the gap re-attach", func() bool { return r.resumed("s") })
			r.link.put(eventFrame(t, "message.complete", `{"text": "done", "outcome": "completed"}`, "s", "a", 1))
			<-delivering
			return jsonObj(t, `{"admission_id": "a", "status": "queued"}`), nil
		case "session.resume":
			return resumedSnapshot(t, ""), nil
		}
		return nil, &gateway.GatewayClientError{Msg: "read_failed"}
	})
	r.editor.onUpdate = func(string, pyacp.SessionUpdate) error {
		once.Do(func() { close(delivering) })
		<-release
		return nil
	}
	task := r.ask("s")
	select {
	case <-delivering:
	case <-time.After(5 * time.Second):
		t.Fatal("the completion was not delivered")
	}
	until(t, "the ack", func() bool { return task.finished() || r.admission("s") == "a" })
	close(release)
	resp, err := task.wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}

func TestOneRecoveryReadSettlesACancellationAndThePumpSurvives(t *testing.T) {
	attaching := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var reads []string
	r := newRig(t, func(r *gatewayRig, ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch {
		case method == "prompt.submit" && params["session_id"] == "t":
			return jsonObj(t, `{"admission_id": "t-a", "status": "queued"}`), nil
		case method == "prompt.submit":
			r.link.put(eventFrame(t, "session.replay_gap", `{}`, "s", "", 0))
			<-attaching
			return jsonObj(t, `{"admission_id": "a", "status": "queued"}`), nil
		case method == "session.resume":
			once.Do(func() { close(attaching) })
			until(t, "the ack", func() bool { return r.admission("s") == "a" })
			return resumedSnapshot(t, ""), nil
		}
		mu.Lock()
		reads = append(reads, pyStr(params["admission_id"]))
		count := len(reads)
		mu.Unlock()
		if count > 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return jsonObj(t, `{"admission_id": "a", "status": "terminal", "outcome": "cancelled"}`), nil
	})
	other := r.ask("t")
	until(t, "the other admission", func() bool { return r.admission("t") == "t-a" })
	first := r.ask("s")
	resp, err := first.wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "cancelled" {
		t.Fatalf("first = %+v, %v", resp, err)
	}
	r.link.put(eventFrame(t, "message.complete", `{"text": "other", "outcome": "completed"}`, "t", "t-a", 1))
	resp, err = other.wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("other = %+v, %v", resp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	failure := onLoop(r.agent, func() error { return r.agent.failure })
	if !reflect.DeepEqual(reads, []string{"a"}) || failure != nil {
		t.Fatalf("reads %v failure %v", reads, failure)
	}
}

func TestRetryAfterALostAckCompletesAPartialReusedReply(t *testing.T) {
	var mu sync.Mutex
	var submissions []string
	r := newRig(t, func(r *gatewayRig, ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "prompt.submit":
			mu.Lock()
			submissions = append(submissions, pyStr(params["input_id"]))
			count := len(submissions)
			mu.Unlock()
			if count == 1 {
				r.link.put(eventFrame(t, "message.delta", `{"text": "Hel"}`, "s", "a", 1))
				r.link.put(eventFrame(t, "session.replay_gap", `{}`, "s", "", 0))
				return nil, &gateway.GatewayClientError{Msg: "submit timed out"}
			}
			return jsonObj(t, `{"admission_id": "a", "status": "terminal", "outcome": "completed"}`), nil
		case "session.resume":
			return resumedSnapshot(t, ""), nil
		}
		return terminalReceipt(t, "a", "Hello", true), nil
	})
	_, err := r.ask("s").wait(t, 5*time.Second)
	var refused *gateway.GatewayClientError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v", err)
	}
	until(t, "the gap re-attach", func() bool { return r.resumed("s") })
	resp, err := r.ask("s").wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if submissions[0] != submissions[1] || shownText(r.editor, "s") != "Hello" {
		t.Fatalf("submissions %v shown %q", submissions, shownText(r.editor, "s"))
	}
}

func TestInterimAfterTheGapShowsTheReplyOnce(t *testing.T) {
	for _, completion := range []string{"", "ANSWER"} {
		t.Run("completion_"+completion, func(t *testing.T) {
			r := newRig(t, func(r *gatewayRig, ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				if method == "prompt.submit" {
					return jsonObj(t, `{"admission_id": "a", "status": "queued"}`), nil
				}
				return resumedSnapshot(t, `{"admission_id": "a", "status": "started"}`), nil
			})
			task := r.ask("s")
			until(t, "the ack", func() bool { return r.admission("s") == "a" })
			r.link.put(eventFrame(t, "message.delta", `{"text": "Let me "}`, "s", "a", 1))
			r.link.put(eventFrame(t, "session.replay_gap", `{}`, "s", "", 0))
			until(t, "the gap re-attach", func() bool { return r.resumed("s") })
			for i, kind := range []string{"message.interim", "message.complete"} {
				text := "ANSWER"
				if kind == "message.complete" {
					text = completion
				}
				payload := pyjson.ObjectOf("text", text, "already_streamed", false, "outcome", "completed")
				dumped, _ := pyjson.Dumps(payload)
				r.link.put(eventFrame(t, kind, dumped, "s", "a", i+1))
			}
			resp, err := task.wait(t, 2*time.Second)
			if err != nil || resp.StopReason != "end_turn" {
				t.Fatalf("resp = %+v, %v", resp, err)
			}
			if shown := shownText(r.editor, "s"); strings.Count(shown, "ANSWER") != 1 {
				t.Fatalf("shown = %q", shown)
			}
		})
	}
}

func TestWithoutAGapAStreamedReusedReplyIsNotRepainted(t *testing.T) {
	r := newRig(t, func(r *gatewayRig, ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		return jsonObj(t, `{"admission_id": "a", "status": "queued"}`), nil
	})
	task := r.ask("s")
	until(t, "the ack", func() bool { return r.admission("s") == "a" })
	frames := [][2]string{
		{"message.delta", `{"text": "FINAL"}`}, {"message.delta", `{"text": null}`},
		{"tool.start", `{"tool_call_id": "t1", "tool_name": "memory", "args": {}}`},
		{"tool.complete", `{"tool_call_id": "t1", "tool_name": "memory", "result": "ok"}`},
		{"message.complete", `{"text": "FINAL\nMEDIA:/tmp/x.png", "outcome": "completed", "response_reused": true}`},
	}
	for i, f := range frames {
		r.link.put(eventFrame(t, f[0], f[1], "s", "a", i+1))
	}
	resp, err := task.wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if shown := shownText(r.editor, "s"); strings.Count(shown, "FINAL") != 1 {
		t.Fatalf("shown = %q", shown)
	}
}
