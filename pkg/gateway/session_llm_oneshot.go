package gateway

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

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
func answerOneshot(pool *oneshotPool, transport tuihost.Transport, rid any, call *oneshotCall, check func(result any) error, answer *oneshotAnswer) bool {
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
			pkgLog().Warn(fmt.Sprintf("llm.oneshot failed: %s", pyrepr.ExceptionMessage(failure)), "error", failure)
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

// One shot generation reads from pkg/agent, assigned in cmd/icebath/wiring.go
var OneshotAgent struct {
	TemplateKnown func(name string) bool
}
