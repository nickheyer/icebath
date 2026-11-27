package speech

import (
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/transport"
)

var (
	sentenceEnd = pattern(`(?<=[.!?])(?:\s|\n)|(?:\n\n)`)
	thinkTagged = regexp2.MustCompile(`<(`+thinkNames()+`)[\s>].*?</\1>`, regexp2.Singleline|regexp2.IgnoreCase)
	thinkOpened = regexp2.MustCompile(`<(?:`+thinkNames()+`)(?=[\s>]|$)`, regexp2.IgnoreCase)
)

func thinkNames() string {
	names := make([]string, len(transport.ThinkTags))
	for i, name := range transport.ThinkTags {
		names[i] = regexp2.Escape(name)
	}
	return strings.Join(names, "|")
}

// Cuts streamed reply text into sentences to speak as they complete, reasoning blocks dropped and sentences shorter than the least merged
// into the next
type Chunker struct {
	Least  int
	buffer string
}

func NewChunker(least int) *Chunker { return &Chunker{Least: max(1, least)} }

// Text held back for the next sentence
func (c *Chunker) Held() string { return c.buffer }

// Absorbs a delta and answers every sentence it completes, none while a reasoning tag is open
func (c *Chunker) Feed(delta string) []string {
	c.buffer = subText(thinkTagged, c.buffer+delta, "")
	if m, _ := thinkOpened.FindStringMatch(c.buffer); m != nil {
		return nil
	}
	var out []string
	start := 0
	for {
		runes := []rune(c.buffer)
		m, _ := sentenceEnd.FindRunesMatchStartingAt(runes, start)
		if m == nil {
			return out
		}
		end := m.RuneIndex + m.RuneLength
		head := string(runes[:end])
		if utf8.RuneCountInString(strings.TrimSpace(head)) < c.Least {
			start = end
			continue
		}
		out = append(out, head)
		c.buffer = string(runes[end:])
		start = 0
	}
}

// Answers the rest, never an unterminated reasoning block
func (c *Chunker) Flush() []string {
	tail := subText(thinkTagged, c.buffer, "")
	c.buffer = ""
	if m, _ := thinkOpened.FindStringMatch(tail); m != nil {
		tail = string([]rune(tail)[:m.RuneIndex])
	}
	if tail = strings.TrimSpace(tail); tail != "" {
		return []string{tail}
	}
	return nil
}
