package weixin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

var (
	headerRE     = regexutil.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
	tableRuleRE  = regexutil.MustCompile(`^\s*\|?(?:\s*:?-{3,}:?\s*\|)+\s*:?-{3,}:?\s*\|?\s*$`)
	fenceRE      = regexutil.MustCompile("^```([^\\n`]*)\\s*$")
	boldLineRE   = regexutil.MustCompile(`^\*\*[^*]+\*\*$`)
	numberedRE   = regexutil.MustCompile(`^\d+\.\s`)
	sendTargetRE = `^\s*((?:wxid|gh|v\d+|wm|wb)_[A-Za-z0-9_-]+|[A-Za-z0-9._-]+@chatroom|filehelper)\s*$`
)

// Whether the pattern matches at the start of the text
func matches(re interface{ MatchString(string) (bool, error) }, text string) bool {
	ok, err := re.MatchString(text)
	return err == nil && ok
}

// One markdown line with whether it is a fence and whether a code block was open before it
type markdownLine struct {
	line        string
	isFence     bool
	inCodeBlock bool
}

// Right stripped lines of the content with their fence state
func walkMarkdownLines(content string) []markdownLine {
	var out []markdownLine
	inCodeBlock := false
	for _, raw := range textutil.Lines(content) {
		line := strings.TrimRightFunc(raw, unicode.IsSpace)
		isFence := matches(fenceRE, strings.TrimSpace(line))
		out = append(out, markdownLine{line, isFence, inCodeBlock})
		if isFence {
			inCodeBlock = !inCodeBlock
		}
	}
	return out
}

