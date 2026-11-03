package gfx

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image/color"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

const (
	ackTimeout    = 400 * time.Millisecond
	silentAfter   = 3
	brokenAfter   = 3
	fastFrames    = 8
	maxBounces    = 6
	maxDivisor    = 8.0
	directDivisor = 2.0
	sentLimit     = 8
	minBudgetW    = 64
	minBudgetH    = 32
)

// Count of distinct row and column marks the kitty placeholder table holds
var diacritics = func() int {
	n := 1
	for kitty.Diacritic(n) != kitty.Diacritic(0) {
		n++
	}
	return n
}()

// Numbers for a status line
type Stats struct {
	Latency  time.Duration
	Divisor  float64
	Frames   int
	Acked    int
	Timeouts int
	Failed   int
}

// States an image id of a kitty surface moves through
const (
	empty = iota
	flying
	ready
)

// Transmission into an image id waiting for its answer, naming its shared memory object when it used one
type sending struct {
	at  time.Time
	shm string
}

// One of the two image ids a kitty surface alternates between
type slot struct {
	state      int
	promoted   bool
	placed     bool
	gen        uint64
	sentAt     time.Time
	sent       []sending
	cols, rows int
}

// Box of cells the last sixel or iTerm2 image covered
type box struct {
	x, y, cols, rows int
	set              bool
}

// Image shown over a box of cells, frame after frame, by one protocol
type Surface struct {
	t     *Terminal
	proto Protocol
	ids   [2]uint32
	seq   atomic.Uint64

	pack sync.Mutex
	zw   *zlib.Writer
	comp bytes.Buffer
	six  sixelEncoder
	png  pngEncoder

	mu                    sync.Mutex
	rate                  int
	divisor               float64
	shared                bool
	slots                 [2]slot
	inflight              int
	gen                   uint64
	lastEmit              time.Time
	lastSeq, floor        uint64
	timeouts, fails, fast int
	// Whether the divisor was lowered since it last rose, and how often a rise followed a lowering, each doubling the fast run a lowering takes
	lowered bool
	bounces int
	stats   Stats
	pending map[uint64]string
	box     box
	grid    [][]string
	gridMSB uint32
}

// Reports whether the protocol is one of the kitty graphics ways
func isKitty(p Protocol) bool { return p == KittyCells || p == KittyPlaced }

// Frames per second each protocol takes at most
func maxRate(p Protocol) int {
	switch p {
	case KittyCells, KittyPlaced:
		return 30
	case Sixel:
		return 8
	case ITerm:
		return 6
	}
	return 1
}

// Most pixels one frame of the protocol carries
func pixelCap(p Protocol, shared bool) float64 {
	switch {
	case isKitty(p) && shared:
		return 2.0e6
	case isKitty(p):
		return 0.8e6
	case p == Sixel, p == ITerm:
		return 0.3e6
	}
	return 0
}

// Surface drawing through the given protocol, which must be one the terminal returned from Protocol or Guess
func (t *Terminal) NewSurface(p Protocol) *Surface {
	s := &Surface{t: t, proto: p, ids: t.allocIDs(), rate: maxRate(p), divisor: 1, inflight: -1, pending: map[uint64]string{}}
	if isKitty(p) {
		s.shared = t.shared()
		if !s.shared {
			s.divisor = directDivisor
		}
		s.zw, _ = zlib.NewWriterLevel(&s.comp, zlib.BestSpeed)
	}
	return s
}

// Way the surface draws its images
func (s *Surface) Protocol() Protocol { return s.proto }

// Frames per second the surface is asked to keep up with, clamped to what the protocol takes
func (s *Surface) SetRate(fps int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate = min(max(fps, 1), maxRate(s.proto))
}

func (s *Surface) interval() time.Duration { return time.Second / time.Duration(s.rate) }

// Leaves shared memory for direct transmission once the terminal stopped taking it, starting the divisor over
func (s *Surface) syncLocked(shared bool) {
	if s.shared && !shared {
		s.shared, s.divisor, s.fast, s.lowered, s.bounces = false, directDivisor, 0, false, 0
	}
}

// Whether a new frame may be emitted now
func (s *Surface) Ready(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleLocked(now)
	switch s.proto {
	case KittyCells, KittyPlaced:
		return s.inflight < 0
	case Sixel, ITerm:
		return s.lastEmit.IsZero() || now.Sub(s.lastEmit) >= s.interval()
	}
	return false
}

// Takes an unanswered frame as landed past the timeout, counting it, or past the interval once the terminal is silent
func (s *Surface) settleLocked(now time.Time) {
	if !isKitty(s.proto) || s.inflight < 0 {
		return
	}
	age := now.Sub(s.slots[s.inflight].sentAt)
	if s.timeouts >= silentAfter {
		if age >= s.interval() {
			s.promoteLocked()
		}
		return
	}
	if age >= ackTimeout {
		s.stats.Timeouts++
		s.timeouts++
		s.fast = 0
		s.promoteLocked()
	}
}

