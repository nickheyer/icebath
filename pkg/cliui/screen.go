package cliui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/xo/terminfo"
)

// Attr is a curses style attribute, Pair naming color pair one green, two yellow, three dim gray, four red
type Attr struct {
	Bold bool
	Dim  bool
	Pair int
}

// Screen is the curses window surface menus draw on and read keys from
type Screen interface {
	// Next key code, keypad sequences translated, minus one when the timeout passes
	Getch() (int, error)
	// Read timeout in milliseconds, negative blocking
	Timeout(ms int)
	Clear()
	GetMaxYX() (int, int)
	// Writes at most n characters at a cell, an error meaning the write fell off the screen
	AddNStr(y, x int, text string, n int, attr Attr) error
	Refresh() error
	HasColors() bool
	// Hides the cursor and sets up the color pairs, failing like curs_set or start_color on a limited terminal
	InitColors(extraPairs bool) error
}

type cell struct {
	text string
	attr Attr
	wide bool
}

// Full screen ANSI renderer over the console terminal with curses keypad translation
type ansiScreen struct {
	c         *Console
	ti        *terminfo.Terminfo
	rows      [][]cell
	h, w      int
	timeoutMs int
	pending   []int
	keypad    map[string]int
	prefixes  map[string]bool
	colors    int
	pairs     map[int]string
	sigCh     chan os.Signal
	escDelay  time.Duration
}

var keypadCaps = map[int]int{
	terminfo.KeyUp: KeyUp, terminfo.KeyDown: KeyDown, terminfo.KeyLeft: KeyLeft, terminfo.KeyRight: KeyRight,
	terminfo.KeyHome: KeyHome, terminfo.KeyBackspace: KeyBackspace, terminfo.KeyDc: KeyDC, terminfo.KeyIc: KeyIC,
	terminfo.KeyNpage: KeyNPage, terminfo.KeyPpage: KeyPPage, terminfo.KeyEnter: KeyEnter, terminfo.KeyEnd: KeyEnd,
	terminfo.KeyF1: KeyF0 + 1, terminfo.KeyF2: KeyF0 + 2, terminfo.KeyF3: KeyF0 + 3, terminfo.KeyF4: KeyF0 + 4,
	terminfo.KeyF5: KeyF0 + 5, terminfo.KeyF6: KeyF0 + 6, terminfo.KeyF7: KeyF0 + 7, terminfo.KeyF8: KeyF0 + 8,
	terminfo.KeyF9: KeyF0 + 9, terminfo.KeyF10: KeyF0 + 10, terminfo.KeyF11: KeyF0 + 11, terminfo.KeyF12: KeyF0 + 12,
}

// Opens the full screen renderer like initscr with keypad on, failing when TERM has no terminfo entry
func (c *Console) openANSIScreen() (Screen, func(), error) {
	name, _ := c.getenv("TERM")
	if name == "" {
		return nil, nil, fmt.Errorf("%w: setupterm: could not find terminal", errCursesInit)
	}
	ti, err := terminfo.Load(name)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: setupterm: could not find terminal: %v", errCursesInit, err)
	}
	restore, err := c.makeRaw()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errCursesInit, err)
	}
	s := &ansiScreen{c: c, ti: ti, timeoutMs: -1, keypad: map[string]int{}, prefixes: map[string]bool{}, pairs: map[int]string{}, escDelay: c.escDelay()}
	for capIdx, code := range keypadCaps {
		if seq := string(ti.Strings[capIdx]); seq != "" {
			s.keypad[seq] = code
			for i := 1; i < len(seq); i++ {
				s.prefixes[seq[:i]] = true
			}
		}
	}
	s.colors = ti.Nums[terminfo.MaxColors]
	if c.stdinTTY() {
		s.sigCh = make(chan os.Signal, 1)
		signal.Notify(s.sigCh, os.Interrupt)
	}
	io.WriteString(c.Out, string(ti.Strings[terminfo.EnterCaMode])+string(ti.Strings[terminfo.KeypadXmit]))
	closeFn := func() {
		if s.sigCh != nil {
			signal.Stop(s.sigCh)
		}
		io.WriteString(c.Out, "\x1b[0m"+string(ti.Strings[terminfo.CursorNormal])+string(ti.Strings[terminfo.KeypadLocal])+string(ti.Strings[terminfo.ExitCaMode]))
		restore()
	}
	return s, closeFn, nil
}

func (s *ansiScreen) HasColors() bool {
	return s.colors > 0 && (len(s.ti.Strings[terminfo.SetAForeground]) > 0 || len(s.ti.Strings[terminfo.SetForeground]) > 0)
}

