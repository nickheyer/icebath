package gateway

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python str whitespace as a regexp class body
const pyWhitespace = `\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// Markdown and HTML image tags with an http URL and the URL markers that make a markdown link an image
var (
	MDImageTagPattern   = regexp.MustCompile(`!\[([^\]]*)\]\((https?://[^` + pyWhitespace + `\)]+)\)`)
	HTMLImageTagPattern = regexp.MustCompile(`<img[` + pyWhitespace + `]+src=["']?(https?://[^` + pyWhitespace + `"'<>]+)["']?[` + pyWhitespace + `]*/?>[` + pyWhitespace + `]*(?:</img>)?`)
	imageURLMarkers     = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", "fal.media", "fal-cdn", "replicate.delivery"}
	threeNewlinesRE     = regexp.MustCompile(`\n{3,}`)
)

// One image URL found in a response with its alt text
type ImageRef struct {
	URL string
	Alt string
}

// Reports whether the URL points at an animated GIF
func isAnimationURL(rawURL string) bool {
	return strings.HasSuffix(strings.SplitN(pystr.Lower(rawURL), "?", 2)[0], ".gif")
}

// Markdown images whose URL looks like an image and every HTML img URL, with only those tags removed from the text
func ExtractImages(content string) ([]ImageRef, string) {
	var images []ImageRef
	for _, m := range MDImageTagPattern.FindAllStringSubmatch(content, -1) {
		lowered := pystr.Lower(m[2])
		for _, marker := range imageURLMarkers {
			if strings.HasSuffix(lowered, marker) || strings.Contains(lowered, marker) {
				images = append(images, ImageRef{URL: m[2], Alt: m[1]})
				break
			}
		}
	}
	for _, m := range HTMLImageTagPattern.FindAllStringSubmatch(content, -1) {
		images = append(images, ImageRef{URL: m[1]})
	}
	if len(images) == 0 {
		return images, content
	}
	extracted := map[string]bool{}
	for _, img := range images {
		extracted[img.URL] = true
	}
	cleaned := content
	for _, pattern := range []struct {
		re    *regexp.Regexp
		group int
	}{{MDImageTagPattern, 2}, {HTMLImageTagPattern, 1}} {
		cleaned = pattern.re.ReplaceAllStringFunc(cleaned, func(match string) string {
			if extracted[pattern.re.FindStringSubmatch(match)[pattern.group]] {
				return ""
			}
			return match
		})
	}
	return images, pystr.Strip(threeNewlinesRE.ReplaceAllString(cleaned, "\n\n"))
}

// Final code fence state and language after walking the lines and toggling on each fence line
func FenceStateAfter(text string, inCode bool, lang string) (bool, string) {
	for _, line := range strings.Split(text, "\n") {
		stripped := pystr.Strip(line)
		if !strings.HasPrefix(stripped, "```") {
			continue
		}
		if inCode {
			inCode, lang = false, ""
			continue
		}
		inCode, lang = true, ""
		if tag := pystr.SplitWhitespace(stripped[3:], -1); len(tag) > 0 {
			lang = tag[0]
		}
	}
	return inCode, lang
}

// Last rune index of the needle within s before the end bound, or minus one
func runeRfind(s []rune, needle rune, end int) int {
	if end > len(s) {
		end = len(s)
	}
	for i := end - 1; i >= 0; i-- {
		if s[i] == needle {
			return i
		}
	}
	return -1
}

// Splits a long message into chunks that close and reopen split code fences and carry part indicators
func TruncateMessage(content string, maxLength int, lenFn func(string) int) []string {
	measure := lenFn
	if measure == nil {
		measure = utf8.RuneCountInString
	}
	if measure(content) <= maxLength {
		return []string{content}
	}
	const indicatorReserve = 10
	const fenceClose = "\n```"
	var chunks []string
	remaining := []rune(content)
	var carryLang *string
	for len(remaining) > 0 {
		prefix := ""
		if carryLang != nil {
			prefix = "```" + *carryLang + "\n"
		}
		headroom := maxLength - indicatorReserve - measure(prefix) - measure(fenceClose)
		if headroom < 1 {
			headroom = max(1, maxLength/2)
		}
		if measure(prefix)+measure(string(remaining)) <= maxLength-indicatorReserve {
			finalChunk := prefix + string(remaining)
			if carryLang != nil {
				if open, _ := FenceStateAfter(string(remaining), true, *carryLang); open {
					finalChunk += fenceClose
				}
			}
			chunks = append(chunks, finalChunk)
			break
		}
		cpLimit := headroom
		if lenFn != nil {
			cpLimit = customUnitToCP(remaining, headroom, lenFn)
		}
		region := remaining[:min(cpLimit, len(remaining))]
		splitAt := runeRfind(region, '\n', len(region))
		if splitAt < floorDiv(cpLimit, 2) {
			splitAt = runeRfind(region, ' ', len(region))
		}
		if splitAt < 1 {
			splitAt = max(1, cpLimit)
		}
		splitAt = min(splitAt, len(remaining))
		candidate := remaining[:splitAt]
		candText := string(candidate)
		if (strings.Count(candText, "`")-strings.Count(candText, "\\`"))%2 == 1 {
			lastBT := runeRfind(candidate, '`', len(candidate))
			for lastBT > 0 && candidate[lastBT-1] == '\\' {
				lastBT = runeRfind(candidate, '`', lastBT)
			}
			if lastBT > 0 {
				safeSplit := max(runeRfind(candidate, ' ', lastBT), runeRfind(candidate, '\n', lastBT))
				if safeSplit > floorDiv(cpLimit, 4) {
					splitAt = safeSplit
				}
			}
		}
		chunkBody := string(remaining[:splitAt])
		remaining = []rune(pystr.Lstrip(string(remaining[splitAt:])))
		carried := carryLang != nil
		carriedLang := ""
		if carried {
			carriedLang = *carryLang
		}
		inCode, lang := FenceStateAfter(chunkBody, carried, carriedLang)
		carryLang = nil
		if inCode {
			carryLang = &lang
			chunks = append(chunks, prefix+chunkBody+fenceClose)
		} else {
			chunks = append(chunks, prefix+chunkBody)
		}
	}
	if len(chunks) > 1 {
		for i, chunk := range chunks {
			chunks[i] = fmt.Sprintf("%s (%d/%d)", chunk, i+1, len(chunks))
		}
	}
	return chunks
}

// Python floor division of an int by a positive divisor
func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}

// Optional adapter hook fixing the status thread of a turn on its own triggering message
type StatusMetadataForTurner interface {
	StatusMetadataForTurn(chatID, anchor string, metadata map[string]any) map[string]any
}

// Typing metadata of a turn fixed at its start, through the adapter status hook when it has one
func typingMetadataForTurn(adapter any, chatID string, anchor *string, metadata map[string]any) map[string]any {
	hook, ok := adapter.(StatusMetadataForTurner)
	if anchor == nil || !ok {
		return metadata
	}
	return hook.StatusMetadataForTurn(chatID, *anchor, metadata)
}