func (s *Surface) promoteLocked() {
	sl := &s.slots[s.inflight]
	sl.state, sl.promoted = ready, true
	s.inflight = -1
}

// Pixels across and down the next frame should be rendered at for a box of cols by rows cells
func (s *Surface) Budget(cols, rows int) (w, h int) {
	cw, ch := s.t.Cell()
	shared := s.t.shared()
	s.mu.Lock()
	s.syncLocked(shared)
	d, limit := s.divisor, pixelCap(s.proto, s.shared)
	s.mu.Unlock()
	return budget(float64(cols*cw), float64(rows*ch), d, limit)
}

// Native size divided by the divisor, floored at the minimum, capped by pixels and the native size, aspect kept
func budget(nw, nh, divisor, limit float64) (w, h int) {
	if nw <= 0 || nh <= 0 || limit <= 0 {
		return 0, 0
	}
	f := max(1/divisor, minBudgetW/nw, minBudgetH/nh)
	f = min(f, math.Sqrt(limit/(nw*nh)), 1)
	return max(int(nw*f), 1), max(int(nh*f), 1)
}

// Escape text that shows a packet with the top left cell of its box at screen cell x, y
func (s *Surface) Emit(p *Packet, x, y int, now time.Time) string {
	if p == nil || p.s != s || s.proto == None {
		return ""
	}
	shared := s.t.shared()
	if isKitty(s.proto) {
		s.t.register(s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked(shared)
	if s.staleLocked(p.seq) {
		s.dropLocked(p.seq)
		return ""
	}
	s.settleLocked(now)
	s.lastSeq = p.seq
	s.pruneLocked(p.seq)
	var out string
	if isKitty(s.proto) {
		out = s.emitKittyLocked(p, x, y, now)
	} else {
		out = s.emitCursorLocked(p, x, y)
	}
	if out == "" {
		return ""
	}
	s.lastEmit = now
	s.stats.Frames++
	return out
}

// Reports whether a packet is older than one already emitted or was packed before the last release
func (s *Surface) staleLocked(seq uint64) bool { return seq < s.lastSeq || seq <= s.floor }

// Removes the shared memory object of a packet that will never be shown
func (s *Surface) dropLocked(seq uint64) {
	if name, ok := s.pending[seq]; ok {
		shmRemove(name)
		delete(s.pending, seq)
	}
}

// Removes the shared memory objects of every packet older than the one emitted
func (s *Surface) pruneLocked(seq uint64) {
	for k, name := range s.pending {
		if k < seq {
			shmRemove(name)
			delete(s.pending, k)
		}
	}
}

// Image id the next transmission goes to, never the one the cells or the screen show
func (s *Surface) reuseLocked() int {
	if c := s.latestLocked(); c >= 0 {
		return 1 - c
	}
	if s.inflight >= 0 {
		return s.inflight
	}
	return 0
}

// Slot holding the newest image the terminal has, minus one when none has landed
func (s *Surface) latestLocked() int {
	best := -1
	for i := range s.slots {
		if s.slots[i].state == ready && (best < 0 || s.slots[i].gen > s.slots[best].gen) {
			best = i
		}
	}
	return best
}

// Writes the kitty transmission into the free image id, deleting what that id held, placed under the text when placed
func (s *Surface) emitKittyLocked(p *Packet, x, y int, now time.Time) string {
	if p.shm != "" && !s.shared {
		raw, err := shmTake(p.shm)
		delete(s.pending, p.seq)
		if err != nil {
			return ""
		}
		zw, _ := zlib.NewWriterLevel(nil, zlib.BestSpeed)
		var comp bytes.Buffer
		p.first, p.rest = frameChunks(deflate(&comp, zw, raw), s.t.tmux)
		p.shm = ""
	}
	i := s.reuseLocked()
	sl := &s.slots[i]
	id := s.ids[i]
	name := ""
	payload, rest := p.first, p.rest
	if p.shm != "" {
		name = fmt.Sprintf("icebath-%d-%d-%d", os.Getpid(), id, s.stats.Frames+1)
		if err := shmLink(p.shm, name); err != nil {
			return ""
		}
		payload = []byte(base64.StdEncoding.EncodeToString([]byte("/" + name)))
	}
	var b strings.Builder
	b.Grow(len(payload) + len(rest) + 256)
	if s.proto == KittyPlaced {
		b.WriteString("\x1b7")
		b.WriteString(ansi.CursorPosition(x+1, y+1))
	}
	if sl.placed {
		b.WriteString(s.t.wrap(deleteImage(id)))
	}
	b.WriteString(s.t.wrap("\x1b_G" + s.keys(id, p, name != "", len(rest) > 0) + ";" + string(payload) + "\x1b\\"))
	b.Write(rest)
	if s.proto == KittyPlaced {
		b.WriteString("\x1b8")
	}
	s.gen++
	sl.state, sl.promoted, sl.placed, sl.gen, sl.sentAt = flying, false, true, s.gen, now
	sl.cols, sl.rows = p.cols, p.rows
	sl.sent = append(sl.sent, sending{at: now, shm: name})
	if over := len(sl.sent) - sentLimit; over > 0 {
		for _, old := range sl.sent[:over] {
			shmRemove(old.shm)
		}
		sl.sent = append(sl.sent[:0], sl.sent[over:]...)
	}
	s.inflight = i
	return b.String()
}

// Control keys of a transmission, every one asking for an answer with q=0
func (s *Surface) keys(id uint32, p *Packet, shared, more bool) string {
	k := make([]byte, 0, 112)
	k = append(k, "a=T,i="...)
	k = strconv.AppendUint(k, uint64(id), 10)
	k = append(k, ",f=24,s="...)
	k = strconv.AppendInt(k, int64(p.w), 10)
	k = append(k, ",v="...)
	k = strconv.AppendInt(k, int64(p.h), 10)
	k = append(k, ",c="...)
	k = strconv.AppendInt(k, int64(p.cols), 10)
	k = append(k, ",r="...)
	k = strconv.AppendInt(k, int64(p.rows), 10)
	if shared {
		k = append(k, ",t=s,S="...)
		k = strconv.AppendInt(k, int64(p.size), 10)
	} else {
		k = append(k, ",t=d,o=z"...)
	}
	if s.proto == KittyCells {
		k = append(k, ",U=1"...)
	} else {
		k = append(k, ",p=1,C=1,z=-1"...)
	}
	k = append(k, ",q=0"...)
	if more {
		k = append(k, ",m=1"...)
	}
	return string(k)
}

// Kitty command deleting an image and all its placements, answered by nothing
func deleteImage(id uint32) string {
	return "\x1b_Ga=d,d=I,i=" + strconv.FormatUint(uint64(id), 10) + ",q=2\x1b\\"
}

// Writes each tile at its cell under cursor save and restore, sixel keeping the cursor beside the image
func (s *Surface) emitCursorLocked(p *Packet, x, y int) string {
	n := 64
	for _, tl := range p.tiles {
		n += len(tl.data) + 16
	}
	var b strings.Builder
	b.Grow(n)
	b.WriteString("\x1b7")
	if s.proto == Sixel {
		b.WriteString("\x1b[?8452h")
	}
	for _, tl := range p.tiles {
		b.WriteString(ansi.CursorPosition(x+tl.col+1, y+tl.row+1))
		b.Write(tl.data)
	}
	if s.proto == Sixel {
		b.WriteString("\x1b[?8452l")
	}
	b.WriteString("\x1b8")
	s.box = box{x: x, y: y, cols: p.cols, rows: p.rows, set: true}
	return b.String()
}

// Outcome of a kitty answer to one of the images of a surface
type answered struct {
	repaint, refused, broken bool
}

// Takes the answer to the oldest outstanding transmission into the image id
func (s *Surface) answer(id uint32, ok bool, now time.Time) (r answered) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := -1
	for k, v := range s.ids {
		if v == id {
			i = k
		}
	}
	if i < 0 || len(s.slots[i].sent) == 0 {
		return r
	}
	sl := &s.slots[i]
	tx := sl.sent[0]
	sl.sent = sl.sent[1:]
	latest := len(sl.sent) == 0
	named := s.latestLocked() == i
	if ok {
		lat := now.Sub(tx.at)
		s.stats.Acked++
		s.stats.Latency = lat
		s.fails = 0
		if lat < ackTimeout {
			s.timeouts = 0
		}
		s.adaptLocked(lat)
		if latest && (sl.state == flying || sl.promoted) {
			r.repaint = s.proto == KittyCells && sl.state == flying
			sl.state, sl.promoted = ready, false
			if s.inflight == i {
				s.inflight = -1
			}
		}
		return r
	}
	if now.Sub(tx.at) < ackTimeout {
		s.timeouts = 0
	}
	if tx.shm != "" {
		shmRemove(tx.shm)
	}
	if latest && sl.state != empty {
		sl.state, sl.promoted = empty, false
		if s.inflight == i {
			s.inflight = -1
		}
		r.repaint = s.proto == KittyCells && named
	}
	if tx.shm != "" {
		r.refused = true
		s.syncLocked(false)
		return r
	}
	s.stats.Failed++
	s.fails++
	r.broken = s.fails >= brokenAfter
	return r
}

// Raises the divisor on a slow answer and lowers it after a run of fast ones, the run doubling every time a rise undid a lowering
func (s *Surface) adaptLocked(lat time.Duration) {
	target := s.interval()
	switch {
	case lat > target*5/4:
		if s.divisor < maxDivisor && s.lowered {
			s.bounces = min(s.bounces+1, maxBounces)
		}
		s.divisor = min(s.divisor*1.25, maxDivisor)
		s.fast, s.lowered = 0, false
	case lat < target/2:
		s.fast++
		if s.fast >= fastFrames<<s.bounces {
			if s.divisor > 1 {
				s.lowered = true
			}
			s.divisor = max(s.divisor*0.85, 1)
			s.fast = 0
		}
	default:
		s.fast = 0
	}
}

// Fills the cells of the box as the image needs them under it
func (s *Surface) Paint(buf *uv.Buffer, x, y, cols, rows int, bg color.Color) {
	switch s.proto {
	case KittyPlaced:
		fill(buf, x, y, cols, rows, uv.Cell{Content: " ", Width: 1})
		return
	case Sixel, ITerm, None:
		fill(buf, x, y, cols, rows, uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}})
		return
	}
	s.mu.Lock()
	var id uint32
	gc, gr := 0, 0
	if i := s.latestLocked(); i >= 0 {
		id = s.ids[i]
		gc, gr = min(s.slots[i].cols, cols), min(s.slots[i].rows, rows)
	}
	grid := s.gridLocked(gc, gr, id>>24)
	s.mu.Unlock()
	blank := uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}}
	fg := color.RGBA{R: uint8(id >> 16), G: uint8(id >> 8), B: uint8(id), A: 0xff}
	for r := range rows {
		for c := range cols {
			if r < gr && c < gc {
				buf.SetCell(x+c, y+r, &uv.Cell{Content: grid[r][c], Width: 1, Style: uv.Style{Fg: fg, Bg: bg}})
				continue
			}
			buf.SetCell(x+c, y+r, &blank)
		}
	}
}

