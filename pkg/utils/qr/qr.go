package qrutil

import (
	"errors"
	"fmt"

	"rsc.io/qr/coding"
)

// How much of a QR symbol can be damaged and still read; higher levels hold less data
type Level int

const (
	// About 7% of the symbol recoverable
	Low Level = iota
	// About 15% of the symbol recoverable
	Medium
	// About 25% of the symbol recoverable
	Quartile
	// About 30% of the symbol recoverable
	High
)

// Error for text that does not fit even the largest QR symbol at the chosen level
var ErrTooLong = errors.New("text too long for a QR code")

// QR symbol, a square of dark and light modules without the quiet zone around it
type Code struct {
	size    int
	modules []bool
}

// First and last version of each character count width class
var classVersions = [3][2]coding.Version{{1, 9}, {10, 26}, {27, 40}}

// Smallest QR symbol holding the bytes of text at the level, segmented into the modes that need the fewest bits and masked with the pattern that scores the lowest penalty
func Encode(text string, level Level) (*Code, error) {
	if level < Low || level > High {
		return nil, fmt.Errorf("unknown QR error correction level %d", int(level))
	}
	l := coding.Level(level)
	for class, versions := range classVersions {
		segs := segment(text, class)
		bits := 0
		for _, s := range segs {
			bits += s.Bits(versions[0])
		}
		for v := versions[0]; v <= versions[1]; v++ {
			if bits <= v.DataBytes(l)*8 {
				return build(v, l, segs)
			}
		}
	}
	return nil, fmt.Errorf("%w: %d bytes", ErrTooLong, len(text))
}

// Symbol of the segments at a version and level under each of the eight masks, the lowest penalty kept
func build(v coding.Version, l coding.Level, segs []coding.Encoding) (*Code, error) {
	var best *Code
	bestScore := 0
	var spare []bool
	for mask := range 8 {
		plan, err := coding.NewPlan(v, l, coding.Mask(mask))
		if err != nil {
			return nil, err
		}
		cc, err := plan.Encode(segs...)
		if err != nil {
			return nil, err
		}
		if len(spare) != cc.Size*cc.Size {
			spare = make([]bool, cc.Size*cc.Size)
		}
		for y := range cc.Size {
			for x := range cc.Size {
				spare[y*cc.Size+x] = cc.Black(x, y)
			}
		}
		if score := penalty(spare, cc.Size); best == nil || score < bestScore {
			if best == nil {
				best = &Code{size: cc.Size}
			}
			best.modules, spare = spare, best.modules
			bestScore = score
		}
	}
	return best, nil
}
