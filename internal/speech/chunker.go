package speech

import (
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/transport"
)

var (
	// End of a sentence of speech text, where a voice may stop: its mark and the space after it
	sentenceEnd = pattern(`(?<=[.!?]["'\u2019\u201d)\]]*)\s+`)
	// Raw text that may end a sentence of the speech: a sentence mark, closing quotes or brackets after it, before a space, or a new line
	boundary = pattern("[.!?][\"'\u2019\u201d)\\]*_~`]*\\s|\n")
)

func thinkNames() string {
	names := make([]string, len(transport.ThinkTags))
	for i, name := range transport.ThinkTags {
		names[i] = regexp2.Escape(name)
	}
	return strings.Join(names, "|")
}

// Reads a streamed reply out as speech text a sentence at a time, sentences shorter than the least merged into the next. The reply is never
// cleaned piece by piece: each time a sentence may have ended, the whole reply so far is cleaned as Spoken cleans it, to its last space, and
// the sentences that cleaning completes beyond what was handed out are handed out, a sentence once the word after it came, since only
// that word settles how it ends. A span of markup the cut falls inside is taken out as Spoken takes out one that never closes, so no
// markup is read whatever the cut, and the speech of a reply streamed in pieces is the speech Spoken gives the whole reply, cut at the
// same places
type Chunker struct {
	Least int
	raw   string
	// End of the raw text cleaned last, and where the next look for the end of a sentence starts: the last word of that text
	cleaned, from int
	// Speech text handed out so far
	said string
}

func NewChunker(least int) *Chunker { return &Chunker{Least: max(1, least)} }

// Absorbs a delta and answers every sentence of speech it completes
func (c *Chunker) Feed(delta string) []string {
	c.raw += delta
	end := strings.LastIndexAny(c.raw, " \t\n") + 1
	if end <= c.cleaned || !matches(boundary, c.raw[c.from:end]) {
		return nil
	}
	c.cleaned = end
	c.from = strings.LastIndexAny(strings.TrimRight(c.raw[:end], " \t\n"), " \t\n") + 1
	return c.sentences(c.rest(Spoken(c.raw[:end], 0)))
}

// Hands out the speech held back but its last word when more than over characters of it wait, for a reply that pauses mid-sentence
func (c *Chunker) Spill(over int) []string {
	end := strings.LastIndexAny(c.raw, " \t\n") + 1
	rest := c.rest(Spoken(c.raw[:end], 0))
	if utf8.RuneCountInString(strings.TrimSpace(rest)) <= over {
		return nil
	}
	cut := strings.LastIndexAny(strings.TrimRight(rest, " \t\n"), " \t\n")
	if cut <= 0 {
		return nil
	}
	c.said += rest[:cut]
	if piece := strings.TrimSpace(rest[:cut]); piece != "" {
		return []string{piece}
	}
	return nil
}

// Answers the rest of the speech of the whole reply
func (c *Chunker) Flush() []string {
	rest := c.rest(Spoken(c.raw, 0))
	c.raw, c.cleaned, c.from, c.said = "", 0, 0, ""
	if piece := strings.TrimSpace(rest); piece != "" {
		return []string{piece}
	}
	return nil
}

// Speech of the reply so far beyond what was handed out. A cleaning that differs in what was handed out goes on after as much of it
func (c *Chunker) rest(clean string) string {
	if rest, ok := strings.CutPrefix(clean, c.said); ok {
		return rest
	}
	runes, said := []rune(clean), utf8.RuneCountInString(c.said)
	if said >= len(runes) {
		return ""
	}
	c.said = string(runes[:said])
	return string(runes[said:])
}

// The sentences the speech completes, each of at least the least characters, what follows the last held back
func (c *Chunker) sentences(rest string) []string {
	var out []string
	runes := []rune(rest)
	last := 0
	m, _ := sentenceEnd.FindRunesMatch(runes)
	for ; m != nil; m, _ = sentenceEnd.FindNextMatch(m) {
		end := m.RuneIndex + m.RuneLength
		piece := strings.TrimSpace(string(runes[last:end]))
		if utf8.RuneCountInString(piece) < c.Least {
			continue
		}
		out = append(out, piece)
		last = end
	}
	c.said += string(runes[:last])
	return out
}
