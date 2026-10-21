package harness

import (
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Frame budget deltas are held for before they are delivered together
const DefaultInterval = 16 * time.Millisecond

type deltaKey struct {
	run   string
	agent string
	round int
	index int
	typ   protocol.EventType
}

// Observer that forwards events as messages, merging streamed deltas so a fast model never floods the program
type Pump struct {
	send     Sender
	Interval time.Duration
	mu       sync.Mutex
	pending  []EventMsg
	index    map[deltaKey]int
	timer    *time.Timer
	Clock    func() time.Time
}

func NewPump(send Sender) *Pump {
	return &Pump{send: send, Interval: DefaultInterval, index: map[deltaKey]int{}, Clock: time.Now}
}

// Observer for one run, safe to call from any goroutine
func (p *Pump) Observer(run string) func(agent.Event) {
	return func(ev agent.Event) { p.Push(run, ev) }
}

func isDelta(ev agent.Event) bool {
	if ev.Type != agent.EventModel {
		return false
	}
	switch ev.Model.Type {
	case protocol.EventTextDelta, protocol.EventThinkingDelta, protocol.EventToolCallDelta, protocol.EventServerToolCallDelta:
		return true
	}
	return false
}

// Delivers an event, holding deltas until the frame ends and flushing them ahead of anything else
func (p *Pump) Push(run string, ev agent.Event) {
	if ev.Time.IsZero() {
		ev.Time = p.Clock()
	}
	msg := EventMsg{Run: run, Event: ev}
	if !isDelta(ev) {
		batch := p.take()
		for _, m := range batch {
			p.send(m)
		}
		p.send(msg)
		return
	}
	p.mu.Lock()
	key := deltaKey{run, ev.Agent, ev.Round, ev.Model.Index, ev.Model.Type}
	if i, ok := p.index[key]; ok {
		held := &p.pending[i]
		held.Event.Model.Delta += ev.Model.Delta
		held.Event.Model.Partial = ev.Model.Partial
		held.Event.Model.Response = ev.Model.Response
		held.Event.Seq = ev.Seq
		held.Event.Time = ev.Time
	} else {
		p.index[key] = len(p.pending)
		p.pending = append(p.pending, msg)
	}
	if p.timer == nil {
		interval := p.Interval
		if interval <= 0 {
			interval = DefaultInterval
		}
		p.timer = time.AfterFunc(interval, p.Flush)
	}
	p.mu.Unlock()
}

// Takes every held delta in arrival order and disarms the timer
func (p *Pump) take() []EventMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if len(p.pending) == 0 {
		return nil
	}
	out := p.pending
	p.pending = nil
	p.index = map[deltaKey]int{}
	return out
}

// Delivers every held delta now
func (p *Pump) Flush() {
	for _, m := range p.take() {
		p.send(m)
	}
}
