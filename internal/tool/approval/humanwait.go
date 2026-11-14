package approval

import (
	"context"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	humanWaitMaxSessions = 256
	humanWaitMargin      = 60 * time.Second
	heartbeatInterval    = 10 * time.Second
)

type waitState struct {
	pending   int
	started   time.Time
	open      bool
	completed time.Duration
}

func (g *Gate) ceiling(ctx context.Context) time.Duration {
	limit := time.Duration(approvalTimeout(ctx, g.o.Config))*time.Second + humanWaitMargin
	return min(limit, config.MaxSafeTimeout)
}

func (g *Gate) Ceiling() time.Duration { return g.ceiling(context.Background()) }

func clampedWindow(started, now time.Time, ceiling time.Duration) time.Duration {
	return min(max(0, now.Sub(started)), ceiling)
}

func (g *Gate) waitStateLocked(key string) *waitState {
	if st, ok := g.waits[key]; ok {
		return st
	}
	kept := g.waitOrder[:0]
	for _, k := range g.waitOrder {
		if len(g.waits) >= humanWaitMaxSessions && g.waits[k].pending == 0 {
			delete(g.waits, k)
			continue
		}
		kept = append(kept, k)
	}
	g.waitOrder = kept
	st := &waitState{}
	g.waits[key] = st
	g.waitOrder = append(g.waitOrder, key)
	return st
}

func (g *Gate) humanWait(ctx context.Context, key string) func() {
	now := time.Now()
	g.mu.Lock()
	st := g.waitStateLocked(key)
	if st.pending == 0 {
		st.started, st.open = now, true
	}
	st.pending++
	g.mu.Unlock()
	return func() {
		now := time.Now()
		ceiling := g.ceiling(ctx)
		g.mu.Lock()
		defer g.mu.Unlock()
		st, ok := g.waits[key]
		if !ok {
			return
		}
		st.pending--
		if st.pending == 0 {
			if st.open {
				st.completed += clampedWindow(st.started, now, ceiling)
			}
			st.open = false
		}
	}
}

func waitKey(sessionKey string) string {
	if sessionKey == "" {
		return "default"
	}
	return sessionKey
}

func (g *Gate) Window(ctx context.Context, sessionKey string) func() {
	return g.humanWait(ctx, waitKey(sessionKey))
}

func (g *Gate) Seconds(sessionKey string) float64 {
	now := time.Now()
	ceiling := g.Ceiling()
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.waits[waitKey(sessionKey)]
	if !ok {
		return 0
	}
	total := st.completed
	if st.open {
		total += clampedWindow(st.started, now, ceiling)
	}
	return total.Seconds()
}

type heartbeat struct {
	c     *tool.Call
	label string
	start time.Time
	last  time.Time
}

func newHeartbeat(c *tool.Call, label string) *heartbeat {
	now := time.Now()
	return &heartbeat{c: c, label: label, start: now, last: now}
}

func (h *heartbeat) beat() {
	now := time.Now()
	if now.Sub(h.last) < heartbeatInterval {
		return
	}
	h.last = now
	if h.c == nil || h.c.Activity == nil {
		return
	}
	h.c.Activity(h.label + " (" + strconv.Itoa(int(now.Sub(h.start).Seconds())) + "s elapsed)")
}

var _ tool.HumanWait = (*Gate)(nil)
