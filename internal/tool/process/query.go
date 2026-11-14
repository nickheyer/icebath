package process

import (
	"bytes"
	"fmt"
)

const (
	queryEsc           = 0x1B
	queryMaxModeDigits = 10
	queryMaxBytes      = queryMaxModeDigits + 5
)

type queryAnswer struct {
	query, response []byte
}

type queryResponder struct {
	answers []queryAnswer
	pending []byte
}

func newQueryResponder(rows, cols int) *queryResponder {
	return &queryResponder{answers: []queryAnswer{
		{[]byte("\x1b[5n"), []byte("\x1b[0n")},
		{[]byte("\x1b[18t"), fmt.Appendf(nil, "\x1b[8;%d;%dt", rows, cols)},
		{[]byte("\x1b[6n"), []byte("\x1b[1;1R")},
	}}
}

func asciiDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (q *queryResponder) process(data []byte) (output, responses []byte) {
	if len(q.pending) == 0 && bytes.IndexByte(data, queryEsc) < 0 {
		return data, nil
	}
	for _, b := range data {
		if len(q.pending) == 0 && b != queryEsc {
			output = append(output, b)
			continue
		}
		if b == queryEsc {
			output = append(output, q.pending...)
			q.pending = q.pending[:0]
		}
		q.pending = append(q.pending, b)
		if len(q.pending) == 1 || string(q.pending) == "\x1b[" ||
			q.pending[1] == '[' && (b < 0x40 || b > 0x7E) && len(q.pending) < queryMaxBytes {
			continue
		}
		seq := q.pending
		matched := false
		for _, a := range q.answers {
			if bytes.Equal(seq, a.query) {
				responses = append(responses, a.response...)
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
				len(mode) > 0 && len(mode) <= queryMaxModeDigits && asciiDigits(mode) {
				responses = append(responses, "\x1b[?"...)
				responses = append(responses, mode...)
				responses = append(responses, ";0$y"...)
			} else {
				output = append(output, seq...)
			}
		}
		q.pending = q.pending[:0]
	}
	return output, responses
}

func (q *queryResponder) flush() []byte {
	tail := append([]byte(nil), q.pending...)
	q.pending = q.pending[:0]
	return tail
}