func (s *ansiScreen) InitColors(extra bool) error {
	civis := s.ti.Strings[terminfo.CursorInvisible]
	if len(civis) == 0 {
		return errors.New("curs_set() returned ERR")
	}
	io.WriteString(s.c.Out, string(civis))
	if !s.HasColors() {
		return nil
	}
	s.pairs[1] = "32"
	s.pairs[2] = "33"
	s.pairs[4] = "31"
	if extra {
		if s.colors > 8 {
			s.pairs[3] = "90"
		} else {
			s.pairs[3] = "37"
		}
	}
	return nil
}

func (s *ansiScreen) Timeout(ms int) { s.timeoutMs = ms }

func (s *ansiScreen) Clear() {
	h, w := s.GetMaxYX()
	s.h, s.w = h, w
	s.rows = make([][]cell, h)
	for y := range s.rows {
		s.rows[y] = make([]cell, w)
	}
}

func (s *ansiScreen) GetMaxYX() (int, int) {
	h, w, err := s.c.termSize()
	if err != nil || h <= 0 || w <= 0 {
		return 24, 80
	}
	return h, w
}

func (s *ansiScreen) AddNStr(y, x int, text string, n int, attr Attr) error {
	if y < 0 || y >= s.h || x < 0 || x >= s.w {
		return errors.New("addnstr() returned ERR")
	}
	col := x
	count := 0
	for _, g := range graphemes(text) {
		if count >= n {
			break
		}
		gw := ansi.StringWidth(g)
		if gw == 0 {
			gw = 1
		}
		if col+gw > s.w {
			return errors.New("addnstr() returned ERR")
		}
		s.rows[y][col] = cell{text: g, attr: attr}
		if gw == 2 {
			s.rows[y][col].wide = true
			s.rows[y][col+1] = cell{text: "", attr: attr}
		}
		col += gw
		count += len([]rune(g))
	}
	return nil
}

// Splits text into runes, combining marks joining their base
func graphemes(text string) []string {
	var out []string
	for _, r := range text {
		if len(out) > 0 && ansi.StringWidth(string(r)) == 0 && r >= 0x300 {
			out[len(out)-1] += string(r)
			continue
		}
		out = append(out, string(r))
	}
	return out
}

func (s *ansiScreen) sgr(a Attr) string {
	codes := []string{"0"}
	if a.Bold {
		codes = append(codes, "1")
	}
	if a.Dim {
		codes = append(codes, "2")
	}
	if fg, ok := s.pairs[a.Pair]; ok && a.Pair > 0 {
		codes = append(codes, fg)
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// Paints the whole frame row by row with attribute runs and clears each row tail
func (s *ansiScreen) Refresh() error {
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	for y, row := range s.rows {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[0m\x1b[2K", y+1)
		var cur *Attr
		for x := 0; x < len(row); x++ {
			c := row[x]
			if c.text == "" && x > 0 && row[x-1].wide {
				continue
			}
			text := c.text
			if text == "" {
				text = " "
			}
			if cur == nil || *cur != c.attr {
				a := c.attr
				cur = &a
				b.WriteString(s.sgr(a))
			}
			b.WriteString(text)
		}
	}
	b.WriteString("\x1b[0m\x1b[?2026l")
	_, err := io.WriteString(s.c.Out, b.String())
	return err
}

// Reads one byte honoring the screen timeout, minus one when it passes
func (s *ansiScreen) readRaw(timeout time.Duration) (int, error) {
	b, err := s.c.readByte(timeout, s.sigCh)
	if errors.Is(err, errTimeout) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return int(b), nil
}

func (s *ansiScreen) timeoutDuration() time.Duration {
	if s.timeoutMs < 0 {
		return -1
	}
	return time.Duration(s.timeoutMs) * time.Millisecond
}

// Next key with keypad sequences translated, unmatched sequence bytes replayed one by one
func (s *ansiScreen) Getch() (int, error) {
	if len(s.pending) > 0 {
		k := s.pending[0]
		s.pending = s.pending[1:]
		return k, nil
	}
	k, err := s.readRaw(s.timeoutDuration())
	if err != nil || k < 0 {
		return k, err
	}
	seq := string(rune(k))
	if k >= 0x80 {
		seq = string([]byte{byte(k)})
	}
	if !s.prefixes[seq] {
		if code, ok := s.keypad[seq]; ok {
			return code, nil
		}
		return k, nil
	}
	buf := []byte{byte(k)}
	for {
		nb, err := s.readRaw(s.escDelay)
		if err != nil {
			return 0, err
		}
		if nb < 0 {
			break
		}
		buf = append(buf, byte(nb))
		if code, ok := s.keypad[string(buf)]; ok && !s.prefixes[string(buf)] {
			return code, nil
		}
		if !s.prefixes[string(buf)] {
			break
		}
	}
	if code, ok := s.keypad[string(buf)]; ok {
		return code, nil
	}
	for _, b := range buf[1:] {
		s.pending = append(s.pending, int(b))
	}
	return int(buf[0]), nil
}
