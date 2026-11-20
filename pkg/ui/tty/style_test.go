package tty

import (
	"math/rand/v2"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Random style over every attribute, shape and color kind
func randomStyle(rng *rand.Rand) Style {
	col := func() Color {
		switch rng.IntN(4) {
		case 0:
			return 0
		case 1:
			return Basic(uint8(rng.IntN(16)))
		case 2:
			return Indexed(uint8(rng.IntN(256)))
		}
		return RGB(uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256)))
	}
	ul := col()
	if ul&kindMask == kindBasic {
		ul = Indexed(uint8(ul))
	}
	return Style{Fg: col(), Bg: col(), Ul: ul, Under: uint8(rng.IntN(6)), Attrs: uint8(rng.IntN(256))}
}

// Pen after the SGR sequence in b is applied to from
func applySGR(t *testing.T, from Style, b []byte) Style {
	t.Helper()
	p := ansi.NewParser()
	pen := from
	var state byte
	for len(b) > 0 {
		seq, _, n, next := ansi.DecodeSequence(b, state, p)
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			readSGR(p.Params(), &pen)
		} else {
			t.Fatalf("unexpected sequence %q", seq)
		}
		state, b = next, b[n:]
	}
	return pen
}

func TestSGRTransitionsLandOnTheTarget(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		from, to := randomStyle(rng), randomStyle(rng)
		b := appendSGR(nil, from, true, to)
		if got := applySGR(t, from, b); got != to {
			t.Fatalf("%+v to %+v wrote %q and landed on %+v", from, to, b, got)
		}
		if reset := append(append([]byte("\x1b[0;"), onParams(nil, to)...), 'm'); len(b) > len(reset) {
			t.Fatalf("%q is longer than the reset form %q", b, reset)
		}
		if got := applySGR(t, randomStyle(rng), appendSGR(nil, from, false, to)); got != to {
			t.Fatalf("unknown pen to %+v landed on %+v", to, got)
		}
		if sgrCost(from, true, to) != len(b) {
			t.Fatalf("cost of %q reported as %d", b, sgrCost(from, true, to))
		}
	}
}

func TestSGRKeepsColorKinds(t *testing.T) {
	cases := map[string]Style{
		"\x1b[31m":            {Fg: Basic(1)},
		"\x1b[91m":            {Fg: Basic(9)},
		"\x1b[44m":            {Bg: Basic(4)},
		"\x1b[103m":           {Bg: Basic(11)},
		"\x1b[38;5;196m":      {Fg: Indexed(196)},
		"\x1b[48;2;1;2;3m":    {Bg: RGB(1, 2, 3)},
		"\x1b[58;5;3m":        {Ul: Indexed(3)},
		"\x1b[4:3m":           {Under: UnderCurly},
		"\x1b[1;2;3;5;7;8;9m": {Attrs: AttrBold | AttrFaint | AttrItalic | AttrBlink | AttrReverse | AttrConceal | AttrStrike},
	}
	for seq, want := range cases {
		if got := applySGR(t, Style{}, []byte(seq)); got != want {
			t.Errorf("%q read as %+v, want %+v", seq, got, want)
		}
		if b := appendSGR(nil, Style{}, true, want); applySGR(t, Style{}, b) != want {
			t.Errorf("%+v written as %q does not read back", want, b)
		}
	}
}
