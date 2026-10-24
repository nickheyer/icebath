package anim

import (
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Delivered once per animation frame with the seconds elapsed since the previous frame
type FrameMsg struct {
	At  time.Time
	DT  float64
	id  uint64
	gen uint64
}

// Schedules frames quickly while motion is active and slowly while the screen is at rest
type Clock struct {
	Active int
	Idle   int
	id     uint64
	gen    uint64
	last   time.Time
}

var clockIDs atomic.Uint64

// Builds a clock with the given active and idle frame rates, defaulting to 60 and 30
func NewClock(active, idle int) Clock {
	if active < 1 {
		active = 60
	}
	if idle < 1 {
		idle = 30
	}
	return Clock{Active: active, Idle: idle, id: clockIDs.Add(1)}
}

// Requests a frame immediately and invalidates any frame already pending
func (c *Clock) Wake() tea.Cmd { return c.schedule(time.Millisecond) }

// Schedules the following frame at the active or idle rate
func (c *Clock) Next(active bool) tea.Cmd {
	fps := c.Idle
	if active {
		fps = c.Active
	}
	return c.schedule(time.Second / time.Duration(fps))
}

// Stamps a frame with its delta time, rejecting frames from stale schedules or other clocks
func (c *Clock) Accept(msg FrameMsg) (FrameMsg, bool) {
	if msg.id != c.id || msg.gen != c.gen {
		return msg, false
	}
	dt := 1 / float64(c.Active)
	if !c.last.IsZero() {
		dt = msg.At.Sub(c.last).Seconds()
	}
	dt = min(max(dt, 1.0/240), 1.0/15)
	c.last = msg.At
	msg.DT = dt
	return msg, true
}

func (c *Clock) schedule(d time.Duration) tea.Cmd {
	c.gen++
	id, gen := c.id, c.gen
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return FrameMsg{At: t, id: id, gen: gen}
	})
}
