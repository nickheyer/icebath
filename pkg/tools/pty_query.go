package tools

import (
	"bytes"
	"fmt"
)

const ptyEsc = 0x1b

// Longest numeric mode a DEC private mode query may carry
const ptyMaxModeDigits = 10

// Longest handled sequence, ESC [ ? digits $ p
const ptyMaxQueryBytes = ptyMaxModeDigits + 5

// PtyQueryResponder strips the bounded set of terminal queries from PTY output and answers them
type PtyQueryResponder struct {
	responses [][2][]byte
	pending   []byte
}

// NewPtyQueryResponder answers window size queries with the given PTY dimensions
func NewPtyQueryResponder(rows, cols int) *PtyQueryResponder {
	return &PtyQueryResponder{responses: [][2][]byte{
		{[]byte("\x1b[5n"), []byte("\x1b[0n")},
		{[]byte("\x1b[18t"), []byte(fmt.Sprintf("\x1b[8;%d;%dt", rows, cols))},
		{[]byte("\x1b[6n"), []byte("\x1b[1;1R")},
	}}
}

// Process scans a chunk and returns the output with handled queries removed plus the replies to write
func (r *PtyQueryResponder) Process(data []byte) ([]byte, []byte) {
	if len(r.pending) == 0 && bytes.IndexByte(data, ptyEsc) < 0 {
		return data, nil
	}
	var output, replies []byte
	for _, b := range data {
		if len(r.pending) == 0 && b != ptyEsc {
			output = append(output, b)
			continue
		}
		if b == ptyEsc {
			output = append(output, r.pending...)
			r.pending = r.pending[:0]
		}
		r.pending = append(r.pending, b)
		if len(r.pending) == 1 || bytes.Equal(r.pending, []byte("\x1b[")) ||
			(r.pending[1] == '[' && !(b >= 0x40 && b <= 0x7e) && len(r.pending) < ptyMaxQueryBytes) {
			continue
		}
		seq := r.pending
		matched := false
		for _, pair := range r.responses {
			if bytes.Equal(seq, pair[0]) {
				replies = append(replies, pair[1]...)
				matched = true
				break
			}
		}
		if !matched {
			var mode []byte
			if len(seq) >= 5 {
				mode = seq[3 : len(seq)-2]
			}
			if bytes.HasPrefix(seq, []byte("\x1b[?")) && bytes.HasSuffix(seq, []byte("$p")) &&
				len(mode) > 0 && len(mode) <= ptyMaxModeDigits && allDigits(mode) {
				replies = append(replies, []byte("\x1b[?")...)
				replies = append(replies, mode...)
				replies = append(replies, []byte(";0$y")...)
			} else {
				output = append(output, r.pending...)
			}
		}
		r.pending = r.pending[:0]
	}
	return output, replies
}

// Flush returns any incomplete trailing sequence the scanner held back
func (r *PtyQueryResponder) Flush() []byte {
	tail := append([]byte(nil), r.pending...)
	r.pending = r.pending[:0]
	return tail
}

// Reports whether every byte is an ASCII digit
func allDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