// Lines right stripped and blank runs collapsed to one, fenced code left untouched
func normalizeMarkdownBlocks(content string) string {
	var result []string
	blankRun := 0
	for _, l := range walkMarkdownLines(content) {
		if !l.isFence && !l.inCodeBlock && strings.TrimSpace(l.line) == "" {
			blankRun++
			if blankRun <= 1 {
				result = append(result, "")
			}
			continue
		}
		if l.isFence || !l.inCodeBlock {
			blankRun = 0
		}
		result = append(result, l.line)
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

// Long display lines wrapped for copying in WeChat clients, code and table lines kept whole
func wrapCopyFriendlyLinesForWeixin(content string) string {
	if content == "" {
		return content
	}
	options := textutil.WrapOptions{KeepLongWords: true, NoHyphenBreaks: true, KeepWhitespace: true}
	var wrapped []string
	for _, l := range walkMarkdownLines(content) {
		stripped := strings.TrimSpace(l.line)
		keep := l.isFence || l.inCodeBlock || utf8.RuneCountInString(l.line) <= weixinCopyLineWidth || stripped == "" || strings.HasPrefix(stripped, "|")
		if keep || matches(tableRuleRE, stripped) {
			wrapped = append(wrapped, l.line)
			continue
		}
		parts := textutil.Wrap(l.line, weixinCopyLineWidth, options)
		if len(parts) == 0 {
			parts = []string{l.line}
		}
		wrapped = append(wrapped, parts...)
	}
	return strings.TrimSpace(strings.Join(wrapped, "\n"))
}

// Paragraph and fenced code blocks of the content, a fence always starting and ending its own block
func splitMarkdownBlocks(content string) []string {
	var blocks, current []string
	flush := func() {
		if len(current) > 0 {
			blocks = append(blocks, strings.TrimSpace(strings.Join(current, "\n")))
			current = nil
		}
	}
	for _, l := range walkMarkdownLines(content) {
		if l.isFence && !l.inCodeBlock {
			flush()
		}
		if l.isFence || l.inCodeBlock || strings.TrimSpace(l.line) != "" {
			current = append(current, l.line)
		} else {
			flush()
		}
		if l.isFence && l.inCodeBlock {
			flush()
		}
	}
	flush()
	out := []string{}
	for _, block := range blocks {
		if block != "" {
			out = append(out, block)
		}
	}
	return out
}

// Delivery units of the content, top level lines apart, fenced code whole and indented lines joined to the one above
func splitDeliveryUnitsForWeixin(content string) []string {
	var units []string
	for _, block := range splitMarkdownBlocks(content) {
		lines := textutil.Lines(block)
		if matches(fenceRE, strings.TrimSpace(lines[0])) {
			units = append(units, block)
			continue
		}
		var current []string
		for _, raw := range lines {
			line := strings.TrimRightFunc(raw, unicode.IsSpace)
			if len(current) > 0 && strings.TrimSpace(line) != "" && (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) {
				current = append(current, line)
				continue
			}
			units = append(units, strings.TrimSpace(strings.Join(current, "\n")))
			current = nil
			if strings.TrimSpace(line) != "" {
				current = []string{line}
			}
		}
		units = append(units, strings.TrimSpace(strings.Join(current, "\n")))
	}
	out := []string{}
	for _, unit := range units {
		if unit != "" {
			out = append(out, unit)
		}
	}
	return out
}

// Whether a line reads as a short chat line rather than a list, quote, heading or table row
func looksLikeChattyLineForWeixin(line string) bool {
	stripped := strings.TrimSpace(line)
	if stripped == "" || utf8.RuneCountInString(stripped) > 48 || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return false
	}
	for _, prefix := range []string{">", "-", "*", "【", "#", "|"} {
		if strings.HasPrefix(stripped, prefix) {
			return false
		}
	}
	return !matches(tableRuleRE, stripped) && !matches(boldLineRE, stripped) && !matches(numberedRE, stripped)
}

// Whether a block of two to six chatty lines without a heading first line splits into separate bubbles
func shouldSplitShortChatBlockForWeixin(block string) bool {
	var lines []string
	for _, line := range textutil.Lines(block) {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 || len(lines) > 6 {
		return false
	}
	first := strings.TrimSpace(lines[0])
	if matches(headerRE, first) || (utf8.RuneCountInString(first) <= 24 && (strings.HasSuffix(first, ":") || strings.HasSuffix(first, "："))) {
		return false
	}
	for _, line := range lines {
		if !looksLikeChattyLineForWeixin(line) {
			return false
		}
	}
	return true
}

// Markdown blocks packed greedily under the length cap, an oversized block truncated on its own
func packMarkdownBlocksForWeixin(content string, maxLength int) []string {
	if utf8.RuneCountInString(content) <= maxLength {
		return []string{content}
	}
	overflow := func(block string) []string { return gateway.TruncateMessage(block, maxLength, nil) }
	return gateway.GreedyPackBlocks(splitMarkdownBlocks(content), maxLength, nil, "\n\n", overflow)
}

// Messages a reply is delivered as, one compact message unless it is a short chat exchange or per line splitting is on
func splitTextForWeixinDelivery(content string, maxLength int, splitPerLine bool) []string {
	if content == "" {
		return []string{}
	}
	if splitPerLine {
		if utf8.RuneCountInString(content) <= maxLength && !strings.Contains(content, "\n") {
			return []string{content}
		}
		var chunks []string
		for _, unit := range splitDeliveryUnitsForWeixin(content) {
			packed := []string{unit}
			if utf8.RuneCountInString(unit) > maxLength {
				packed = packMarkdownBlocksForWeixin(unit, maxLength)
			}
			for _, chunk := range packed {
				if chunk != "" {
					chunks = append(chunks, chunk)
				}
			}
		}
		if len(chunks) == 0 {
			return []string{content}
		}
		return chunks
	}
	if utf8.RuneCountInString(content) > maxLength {
		if packed := packMarkdownBlocksForWeixin(content, maxLength); len(packed) > 0 {
			return packed
		}
		return []string{content}
	}
	if shouldSplitShortChatBlockForWeixin(content) {
		return splitDeliveryUnitsForWeixin(content)
	}
	return []string{content}
}

// Items of a message item list that are objects, an error for any other item
func itemObjects(itemList []any) ([]*jsonutil.Object, error) {
	out := make([]*jsonutil.Object, 0, len(itemList))
	for _, raw := range itemList {
		item, ok := jsonutil.AsObject(raw)
		if !ok {
			return nil, fmt.Errorf("item_list entry: expected an object, got %s", anyutil.Kind(raw))
		}
		out = append(out, item)
	}
	return out, nil
}

// Whether an item type is one of the media item types
func isMediaItemType(v any) bool {
	for _, t := range []int64{itemImage, itemVideo, itemFile, itemVoice} {
		if equalsInt(v, t) {
			return true
		}
	}
	return false
}

// Text of a message, the first text item with its quoted reference, else a Weixin voice transcript without audio
func extractText(itemList []any) (string, error) {
	items, err := itemObjects(itemList)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if !equalsInt(item.Value("type"), itemText) {
			continue
		}
		textItem, err := objOrEmpty(item.Value("text_item"))
		if err != nil {
			return "", err
		}
		text := strOf(textItem.Value("text"))
		ref, err := objOrEmpty(item.Value("ref_msg"))
		if err != nil {
			return "", err
		}
		refItem, err := objOrEmpty(ref.Value("message_item"))
		if err != nil {
			return "", err
		}
		if isMediaItemType(refItem.Value("type")) {
			if title := ref.Value("title"); anyutil.IsTrue(title) {
				return strings.TrimSpace("[引用媒体: " + anyutil.String(title) + "]\n" + text), nil
			}
			return strings.TrimSpace("[引用媒体]\n" + text), nil
		}
		if refItem.Len() > 0 {
			var parts []string
			if title := ref.Value("title"); anyutil.IsTrue(title) {
				parts = append(parts, anyutil.String(title))
			}
			quoted, err := extractText([]any{refItem})
			if err != nil {
				return "", err
			}
			if quoted != "" {
				parts = append(parts, quoted)
			}
			if len(parts) > 0 {
				return strings.TrimSpace("[引用: " + strings.Join(parts, " | ") + "]\n" + text), nil
			}
		}
		return text, nil
	}
	for _, item := range items {
		if !equalsInt(item.Value("type"), itemVoice) {
			continue
		}
		voiceItem, err := objOrEmpty(item.Value("voice_item"))
		if err != nil {
			return "", err
		}
		voiceText := strOf(voiceItem.Value("text"))
		if !anyutil.IsTrue(voiceItem.Value("media")) && voiceText != "" {
			return "[Voice transcription provided by Weixin]\n" + voiceText, nil
		}
	}
	return "", nil
}

// Message type by MIME prefix
var mimePrefixTypes = []struct {
	prefix string
	kind   gateway.MessageType
}{{"image/", gateway.MessageTypePhoto}, {"video/", gateway.MessageTypeVideo}, {"audio/", gateway.MessageTypeVoice}}

// Message type of an inbound message from its media types, else a command for slash text and text otherwise
func messageTypeFromMedia(mediaTypes []string, text string) gateway.MessageType {
	for _, p := range mimePrefixTypes {
		for _, m := range mediaTypes {
			if strings.HasPrefix(m, p.prefix) {
				return p.kind
			}
		}
	}
	switch {
	case len(mediaTypes) > 0:
		return gateway.MessageTypeDocument
	case strings.HasPrefix(text, "/"):
		return gateway.MessageTypeCommand
	}
	return gateway.MessageTypeText
}

// Persisted long poll cursor of an account, an empty string when none was saved
func loadSyncBuf(home, accountID string) (any, error) {
	dir, err := accountDir(home)
	if err != nil {
		return nil, err
	}
	data, ok := readJSON(filepath.Join(dir, accountID+".sync.json")).(*jsonutil.Object)
	if !ok {
		return "", nil
	}
	buf, present := data.Get("get_updates_buf")
	if !present {
		return "", nil
	}
	return buf, nil
}

// Persists the long poll cursor of an account
func saveSyncBuf(ctx context.Context, home, accountID, syncBuf string) error {
	dir, err := accountDir(home)
	if err != nil {
		return err
	}
	return config.AtomicJSONWrite(ctx, filepath.Join(dir, accountID+".sync.json"), jsonutil.ObjectOf("get_updates_buf", syncBuf))
}
