package cliui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// One grapheme or escape sequence of a measured text, at a byte offset with a display width
type unit struct {
	at    int
	width int
	space bool
}

// Text measured once so any width breaks it into rows without measuring it again
type measured struct {
	text  string
	units []unit
	width int
	word  int
}

// Measures every grapheme of a text, its whole width and the width of its longest word
func measure(text string) measured {
	m := measured{text: text}
	var state byte
	run := 0
	for at := 0; at < len(text); {
		seq, width, n, next := ansi.DecodeSequence(text[at:], state, nil)
		space := seq == " "
		m.units = append(m.units, unit{at: at, width: width, space: space})
		m.width += width
		if space {
			run = 0
		} else {
			run += width
			m.word = max(m.word, run)
		}
		at += max(n, 1)
		state = next
	}
	return m
}

// Same text with its spaces counted as letters, so rows break only where the width runs out
func (m measured) solid() measured {
	units := make([]unit, len(m.units))
	for i, u := range m.units {
		units[i] = unit{at: u.at, width: u.width}
	}
	return measured{text: m.text, units: units, width: m.width, word: m.width}
}

// Calls emit with the byte span of each row, broken at spaces to fit width and inside longer words
func (m measured) each(width int, emit func(from, to int)) {
	width = max(width, 1)
	if m.width <= width {
		if len(m.text) > 0 {
			emit(0, len(m.text))
		}
		return
	}
	offset := func(i int) int {
		if i < len(m.units) {
			return m.units[i].at
		}
		return len(m.text)
	}
	for i := 0; i < len(m.units); {
		used, gap, word, j := 0, -1, false, i
		for ; j < len(m.units) && used+m.units[j].width <= width; j++ {
			if !m.units[j].space {
				word = true
			} else if word {
				gap = j
			}
			used += m.units[j].width
		}
		stop := j
		switch {
		case j == len(m.units) || m.units[j].space:
		case gap >= 0:
			stop = gap
		case j == i:
			stop = i + 1
		}
		last := stop
		for last > i && m.units[last-1].space {
			last--
		}
		if last > i {
			emit(m.units[i].at, offset(last))
		}
		for i = stop; i < len(m.units) && m.units[i].space; i++ {
		}
	}
}

// Rows the text breaks into at a width
func (m measured) rows(width int) []string {
	var out []string
	m.each(width, func(from, to int) { out = append(out, m.text[from:to]) })
	return out
}

// Number of rows the text takes at a width
func (m measured) height(width int) int {
	n := 0
	m.each(width, func(int, int) { n++ })
	return n
}

// Run of text drawn in one style
type span struct {
	text  string
	style textStyle
}

// Measures styled runs as one joined text
func measureSpans(spans []span) measured {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.text)
	}
	return measure(b.String())
}

// Painted rows of styled runs broken to fit width, m being the measure of the runs joined
func (c *Console) spanRows(m measured, spans []span, width int) []string {
	var out []string
	m.each(width, func(from, to int) {
		var b strings.Builder
		at := 0
		for _, s := range spans {
			if lo, hi := max(from, at), min(to, at+len(s.text)); lo < hi {
				b.WriteString(c.paint(s.text[lo-at:hi-at], s.style))
			}
			at += len(s.text)
		}
		out = append(out, b.String())
	})
	return out
}

// Painted rows of a text in one style broken to fit width
func (c *Console) textRows(text string, style textStyle, width int) []string {
	return c.spanRows(measure(text), []span{{text, style}}, width)
}

// Rows with the first behind lead and the rest behind hang, one empty row standing in for no rows
func hung(rows []string, lead, hang string) []string {
	if len(rows) == 0 {
		return []string{lead}
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		if i == 0 {
			out[i] = lead + row
		} else {
			out[i] = hang + row
		}
	}
	return out
}