// Placeholder text of every cell of a grid, kept until the grid or the high byte of the id changes
func (s *Surface) gridLocked(cols, rows int, msb uint32) [][]string {
	if len(s.grid) == rows && (rows == 0 || len(s.grid[0]) == cols) && s.gridMSB == msb {
		return s.grid
	}
	g := make([][]string, rows)
	for r := range rows {
		g[r] = make([]string, cols)
		for c := range cols {
			var sb strings.Builder
			sb.WriteRune(kitty.Placeholder)
			sb.WriteRune(kitty.Diacritic(r))
			if c < diacritics {
				sb.WriteRune(kitty.Diacritic(c))
				if msb != 0 {
					sb.WriteRune(kitty.Diacritic(int(msb)))
				}
			}
			g[r][c] = sb.String()
		}
	}
	s.grid, s.gridMSB = g, msb
	return g
}

// Writes one cell over every cell of a box
func fill(buf *uv.Buffer, x, y, cols, rows int, c uv.Cell) {
	for r := range rows {
		for k := range cols {
			buf.SetCell(x+k, y+r, &c)
		}
	}
}

// Reports whether a cell is a kitty placeholder, which keeps its colors as they name the image
func Placeholder(c *uv.Cell) bool {
	return c != nil && strings.HasPrefix(c.Content, string(kitty.Placeholder))
}

