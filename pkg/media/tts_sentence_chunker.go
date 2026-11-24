package media

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
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

// StreamingSentenceChunker honours tts.streaming.min_len of the tool env's profile
func StreamingSentenceChunker(ctx context.Context, env *tools.Env) *SentenceChunker {
	return NewSentenceChunker(loadTTSRequest(ctx, env).StreamingMinLen)
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
		if utf8.RuneCountInString(strings.TrimSpace(head)) < c.MinLen {
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
	if tail = strings.TrimSpace(tail); tail != "" {
		return []string{tail}
	}
	return []string{}
}
