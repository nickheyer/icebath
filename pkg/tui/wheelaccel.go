package tui

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Pacing of the wheel in milliseconds and rows, the windows events are told apart by and the caps the rows of one event grow to
const (
	wheelAccelWindowMs       = 40
	wheelAccelStep           = 0.3
	wheelAccelMax            = 6
	wheelBounceGapMaxMs      = 200
	wheelModeStep            = 15
	wheelModeCap             = 15
	wheelModeRamp            = 3
	wheelModeIdleDisengageMs = 1500
	wheelDecayHalflifeMs     = 150
	wheelDecayStep           = 5
	wheelBurstMs             = 5
	wheelDecayGapMs          = 80
	wheelDecayCapSlow        = 3
	wheelDecayCapFast        = 6
	wheelDecayIdleMs         = 500
	wheelBurstEvents         = 5
	precisionWheelFrameMs    = 16
	precisionWheelStickyMs   = 80
	scrollSpeedMax           = 20
)

// Number a setting starts with, the way a script reads a decimal
var leadingNumber = regexp.MustCompile(`^\s*[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?`)

// Wheel acceleration between events, the rows one event scrolls growing with how closely events follow each other
type wheelAccel struct {
	time float64
	mult float64
	dir  int
	// Terminal built on xterm.js, whose events arrive at another cadence than those of a native terminal
	xtermJS bool
	// Part of a row carried to the next event of an xterm.js terminal
	frac float64
	// Rows of one event before any acceleration
	base float64
	// Turn of direction held back until the next event tells a reversal from a bounce of the wheel
	pendingFlip bool
	// Mechanical wheel recognized by its bounce, scrolling by larger steps until it rests
	wheelMode  bool
	burstCount int
}

// Acceleration at rest for a terminal kind and base speed
func newWheelAccel(xtermJS bool, base float64) wheelAccel {
	return wheelAccel{base: base, mult: base, xtermJS: xtermJS}
}

// Rows of one wheel event before acceleration, ICEBATH_TUI_SCROLL_SPEED or else CLAUDE_CODE_SCROLL_SPEED above zero and at most twenty, one otherwise
func scrollSpeedBase(lookup func(string) (string, bool)) float64 {
	raw, set := lookup("ICEBATH_TUI_SCROLL_SPEED")
	if !set {
		raw, _ = lookup("CLAUDE_CODE_SCROLL_SPEED")
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(leadingNumber.FindString(raw)), 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) || n <= 0 {
		return 1
	}
	return math.Min(n, scrollSpeedMax)
}

// Rows a wheel event scrolls in its direction, zero while a turn of direction waits to be told from a bounce
func (w *wheelAccel) step(dir int, now float64) int {
	if w.xtermJS {
		return w.xtermStep(dir, now)
	}
	return w.nativeStep(dir, now)
}

// Step on a native terminal, a short ramp for close events and larger steps once a bouncing mechanical wheel is recognized
func (w *wheelAccel) nativeStep(dir int, now float64) int {
	if w.wheelMode && now-w.time > wheelModeIdleDisengageMs {
		w.wheelMode, w.burstCount, w.mult = false, 0, w.base
	}
	if w.pendingFlip {
		w.pendingFlip = false
		if dir != w.dir || now-w.time > wheelBounceGapMaxMs {
			w.dir, w.time, w.mult = dir, now, w.base
			return int(math.Floor(w.mult))
		}
		w.wheelMode = true
	}
	gap := now - w.time
	if dir != w.dir && w.dir != 0 {
		w.pendingFlip, w.time = true, now
		return 0
	}
	w.dir, w.time = dir, now
	if w.wheelMode {
		if gap < wheelBurstMs {
			if w.burstCount++; w.burstCount < wheelBurstEvents {
				return 1
			}
			w.wheelMode, w.burstCount, w.mult = false, 0, w.base
		} else {
			w.burstCount = 0
		}
	}
	if w.wheelMode {
		decay := math.Pow(0.5, gap/wheelDecayHalflifeMs)
		w.mult = min(max(wheelModeCap, w.base*2), 1+(w.mult-1)*decay+wheelModeStep*decay, w.mult+wheelModeRamp)
		return int(math.Floor(w.mult))
	}
	if gap > wheelAccelWindowMs {
		w.mult = w.base
	} else {
		w.mult = min(max(wheelAccelMax, w.base*2), w.mult+wheelAccelStep)
	}
	return int(math.Floor(w.mult))
}

// Step on an xterm.js terminal, the multiplier decaying with the gap between events and the part of a row carried over
func (w *wheelAccel) xtermStep(dir int, now float64) int {
	gap, same := now-w.time, dir == w.dir
	w.time, w.dir = now, dir
	if same && gap < wheelBurstMs {
		return 1
	}
	if !same || gap > wheelDecayIdleMs {
		w.mult, w.frac = 2, 0
	} else {
		decay := math.Pow(0.5, gap/wheelDecayHalflifeMs)
		limit := float64(wheelDecayCapFast)
		if gap >= wheelDecayGapMs {
			limit = wheelDecayCapSlow
		}
		w.mult = min(limit, 1+(w.mult-1)*decay+wheelDecayStep*decay)
	}
	total := w.mult + w.frac
	rows := math.Floor(total)
	w.frac = total - rows
	return int(rows)
}

// Wheel turned with a modifier held, one row a frame and no acceleration
type precisionWheel struct {
	active       bool
	dir          int
	lastEventAt  float64
	lastScrollAt float64
}

// Step of a wheel event while a modifier is held or was a moment ago, whether this event entered the mode, and the one row it scrolls unless a row already moved this frame
func (p *precisionWheel) step(dir int, modifier bool, now float64) (active, entered bool, rows int) {
	if !modifier && now-p.lastEventAt >= precisionWheelStickyMs {
		p.active = false
		return false, false, 0
	}
	entered = !p.active
	p.active, p.lastEventAt = true, now
	if dir == p.dir && now-p.lastScrollAt < precisionWheelFrameMs {
		return true, entered, 0
	}
	p.dir, p.lastScrollAt = dir, now
	return true, entered, 1
}
