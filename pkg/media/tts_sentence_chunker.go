package media

import (
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	sentenceBoundaryRE = regexp2.MustCompile(`(?<=[.!?])(?:\s|\n)|(?:\n\n)`, regexp2.None)
	thinkBlockRE       = regexp2.MustCompile(`<(`+thinkNames()+`)[\s>].*?</\1>`, regexp2.Singleline|regexp2.IgnoreCase)
	thinkOpenRE        = regexp2.MustCompile(`<(?:`+thinkNames()+`)(?=[\s>]|$)`, regexp2.IgnoreCase)
)

func thinkNames() string {
	names := make([]string, len(provider.ThinkTagNames))
	for i, name := range provider.ThinkTagNames {
		names[i] = regexp2.Escape(name)
	}
	return strings.Join(names, "|")
}

// SentenceChunker cuts streamed LLM text into speakable sentences, dropping think blocks and merging short heads
type SentenceChunker struct {
	MinLen int
	buf    string
}

// NewSentenceChunker merges fragments shorter than minLen into the next sentence
func NewSentenceChunker(minLen int) *SentenceChunker { return &SentenceChunker{MinLen: minLen} }

// SentenceChunkerFromConfig honours tts.streaming.min_len with a floor of 1, the default 20 for a bad value
func SentenceChunkerFromConfig(ttsConfig map[string]any) *SentenceChunker {
	streaming := ttsConfig["streaming"]
	if !pyTruthy(streaming) {
		return NewSentenceChunker(20)
	}
	section, ok := asStrMap(streaming)
	if !ok {
		return NewSentenceChunker(20)
	}
	raw, ok := section["min_len"]
	if !ok {
		raw = 20
	}
	n, err := pyint.Int64(raw)
	if err != nil {
		return NewSentenceChunker(20)
	}
	return NewSentenceChunker(max(1, int(n)))
}

func stripThinkBlocks(text string) string {
	out, err := thinkBlockRE.Replace(text, "", -1, -1)
	if err != nil {
		return text
	}
	return out
}

// Feed absorbs a delta and returns every complete sentence ready to speak, none while a think tag is open
func (c *SentenceChunker) Feed(delta string) []string {
	c.buf = stripThinkBlocks(c.buf + delta)
	if m, _ := thinkOpenRE.FindStringMatch(c.buf); m != nil {
		return []string{}
	}
	out := []string{}
	start := 0
	for {
		runes := []rune(c.buf)
		m, _ := sentenceBoundaryRE.FindRunesMatchStartingAt(runes, start)
		if m == nil {
			return out
		}
		end := m.RuneIndex + m.RuneLength
		head := string(runes[:end])
		if utf8.RuneCountInString(pystr.Strip(head)) < c.MinLen {
			start = end
			continue
		}
		out = append(out, head)
		c.buf = string(runes[end:])
		start = 0
	}
}

// Flush drains the tail, never speaking an unterminated reasoning block
func (c *SentenceChunker) Flush() []string {
	tail := stripThinkBlocks(c.buf)
	c.buf = ""
	if m, _ := thinkOpenRE.FindStringMatch(tail); m != nil {
		tail = string([]rune(tail)[:m.RuneIndex])
	}
	if tail = pystr.Strip(tail); tail != "" {
		return []string{tail}
	}
	return []string{}
}
