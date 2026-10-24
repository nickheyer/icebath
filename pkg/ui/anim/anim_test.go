package anim

import (
	"testing"
	"time"
)

func settle(t *testing.T, step func(float64) bool) int {
	t.Helper()
	for i := 1; i <= 2000; i++ {
		if !step(1.0 / 60) {
			return i
		}
	}
	t.Fatal("spring never settled")
	return 0
}

func TestValueReachesTarget(t *testing.T) {
	v := NewValue(0)
	v.Set(10)
	frames := settle(t, v.Step)
	if v.Pos != 10 || v.Vel != 0 {
		t.Fatalf("pos %v vel %v", v.Pos, v.Vel)
	}
	if frames > 120 {
		t.Fatalf("took %d frames", frames)
	}
}

func TestKickOvershootsThenReturns(t *testing.T) {
	v := NewValue(5)
	v.Kick(26)
	moved := false
	settle(t, func(dt float64) bool {
		moving := v.Step(dt)
		if v.Pos > 5.5 {
			moved = true
		}
		return moving
	})
	if !moved || v.Pos != 5 {
		t.Fatalf("moved %v pos %v", moved, v.Pos)
	}
}

func TestRectConverges(t *testing.T) {
	r := NewRect(0, 0, 10, 10)
	r.Set(20, 5, 40, 12)
	if r.Distance() != 30 {
		t.Fatalf("distance %v", r.Distance())
	}
	settle(t, r.Step)
	x, y, w, h := r.Cells()
	if x != 20 || y != 5 || w != 40 || h != 12 {
		t.Fatalf("cells %d %d %d %d", x, y, w, h)
	}
	if r.W.Damping != 1 || r.H.Damping != 1 {
		t.Fatal("size springs must be critically damped")
	}
}

func TestClockRejectsStaleFrames(t *testing.T) {
	c := NewClock(60, 12)
	stale := c.Wake()
	fresh := c.Wake()
	if _, ok := c.Accept(stale().(FrameMsg)); ok {
		t.Fatal("stale frame accepted")
	}
	frame, ok := c.Accept(fresh().(FrameMsg))
	if !ok || frame.DT <= 0 {
		t.Fatalf("ok %v dt %v", ok, frame.DT)
	}
	other := NewClock(60, 12)
	if _, ok := c.Accept(other.Wake()().(FrameMsg)); ok {
		t.Fatal("foreign frame accepted")
	}
}

func TestClockClampsDelta(t *testing.T) {
	c := NewClock(60, 12)
	first, _ := c.Accept(c.Wake()().(FrameMsg))
	late := FrameMsg{At: first.At.Add(time.Second), id: c.id, gen: c.gen}
	frame, ok := c.Accept(late)
	if !ok || frame.DT != 1.0/15 {
		t.Fatalf("ok %v dt %v", ok, frame.DT)
	}
}

func TestPresetsAndSteadyEdges(t *testing.T) {
	v := NewValueWith(2, Rattle)
	if v.Pos != 2 || v.Frequency != Rattle.Frequency || v.Damping != Rattle.Damping {
		t.Fatalf("%+v", v)
	}
	v.Use(Snappy)
	if v.Frequency != 18 || v.Damping != 1 {
		t.Fatalf("%+v", v)
	}
	v.Kick(90)
	swings, last := 0, 0.0
	v.Use(Rattle)
	settle(t, func(dt float64) bool {
		moving := v.Step(dt)
		if (v.Pos > 2.5 && last <= 2.5) || (v.Pos < 1.5 && last >= 1.5) {
			swings++
		}
		last = v.Pos
		return moving
	})
	if swings < 3 {
		t.Fatalf("a rattle swings a few times, got %d", swings)
	}
	r := NewRect(0, 0, 10, 10)
	r.X.Pos, r.W.Pos = 2.4, 10.3
	x, _, w, _ := r.Cells()
	if x != 2 || x+w != 13 {
		t.Fatalf("far edge rounds on its own, %d %d", x, w)
	}
	r.X.Pos, r.W.Pos = 2.6, 10.1
	x, _, w, _ = r.Cells()
	if x != 3 || x+w != 13 {
		t.Fatalf("the far edge holds still while the near edge crosses a cell, %d %d", x, w)
	}
	if c := NewClock(0, 0); c.Active != 60 || c.Idle != 30 {
		t.Fatalf("%+v", c)
	}
}
