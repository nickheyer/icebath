package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/setupflow"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Setup flows the terminal UI can run on the gateway, each asking through the asker it is given
var setupFlows = map[string]func(ctx context.Context, h string, params map[string]string, ask setupflow.Asker) error{}

// Idle time after which a flow nobody answers is cancelled
var setupFlowIdle = 30 * time.Minute

// Answer the terminal UI gave to the open question
type setupFlowAnswer struct {
	index   int
	indices []int
	text    string
	cancel  bool
}

// One running flow, its printed lines, open question with the reply it waits on, end state and starting transport
type setupFlowSession struct {
	mu       sync.Mutex
	lines    []any
	question map[string]any
	reply    chan setupFlowAnswer
	asked    int
	changed  chan struct{}
	done     bool
	ok       bool
	failure  string
	ctx      context.Context
	cancel   context.CancelFunc
	owner    tuigateway.Transport
}

var (
	setupFlowsMu     sync.Mutex
	setupFlowRuns    = map[string]*setupFlowSession{}
	setupFlowCounter int
	setupFlowRunMu   sync.Mutex
)

var setupFlowRegistry = &handlerRegistry{}

func init() {
	setupFlowRegistry.scopedMethod("setup.flow.start", catch(5044, setupFlowStart))
	setupFlowRegistry.scopedMethod("setup.flow.step", catch(5045, setupFlowStep))
	setupFlowRegistry.install()
	addLongHandlers("setup.flow.step")
}

// Wakes a step waiting on the session
func (s *setupFlowSession) poke() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *setupFlowSession) Say(line string) {
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
	s.poke()
}

// Publishes a question and waits for its answer, a released or idle session ending as cancelled
func (s *setupFlowSession) ask(question map[string]any) (setupFlowAnswer, error) {
	reply := make(chan setupFlowAnswer, 1)
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return setupFlowAnswer{}, setupflow.ErrCancelled
	}
	s.asked++
	question["id"] = s.asked
	s.question, s.reply = question, reply
	s.mu.Unlock()
	s.poke()
	defer func() {
		s.mu.Lock()
		if s.reply == reply {
			s.question, s.reply = nil, nil
		}
		s.mu.Unlock()
	}()
	idle := time.NewTimer(setupFlowIdle)
	defer idle.Stop()
	select {
	case answer := <-reply:
		return answer, nil
	case <-s.ctx.Done():
		return setupFlowAnswer{}, setupflow.ErrCancelled
	case <-idle.C:
		s.cancel()
		return setupFlowAnswer{}, setupflow.ErrCancelled
	}
}

func (s *setupFlowSession) Choose(question string, options []setupflow.Option, def int) (int, error) {
	rows := make([]any, len(options))
	for i, o := range options {
		rows[i] = map[string]any{"title": o.Title, "detail": o.Detail}
	}
	answer, err := s.ask(map[string]any{"kind": "choose", "question": question, "options": rows, "default": def})
	if err != nil {
		return 0, err
	}
	if answer.cancel || answer.index < 0 || answer.index >= len(options) {
		return def, nil
	}
	return answer.index, nil
}

func (s *setupFlowSession) Check(question string, items []setupflow.Item, selected map[int]bool, tokens map[string]int) (map[int]bool, error) {
	rows := make([]any, len(items))
	for i, item := range items {
		tools := make([]any, len(item.Tools))
		for j, name := range item.Tools {
			tools[j] = name
		}
		rows[i] = map[string]any{"title": item.Title, "detail": item.Detail, "tools": tools}
	}
	chosen := []any{}
	for i := range items {
		if selected[i] {
			chosen = append(chosen, i)
		}
	}
	weights := map[string]any{}
	for name, n := range tokens {
		weights[name] = n
	}
	answer, err := s.ask(map[string]any{"kind": "check", "question": question, "items": rows, "selected": chosen, "tokens": weights})
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	if answer.cancel {
		for i := range selected {
			out[i] = selected[i]
		}
		return out, nil
	}
	for _, i := range answer.indices {
		if i >= 0 && i < len(items) {
			out[i] = true
		}
	}
	return out, nil
}

