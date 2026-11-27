package terminal

import (
	"bytes"
	"fmt"
	"runtime"
)

// Longest numeric mode a DEC private mode query carries, and the longest query handled: ESC [ ? digits $ p
const (
	queryModeDigits = 10
	queryMax        = queryModeDigits + 5
)

// Takes the terminal queries a program on a pseudo terminal sends out of its output and answers them, as a terminal would
type queryResponder struct {
	answers [][2][]byte
	pending []byte
}

// Responder answering size queries with the terminal's rows and columns, nil on Windows whose pseudo console answers them itself
func newQueryResponder(rows, cols int) *queryResponder {
	if runtime.GOOS == "windows" {
		return nil
	}
	return &queryResponder{answers: [][2][]byte{
		{[]byte("\x1b[5n"), []byte("\x1b[0n")},
		{[]byte("\x1b[18t"), []byte(fmt.Sprintf("\x1b[8;%d;%dt", rows, cols))},
		{[]byte("\x1b[6n"), []byte("\x1b[1;1R")},
	}}
}

// Output of the chunk without the queries it answered, and the replies to write back
func (q *queryResponder) process(data []byte) ([]byte, []byte) {
	if q == nil || len(q.pending) == 0 && bytes.IndexByte(data, 0x1b) < 0 {
		return data, nil
	}
	var output, replies []byte
	for _, b := range data {
		if len(q.pending) == 0 && b != 0x1b {
			output = append(output, b)
			continue
		}
		if b == 0x1b {
			output = append(output, q.pending...)
			q.pending = q.pending[:0]
		}
		q.pending = append(q.pending, b)
		if len(q.pending) == 1 || bytes.Equal(q.pending, []byte("\x1b[")) || q.pending[1] == '[' && !(b >= 0x40 && b <= 0x7e) && len(q.pending) < queryMax {
			continue
		}
		seq, matched := q.pending, false
		for _, answer := range q.answers {
			if bytes.Equal(seq, answer[0]) {
				replies, matched = append(replies, answer[1]...), true
				break
			}
		}
		if !matched {
			var mode []byte
			if len(seq) >= 5 {
				mode = seq[3 : len(seq)-2]
			}
			if bytes.HasPrefix(seq, []byte("\x1b[?")) && bytes.HasSuffix(seq, []byte("$p")) && len(mode) > 0 && len(mode) <= queryModeDigits && digits(mode) {
				replies = append(append(append(replies, "\x1b[?"...), mode...), ";0$y"...)
			} else {
				output = append(output, q.pending...)
			}
		}
		q.pending = q.pending[:0]
	}
	return output, replies
}

// Incomplete sequence held back at the end
func (q *queryResponder) flush() []byte {
	if q == nil {
		return nil
	}
	tail := append([]byte(nil), q.pending...)
	q.pending = q.pending[:0]
	return tail
}

func digits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