// Escape text that removes every image of the surface from the terminal, and forgets the inflight state
func (s *Surface) Release() string {
	s.mu.Lock()
	var out string
	switch {
	case isKitty(s.proto):
		out = s.t.wrap(deleteImage(s.ids[0])) + s.t.wrap(deleteImage(s.ids[1]))
	case s.box.set:
		var b strings.Builder
		b.WriteString("\x1b7\x1b[0m")
		for r := range s.box.rows {
			b.WriteString(ansi.CursorPosition(s.box.x+1, s.box.y+r+1))
			b.WriteString(strings.Repeat(" ", s.box.cols))
		}
		b.WriteString("\x1b8")
		out = b.String()
	}
	for k, name := range s.pending {
		shmRemove(name)
		delete(s.pending, k)
	}
	s.slots, s.inflight, s.box = [2]slot{}, -1, box{}
	s.floor = s.seq.Load()
	s.mu.Unlock()
	if isKitty(s.proto) {
		s.t.unregister(s)
		for _, id := range s.ids {
			shmSweep(fmt.Sprintf("icebath-%d-%d-", os.Getpid(), id))
		}
	}
	return out
}

// Numbers for a status line
func (s *Surface) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Divisor = s.divisor
	return st
}

// Removes every shared memory object this process left behind
func Sweep() { shmSweep(fmt.Sprintf("icebath-%d-", os.Getpid())) }
