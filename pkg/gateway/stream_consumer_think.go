package gateway

import (
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Whether a tag at idx of buf starts a block, the start of text or a newline plus whitespace
func (s *GatewayStreamConsumer) atBlockBoundary(buf []rune, idx int) bool {
	accBoundary := s.accumulated == "" || strings.HasSuffix(s.accumulated, "\n")
	if idx == 0 {
		return accBoundary
	}
	preceding := buf[:idx]
	lastNL := lastRuneIndex(preceding, '\n')
	if lastNL == -1 {
		return accBoundary && pystr.Strip(string(preceding)) == ""
	}
	return pystr.Strip(string(preceding[lastNL+1:])) == ""
}

// Index and length of the earliest block boundary opening think tag, minus one and zero when none
func (s *GatewayStreamConsumer) earliestOpenTag(buf, lowerBuf []rune) (int, int) {
	bestIdx, bestLen := -1, 0
	for _, tag := range provider.ThinkOpenTags() {
		start := 0
		for {
			idx := runeIndexFrom(lowerBuf, tag, start)
			if idx == -1 {
				break
			}
			if s.atBlockBoundary(buf, idx) {
				if bestIdx == -1 || idx < bestIdx {
					bestIdx, bestLen = idx, len(tag)
				}
				break
			}
			start = idx + 1
		}
	}
	return bestIdx, bestLen
}

// Appends a delta to the buffer discarding think blocks, partial tags held until they resolve
func (s *GatewayStreamConsumer) filterAndAccumulate(text string) {
	buf := []rune(s.thinkBuffer + text)
	s.thinkBuffer = ""
	for len(buf) > 0 {
		lowerBuf := []rune(strings.ToLower(string(buf)))
		if len(lowerBuf) != len(buf) {
			lowerBuf = make([]rune, len(buf))
			for i, r := range buf {
				lowerBuf[i] = unicode.ToLower(r)
			}
		}
		if s.inThinkBlock {
			idx, length := provider.FindFirstThinkTag(buf, provider.ThinkCloseTags())
			if length > 0 {
				s.inThinkBlock = false
				buf = buf[idx+length:]
				continue
			}
			maxTag := 0
			for _, tag := range provider.ThinkCloseTags() {
				maxTag = max(maxTag, len(tag))
			}
			if len(buf) > maxTag {
				s.thinkBuffer = string(buf[len(buf)-maxTag:])
			} else {
				s.thinkBuffer = string(buf)
			}
			return
		}
		idx, length := s.earliestOpenTag(buf, lowerBuf)
		if length > 0 {
			s.appendAccumulated(string(buf[:idx]))
			s.inThinkBlock = true
			buf = buf[idx+length:]
			continue
		}
		if heldBack := provider.MaxPartialThinkSuffix(buf, provider.ThinkOpenTags()); heldBack > 0 {
			s.appendAccumulated(string(buf[:len(buf)-heldBack]))
			s.thinkBuffer = string(buf[len(buf)-heldBack:])
		} else {
			s.appendAccumulated(stripOrphanCloseTags(string(buf)))
		}
		return
	}
}

// Flushes text held back for a possible open tag at stream end
func (s *GatewayStreamConsumer) flushThinkBuffer() {
	if s.thinkBuffer != "" && !s.inThinkBlock {
		s.appendAccumulated(stripOrphanCloseTags(s.thinkBuffer))
		s.thinkBuffer = ""
	}
}

// Close tags with no matching open removed with their trailing whitespace
func stripOrphanCloseTags(text string) string { return provider.StripOrphanThinkCloseTags(text) }
