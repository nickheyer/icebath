package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Params a one shot generation accepts
var oneshotFields = []string{"session_id", "profile", "template", "instructions", "input", "variables", "task",
	"temperature", "max_tokens"}

// Text params of a one shot generation
var oneshotText = []string{"template", "instructions", "input", "task"}

// One request's pending answer, owned by the connection that asked and dropped once its socket closes
type oneshotAnswer struct {
	conn      *AuthorityConnection
	forgotten chan struct{}
	once      sync.Once
}

// Drops the answer, its socket closed
func (a *oneshotAnswer) forget() { a.once.Do(func() { close(a.forgotten) }) }

// Handler result answered later, dispatch handing over the request id and sending no frame
type DeferredReply struct {
	work func(ctx context.Context) (any, error)
}

// Deferred reply running work on the owner's one shot pool
func NewDeferredReply(work func(ctx context.Context) (any, error)) *DeferredReply {
	return &DeferredReply{work: work}
}

// Admits the call or refuses it and answers it later, check running on the result before the reply goes out
func (d *DeferredReply) Start(ctx context.Context, c *AuthorityConnection, rid any, check func(result any) error) error {
	pool := oneshotPoolOf(c.authority)
	scoped := context.WithoutCancel(ctx)
	call, err := pool.submit(func() (any, error) { return d.work(scoped) })
	if err != nil {
		return err
	}
	answer := &oneshotAnswer{conn: c, forgotten: make(chan struct{})}
	pool.lock.Lock()
	pool.answers[answer] = true
	pool.lock.Unlock()
	go func() {
		select {
		case <-call.done:
		case <-pool.stopping.Waiter():
		case <-answer.forgotten:
		}
		pool.loop.post(func() {
			forgotten := answerOneshot(pool, c.transport, rid, call, check, answer)
			pool.lock.Lock()
			delete(pool.answers, answer)
			pool.lock.Unlock()
			if forgotten {
				call.cancel()
			}
		})
	}()
	return nil
}

// Writes the request's one reply on the owner loop, true when its socket closed first and no frame went out
func answerOneshot(pool *oneshotPool, transport tuigateway.Transport, rid any, call *oneshotCall, check func(result any) error, answer *oneshotAnswer) bool {
	select {
	case <-answer.forgotten:
		return true
	default:
	}
	call.mu.Lock()
	settled, cancelled, result, failure := false, call.cancelled, call.result, call.err
	select {
	case <-call.done:
		settled = true
	default:
	}
	call.mu.Unlock()
	var frame *pyjson.Object
	if pool.stopping.IsSet() || !settled || cancelled {
		call.cancel()
		frame = pyjson.ObjectOf("jsonrpc", "2.0", "id", rid, "error", pyjson.ObjectOf("code", 4001,
			"message", "runtime_draining", "data", pyjson.ObjectOf("reason", "runtime_draining")))
	} else {
		if failure == nil && check != nil {
			failure = check(result)
		}
		if failure == nil {
			frame = pyjson.ObjectOf("jsonrpc", "2.0", "id", rid, "result", result)
		} else {
			slog.Warn(fmt.Sprintf("llm.oneshot failed: %s", pyrepr.ExceptionMessage(failure)), "error", failure)
			frame = pyjson.ObjectOf("jsonrpc", "2.0", "id", rid, "error", pyjson.ObjectOf("code", 5030,
				"message", "one-shot generation failed: "+pyrepr.ExceptionMessage(failure)))
		}
	}
	transport.Write(frame)
	return false
}

// Drops the closed socket's queued calls and answers, a running call finishing and its reply discarded
func forgetOneshots(c *AuthorityConnection) {
	c.authority.mu.Lock()
	pool := c.authority.oneshotPool
	c.authority.mu.Unlock()
	if pool == nil {
		return
	}
	pool.lock.Lock()
	owned := []*oneshotAnswer{}
	for answer := range pool.answers {
		if answer.conn == c {
			owned = append(owned, answer)
		}
	}
	pool.lock.Unlock()
	for _, answer := range owned {
		answer.forget()
	}
}

// Refuses one shot params outside the contract, an unknown template, or nothing to generate from
func validateOneshot(params *pyjson.Object) error {
	invalid := paramsOutside(params, oneshotFields...)
	for _, key := range append(append([]string{}, oneshotText...), "session_id") {
		if value, present := params.Get(key); present && value != nil {
			if _, isStr := value.(string); !isStr {
				invalid = true
			}
		}
	}
	if variables := params.GetOr("variables", nil); pyrepr.Truthy(variables) {
		if _, isDict := variables.(*pyjson.Object); !isDict {
			invalid = true
		}
	}
	for _, key := range []string{"temperature", "max_tokens"} {
		if value, present := params.Get(key); present && value != nil {
			switch value.(type) {
			case bool:
				invalid = true
			case int, int64, float64, *big.Int:
			default:
				invalid = true
			}
		}
	}
	if invalid {
		return runtimeRefusal("invalid_params")
	}
	text := func(key string) string {
		value, _ := params.GetOr(key, "").(string)
		return strings.TrimSpace(value)
	}
	if template := text("template"); template != "" && !OneshotAgent.TemplateKnown(template) {
		field, detail := "template", "unknown one-shot template"
		return state.NewRuntimeStoreError("invalid_params", &field, &detail)
	}
	if text("template") == "" && text("instructions") == "" && text("input") == "" {
		field, detail := "input", "needs a template or instructions/input"
		return state.NewRuntimeStoreError("invalid_params", &field, &detail)
	}
	return nil
}

// One shot generation reads from pkg/agent, assigned in cmd/icebath/wiring.go
var OneshotAgent struct {
	TemplateKnown func(name string) bool
}