func (s *setupFlowSession) Text(question, def string, secret bool) (string, error) {
	answer, err := s.ask(map[string]any{"kind": "text", "question": question, "default": def, "secret": secret})
	if err != nil {
		return "", err
	}
	if answer.cancel {
		return "", setupflow.ErrCancelled
	}
	if answer.text == "" {
		return def, nil
	}
	return answer.text, nil
}

// Ends the session with the outcome of its flow
func (s *setupFlowSession) finish(err error) {
	s.mu.Lock()
	s.done, s.ok = true, err == nil
	if err != nil {
		s.failure = err.Error()
	}
	s.question, s.reply = nil, nil
	s.mu.Unlock()
	s.poke()
}

// Starts a named flow in the background, one at a time, and returns the session that follows it
func setupFlowStart(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	h := home.Dir(ctx)
	name := strOr(params["flow"])
	flow, known := setupFlows[name]
	if !known {
		return rpcErr(rid, 4001, "unknown setup flow "+name, nil), nil
	}
	if config.IsManaged(ctx, h) {
		return rpcErr(rid, 4006, "managed install — the configuration is read-only", nil), nil
	}
	flowCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	session := &setupFlowSession{changed: make(chan struct{}, 1), ctx: flowCtx, cancel: cancel, owner: tuigateway.CurrentTransport(ctx)}
	setupFlowsMu.Lock()
	setupFlowCounter++
	id := "flow-" + strconv.Itoa(setupFlowCounter)
	setupFlowRuns[id] = session
	setupFlowsMu.Unlock()
	values := setupParamText(params["params"])
	go func() {
		setupFlowRunMu.Lock()
		defer setupFlowRunMu.Unlock()
		var err error
		defer func() {
			if failure := recover(); failure != nil {
				err = fmt.Errorf("%v", failure)
			}
			session.finish(err)
		}()
		if flowCtx.Err() != nil {
			err = setupflow.ErrCancelled
			return
		}
		err = flow(flowCtx, h, values, session)
	}()
	return ok(rid, map[string]any{"session": id}), nil
}

// Delivers an answer, then waits a short while for output, the next question or the end, forgetting a finished session
func setupFlowStep(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	id := strOr(params["session"])
	setupFlowsMu.Lock()
	session, known := setupFlowRuns[id]
	setupFlowsMu.Unlock()
	if !known {
		return rpcErr(rid, 4001, "unknown setup flow session "+id, nil), nil
	}
	if raw, answered := params["answer"].(map[string]any); answered {
		answer := setupFlowAnswer{text: strOr(raw["text"]), cancel: raw["cancel"] == true, index: -1}
		if index, fits := sectionWriteValue("whole", raw["index"]); fits {
			answer.index = index.(int)
		}
		for _, v := range wireList(raw["indices"]) {
			if index, fits := sectionWriteValue("whole", v); fits {
				answer.indices = append(answer.indices, index.(int))
			}
		}
		asked, _ := sectionWriteValue("whole", raw["question"])
		session.mu.Lock()
		reply := session.reply
		open := session.question != nil && reply != nil && asked == session.asked
		if open {
			session.question, session.reply = nil, nil
		}
		session.mu.Unlock()
		if !open {
			return rpcErr(rid, 4001, "the setup flow is not waiting on that question", nil), nil
		}
		reply <- answer
	}
	deadline := time.After(2 * time.Second)
	for {
		session.mu.Lock()
		lines, question, done := session.lines, session.question, session.done
		if len(lines) > 0 || question != nil || done {
			session.lines = nil
			result := map[string]any{"lines": lines, "question": question, "done": done, "ok": session.ok, "error": session.failure}
			session.mu.Unlock()
			if lines == nil {
				result["lines"] = []any{}
			}
			if done {
				setupFlowsMu.Lock()
				delete(setupFlowRuns, id)
				setupFlowsMu.Unlock()
			}
			return ok(rid, result), nil
		}
		session.mu.Unlock()
		select {
		case <-session.changed:
		case <-deadline:
			return ok(rid, map[string]any{"lines": []any{}, "question": nil, "done": false, "ok": false, "error": ""}), nil
		case <-ctx.Done():
			return nil, errors.New("setup flow step interrupted")
		}
	}
}
