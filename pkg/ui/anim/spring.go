// Spring driven scalars and rectangles for terminal motion
package anim

import (
	"math"

	"github.com/charmbracelet/harmonica"
)

const (
	// Angular frequency that settles most moves in about a third of a second
	DefaultFrequency = 13.0
	// Damping ratio that overshoots slightly before coming to rest
	DefaultDamping = 0.8
	restPos        = 0.05
	restVel        = 0.6
)

// Response and bounce of a spring, named for the motion it gives
type Spring struct {
	Frequency float64
	Damping   float64
}

var (
	// Chrome that keeps up with the hand, settled in a quarter second without overshoot
	Snappy = Spring{Frequency: 18, Damping: 1}
	// Layout moves that land with a hint of overshoot
	Smooth = Spring{Frequency: DefaultFrequency, Damping: DefaultDamping}
	// Fades and veils that ease in without a bounce
	Gentle = Spring{Frequency: 10, Damping: 1}
	// Refusals that swing a few times before resting
	Rattle = Spring{Frequency: 36, Damping: 0.2}
)

// Scalar that eases toward its target with damped spring motion
type Value struct {
	Pos       float64
	Vel       float64
	Target    float64
	Frequency float64
	Damping   float64
}

// Builds a resting scalar with the default spring parameters
func NewValue(v float64) Value {
	return Value{Pos: v, Target: v, Frequency: DefaultFrequency, Damping: DefaultDamping}
}

// Builds a resting scalar that moves with the spring
func NewValueWith(v float64, s Spring) Value {
	out := NewValue(v)
	out.Use(s)
	return out
}

// Switches the motion to the spring without disturbing the position
func (v *Value) Use(s Spring) { v.Frequency, v.Damping = s.Frequency, s.Damping }

// Retargets the motion while keeping the current position and velocity
func (v *Value) Set(target float64) { v.Target = target }

// Adds velocity so the scalar overshoots and springs back to its target
func (v *Value) Kick(impulse float64) { v.Vel += impulse }

// Jumps straight to the target and stops
func (v *Value) Snap() { v.Pos, v.Vel = v.Target, 0 }

// Advances the spring by dt seconds and reports whether it is still moving
func (v *Value) Step(dt float64) bool {
	if v.Settled() {
		v.Pos, v.Vel = v.Target, 0
		return false
	}
	s := harmonica.NewSpring(dt, v.Frequency, v.Damping)
	v.Pos, v.Vel = s.Update(v.Pos, v.Vel, v.Target)
	if v.Settled() {
		v.Pos, v.Vel = v.Target, 0
		return false
	}
	return true
}

// Reports whether position and velocity are both inside the rest tolerance
func (v Value) Settled() bool {
	return math.Abs(v.Pos-v.Target) < restPos && math.Abs(v.Vel) < restVel
}

// Box whose edges animate independently, with size springs that never overshoot
type Rect struct {
	X, Y, W, H Value
}

// Builds a resting rectangle
func NewRect(x, y, w, h float64) Rect {
	r := Rect{X: NewValue(x), Y: NewValue(y), W: NewValue(w), H: NewValue(h)}
	r.W.Damping, r.H.Damping = 1, 1
	return r
}

// Retargets all four edges
func (r *Rect) Set(x, y, w, h float64) {
	r.X.Set(x)
	r.Y.Set(y)
	r.W.Set(w)
	r.H.Set(h)
}

// Jumps every edge straight to its target
func (r *Rect) Snap() {
	r.X.Snap()
	r.Y.Snap()
	r.W.Snap()
	r.H.Snap()
}

// Advances every edge by dt seconds and reports whether any is still moving
func (r *Rect) Step(dt float64) bool {
	x := r.X.Step(dt)
	y := r.Y.Step(dt)
	w := r.W.Step(dt)
	h := r.H.Step(dt)
	return x || y || w || h
}

// Current box rounded to whole cells, the far edges rounded on their own so neither side jitters
func (r Rect) Cells() (x, y, w, h int) {
	return cells(r.X.Pos, r.Y.Pos, r.W.Pos, r.H.Pos)
}

// Destination box rounded to whole cells
func (r Rect) Target() (x, y, w, h int) {
	return cells(r.X.Target, r.Y.Target, r.W.Target, r.H.Target)
}

// Largest remaining gap between any edge and its target
func (r Rect) Distance() float64 {
	d := math.Abs(r.X.Pos - r.X.Target)
	d = math.Max(d, math.Abs(r.Y.Pos-r.Y.Target))
	d = math.Max(d, math.Abs(r.W.Pos-r.W.Target))
	return math.Max(d, math.Abs(r.H.Pos-r.H.Target))
}

func cells(x, y, w, h float64) (int, int, int, int) {
	x0, y0 := round(x), round(y)
	return x0, y0, max(0, round(x+w)-x0), max(0, round(y+h)-y0)
}

func round(f float64) int { return int(math.Round(f)) }
