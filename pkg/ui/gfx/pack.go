package gfx

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
)

// Raw bytes one kitty chunk carries, the 4096 base64 characters the protocol allows
const kittyChunk = 4096 / 4 * 3

// Opaque packed frame
type Packet struct {
	s           *Surface
	seq         uint64
	w, h        int
	cols, rows  int
	first, rest []byte
	shm         string
	size        int
	tiles       []tile
}

// Image placed at a cell offset of the box
type tile struct {
	col, row int
	data     []byte
}

// Serial of shared memory objects made by this process
var shmSerial atomic.Uint64

// Packs a frame of three byte pixels for a box of cells, nil without a protocol or whole pixels
func (s *Surface) Pack(rgb []byte, w, h, cols, rows int) *Packet {
	if s.proto == None || w <= 0 || h <= 0 || cols <= 0 || rows <= 0 || len(rgb) < w*h*3 {
		return nil
	}
	raw := rgb[:w*h*3]
	p := &Packet{s: s, seq: s.seq.Add(1), w: w, h: h, cols: cols, rows: rows}
	switch s.proto {
	case KittyCells, KittyPlaced:
		if s.proto == KittyCells {
			p.rows = min(rows, diacritics)
		}
		if s.t.shared() && s.packShared(p, raw) {
			return p
		}
		s.pack.Lock()
		p.first, p.rest = frameChunks(deflate(&s.comp, s.zw, raw), s.t.tmux)
		s.pack.Unlock()
	case Sixel:
		cw, ch := s.t.Cell()
		registers, gw, gh := s.t.sixelLimits()
		s.pack.Lock()
		p.tiles = s.six.encode(raw, w, h, cols, rows, cw, ch, registers, gw, gh)
		s.pack.Unlock()
		for i := range p.tiles {
			p.tiles[i].data = s.t.wrapBytes(p.tiles[i].data)
		}
	case ITerm:
		s.pack.Lock()
		seq := itermSeq(s.png.encode(raw, w, h), cols, rows)
		s.pack.Unlock()
		p.tiles = []tile{{data: s.t.wrapBytes(seq)}}
	}
	return p
}

// Writes the pixels into a fresh shared memory object the packet names, false when the machine refuses one
func (s *Surface) packShared(p *Packet, raw []byte) bool {
	name := fmt.Sprintf("icebath-%d-0-%d", os.Getpid(), shmSerial.Add(1))
	if err := shmPut(name, raw); err != nil {
		s.t.dropShm()
		return false
	}
	p.shm, p.size = name, len(raw)
	runtime.AddCleanup(p, shmRemove, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staleLocked(p.seq) {
		shmRemove(name)
		return true
	}
	s.pending[p.seq] = name
	return true
}

// Zlib packs the bytes at the fastest level into the buffer
func deflate(buf *bytes.Buffer, zw *zlib.Writer, raw []byte) []byte {
	buf.Reset()
	zw.Reset(buf)
	zw.Write(raw)
	zw.Close()
	return buf.Bytes()
}

// Base64 of the first kitty chunk, and every later chunk framed with only its more flag and wrapped for tmux
func frameChunks(z []byte, tmux bool) (first, rest []byte) {
	enc := base64.StdEncoding
	n := min(len(z), kittyChunk)
	first = make([]byte, enc.EncodedLen(n))
	enc.Encode(first, z[:n])
	if len(z) <= kittyChunk {
		return first, nil
	}
	open, end := "\x1b_G", "\x1b\\"
	if tmux {
		open, end = "\x1bPtmux;\x1b\x1b_G", "\x1b\x1b\\\x1b\\"
	}
	size := 0
	for off := kittyChunk; off < len(z); off += kittyChunk {
		size += len(open) + len("m=1;") + enc.EncodedLen(min(kittyChunk, len(z)-off)) + len(end)
	}
	rest = make([]byte, 0, size)
	for off := kittyChunk; off < len(z); off += kittyChunk {
		m := min(kittyChunk, len(z)-off)
		rest = append(rest, open...)
		if off+m < len(z) {
			rest = append(rest, "m=1;"...)
		} else {
			rest = append(rest, "m=0;"...)
		}
		at := len(rest)
		rest = rest[:at+enc.EncodedLen(m)]
		enc.Encode(rest[at:], z[off:off+m])
		rest = append(rest, end...)
	}
	return first, rest
}

// iTerm2 inline image of a PNG scaled by the terminal over cols by rows cells, leaving the cursor still
func itermSeq(png []byte, cols, rows int) []byte {
	head := "\x1b]1337;File=inline=1;size=" + strconv.Itoa(len(png)) + ";width=" + strconv.Itoa(cols) + ";height=" + strconv.Itoa(rows) + ";preserveAspectRatio=0;doNotMoveCursor=1:"
	n := base64.StdEncoding.EncodedLen(len(png))
	out := make([]byte, len(head)+n+1)
	copy(out, head)
	base64.StdEncoding.Encode(out[len(head):], png)
	out[len(out)-1] = '\a'
	return out
}

// Sequence wrapped for tmux passthrough, the same bytes outside tmux
func (t *Terminal) wrapBytes(seq []byte) []byte {
	if !t.tmux {
		return seq
	}
	out := make([]byte, 0, len(seq)+len(seq)/64+16)
	out = append(out, "\x1bPtmux;"...)
	for _, c := range seq {
		if c == 0x1b {
			out = append(out, 0x1b)
		}
		out = append(out, c)
	}
	return append(out, "\x1b\\"...)
}
