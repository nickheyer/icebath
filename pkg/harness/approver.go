package harness

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
)

// Error an ask returns once the approver was closed
var ErrClosed = errors.New("harness: approver closed")

// One approval waiting for a person
type Pending struct {
	ID      int
	Run     string
	Agent   string
	Request policy.Request
	Asked   time.Time
	answer  chan policy.Verdict
}

// Asker that posts each request into the program and waits for the answer the program hands back
type Approver struct {
	send Sender
	// Answer assumed when nobody answers in time, no wait when zero
	Timeout time.Duration
	mu      sync.Mutex
	next    int
	pending map[int]*Pending
	closed  bool
	Clock   func() time.Time
}

func NewApprover(send Sender) *Approver {
	return &Approver{send: send, pending: map[int]*Pending{}, Clock: time.Now}
}

// Posts the request and blocks until it is answered, the context ends or the timeout passes
func (a *Approver) Ask(ctx context.Context, req policy.Request) (policy.Verdict, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return policy.No, ErrClosed
	}
	a.next++
	p := &Pending{ID: a.next, Run: RunFrom(ctx), Agent: req.Agent, Request: req, Asked: a.Clock(), answer: make(chan policy.Verdict, 1)}
	a.pending[p.ID] = p
	a.mu.Unlock()
	a.send(ApprovalMsg{Pending: p})
	var timeout <-chan time.Time
	if a.Timeout > 0 {
		t := time.NewTimer(a.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	var v policy.Verdict
	var err error
	select {
	case v = <-p.answer:
	case <-ctx.Done():
		err = ctx.Err()
	case <-timeout:
		err = errors.New("harness: approval timed out")
	}
	a.mu.Lock()
	delete(a.pending, p.ID)
	a.mu.Unlock()
	a.send(ApprovalDoneMsg{ID: p.ID, Run: p.Run, Verdict: v, Err: err})
	if err != nil {
		return policy.No, err
	}
	return v, nil
}

// Delivers the answer to a pending approval and reports whether one was waiting
func (a *Approver) Answer(id int, v policy.Verdict) bool {
	a.mu.Lock()
	p, ok := a.pending[id]
	a.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case p.answer <- v:
		return true
	default:
		return false
	}
}

// Answers every pending approval with no
func (a *Approver) DenyAll() int {
	n := 0
	for _, p := range a.Pending() {
		if a.Answer(p.ID, policy.No) {
			n++
		}
	}
	return n
}

// Approvals waiting, oldest first
func (a *Approver) Pending() []*Pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Pending, 0, len(a.pending))
	for _, p := range a.pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *Approver) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// Refuses every later ask and denies the ones waiting
func (a *Approver) Close() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	a.DenyAll()
}

var _ policy.Asker = (*Approver)(nil)
