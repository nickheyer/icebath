package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"golang.org/x/text/width"
)

const slackErrorBodyLimitBytes = 8 * 1024

var boolWords = map[string]bool{"1": true, "0": true, "true": true, "false": true, "yes": true, "no": true, "on": true, "off": true}

// Notice a dead model picker message is rewritten to
func modelPickerExpiredNotice(ctx context.Context) string {
	return i18n.T(ctx, "platform.shared.model_picker_expired", "", nil)
}

// Explicit link preview controls from config, string bools coerced and junk values dropped
func slackUnfurlKwargs(extra map[string]any) map[string]bool {
	kwargs := map[string]bool{}
	for _, key := range []string{"unfurl_links", "unfurl_media"} {
		switch val := extra[key].(type) {
		case bool:
			kwargs[key] = val
		case string:
			word := pystr.Lower(pystr.Strip(val))
			if boolWords[word] {
				kwargs[key] = word == "1" || word == "true" || word == "yes" || word == "on"
			}
		}
	}
	return kwargs
}

// Error body text read in 4 KiB chunks up to limit bytes, the body released once it runs over
func readErrorTextLimited(body io.ReadCloser, limit int) string {
	var data []byte
	total := 0
	buf := make([]byte, 4096)
	for total <= limit {
		size := min(4096, limit+1-total)
		n, err := body.Read(buf[:size])
		if n > 0 {
			data = append(data, buf[:n]...)
			total += n
		}
		if err != nil || n == 0 {
			break
		}
	}
	if total > limit {
		_ = body.Close()
	}
	if len(data) > limit {
		data = data[:limit]
	}
	return pystr.DecodeUTF8Replace(data)
}

// Raw Web API response with its decoded JSON data or the undecoded bytes of a binary body
type webResponse struct {
	Data any
}

// Web API response as a plain object, empty for binary bodies and unknown shapes
func slackResponsePayload(response any) map[string]any {
	switch r := response.(type) {
	case map[string]any:
		return r
	case *webResponse:
		if r != nil {
			if data, ok := r.Data.(map[string]any); ok {
				return data
			}
		}
	}
	return map[string]any{}
}

var fileMarkerNameRe = mustPyRe(`[\r\n\[\]]+`)

// Python str of a value when truthy, empty otherwise
func strOrEmpty(value any) string {
	if pyrepr.Truthy(value) {
		return pyrepr.Str(value)
	}
	return ""
}

// First truthy value under the keys in order, nil when none is truthy
func firstTruthy(mapping map[string]any, keys ...string) any {
	for _, key := range keys {
		if value := mapping[key]; pyrepr.Truthy(value) {
			return value
		}
	}
	return nil
}

// Compact attachment marker with the name stripped of newlines and brackets
func slackFileMarker(fileObj map[string]any) string {
	name := "file"
	if v := firstTruthy(fileObj, "name", "title", "id"); v != nil {
		name = pyrepr.Str(v)
	}
	name = pystr.Strip(replaceAll(fileMarkerNameRe, name, func(reMatch) string { return " " }))
	if name == "" {
		name = "file"
	}
	mimetype := strOrEmpty(fileObj["mimetype"])
	for _, kind := range []string{"image", "video", "audio"} {
		if strings.HasPrefix(mimetype, kind+"/") {
			return "[" + kind + ": " + name + "]"
		}
	}
	if mimetype != "" {
		return "[file: " + name + " (" + mimetype + ")]"
	}
	return "[file: " + name + "]"
}

var tableSeparatorRe = mustPyRe(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*){1,}\|?\s*$`)

// Monospace display width counting East Asian wide and fullwidth characters as two columns
func dispWidth(s string) int {
	n := 0
	for _, r := range s {
		switch width.LookupRune(r).Kind() {
		case width.EastAsianWide, width.EastAsianFullwidth:
			n += 2
		default:
			n++
		}
	}
	return n
}

// Cell right padded with spaces up to the display width
func pad(cell string, w int) string {
	return cell + strings.Repeat(" ", max(w-dispWidth(cell), 0))
}

// Table rows re-emitted with uniform column count, padded cells and a regenerated separator row
func alignTable(rows []string) []string {
	if len(rows) < 2 {
		return rows
	}
	parsed := make([][]string, len(rows))
	nCols := 0
	for i, r := range rows {
		parsed[i] = gateway.SplitMarkdownTableRow(r)
		nCols = max(nCols, len(parsed[i]))
	}
	for i := range parsed {
		for len(parsed[i]) < nCols {
			parsed[i] = append(parsed[i], "")
		}
	}
	parsed[1] = make([]string, nCols)
	for c := range parsed[1] {
		parsed[1][c] = "---"
	}
	widths := make([]int, nCols)
	for c := range nCols {
		for _, r := range parsed {
			widths[c] = max(widths[c], dispWidth(r[c]))
		}
	}
	out := make([]string, 0, len(parsed))
	for idx, row := range parsed {
		cells := make([]string, nCols)
		for c := range nCols {
			if idx == 1 {
				cells[c] = strings.Repeat("-", widths[c])
			} else {
				cells[c] = pad(row[c], widths[c])
			}
		}
		out = append(out, "| "+strings.Join(cells, " | ")+" |")
	}
	return out
}

// GFM pipe tables outside code fences wrapped in fences with aligned columns
func wrapMarkdownTables(text string) string {
	if text == "" || !strings.Contains(text, "|") || !strings.Contains(text, "-") {
		return text
	}
	lines := strings.Split(text, "\n")
	var out []string
	inFence := false
	i := 0
	for i < len(lines) {
		line := lines[i]
		if strings.HasPrefix(pystr.Lstrip(line), "```") {
			inFence = !inFence
		} else if _, sep := searchAt(tableSeparatorRe, lines, i+1); !inFence && strings.Contains(line, "|") && sep {
			block := []string{line, lines[i+1]}
			j := i + 2
			for j < len(lines) && gateway.IsTableRow(lines[j]) {
				block = append(block, lines[j])
				j++
			}
			out = append(out, "```")
			out = append(out, alignTable(block)...)
			out = append(out, "```")
			i = j
			continue
		}
		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n")
}

// Match of the pattern against lines[i], false when i is past the end
func searchAt(re *regexp2.Regexp, lines []string, i int) (reMatch, bool) {
	if i >= len(lines) {
		return reMatch{}, false
	}
	return search(re, lines[i])
}

// Passive registry probe reporting the Slack transport as present since slack-go is compiled in
func slackDepsPresent() bool { return true }

// User mentions authored in non quoted Block Kit text, quoted and forwarded content ignored
func collectSlackBlockMentions(blocks any) []string {
	var mentions []string
	var walk func(node any, inQuote bool)
	walk = func(node any, inQuote bool) {
		switch n := node.(type) {
		case []any:
			for _, item := range n {
				walk(item, inQuote)
			}
		case []map[string]any:
			for _, item := range n {
				walk(item, inQuote)
			}
		case map[string]any:
			nodeType, _ := n["type"].(string)
			quoted := inQuote || nodeType == "rich_text_quote"
			if nodeType == "user" && !quoted && pyrepr.Truthy(n["user_id"]) {
				mentions = append(mentions, "<@"+pyrepr.Str(n["user_id"])+">")
			}
			for _, key := range []string{"elements", "element"} {
				if child, ok := n[key]; ok && child != nil {
					walk(child, quoted)
				}
			}
		}
	}
	walk(blocks, false)
	return mentions
}

// Mention detection text of flat text plus mentions found only in non quoted blocks
func slackMentionDetectionText(event map[string]any) string {
	flat := strOrEmpty(event["text"])
	blocks := event["blocks"]
	var extra []string
	if pyrepr.Truthy(blocks) {
		for _, m := range collectSlackBlockMentions(blocks) {
			if !strings.Contains(flat, m) {
				extra = append(extra, m)
			}
		}
	}
	if len(extra) == 0 {
		return flat
	}
	return pystr.Strip(pystr.Strip(flat) + "\n" + strings.Join(extra, " "))
}

// Whether a name is a gateway command of the registry, installed by the process wiring from pkg/commands
var IsGatewayKnownCommand func(ctx context.Context, name string) bool

// Leading known bang command rewritten to the gateway slash form
func rewriteKnownBangCommand(ctx context.Context, text string) string {
	if !strings.HasPrefix(text, "!") {
		return text
	}
	fields := pystr.SplitWhitespace(text[1:], 1)
	if len(fields) == 0 {
		return text
	}
	cmdName := pystr.Lower(strings.SplitN(fields[0], "@", 2)[0])
	if cmdName != "" && !strings.Contains(cmdName, "/") && IsGatewayKnownCommand(ctx, cmdName) {
		return "/" + text[1:]
	}
	return text
}

// Workspace independent archives tail of a message permalink, empty without channel or ts
func slackPermalinkPath(channelID, messageTS any) string {
	if !pyrepr.Truthy(channelID) || !pyrepr.Truthy(messageTS) {
		return ""
	}
	return "archives/" + pyrepr.Str(channelID) + "/p" + strings.ReplaceAll(pyrepr.Str(messageTS), ".", "")
}

// Whether a Slack API error carries the code as its structured error field
func slackErrorIs(err error, code string) bool {
	return err != nil && slackErrorCode(err) == code
}

// String field of a Block Kit element, empty when it holds a text object or anything else
func slackStrField(el map[string]any, name string) string {
	if s, ok := el[name].(string); ok {
		return s
	}
	return ""
}

type inlineEntity struct{ format, key, def string }

var inlineEntityFormats = map[string]inlineEntity{
	"channel":   {"<#%s>", "channel_id", ""},
	"user":      {"<@%s>", "user_id", ""},
	"usergroup": {"<!subteam^%s>", "usergroup_id", ""},
	"team":      {"<!team^%s>", "team_id", ""},
	"emoji":     {":%s:", "name", ""},
	"broadcast": {"<!%s>", "range", "here"},
}

// One inline rich text element rendered, unknown types falling back to their url and label
func renderSlackInlineElement(el map[string]any) string {
	elType, _ := el["type"].(string)
	switch elType {
	case "text":
		return slackStrField(el, "text")
	case "color":
		return slackStrField(el, "value")
	}
	if entity, ok := inlineEntityFormats[elType]; ok {
		value, present := el[entity.key]
		if !present {
			value = entity.def
		}
		return fmt.Sprintf(entity.format, pyrepr.Str(value))
	}
	if elType == "date" {
		if fallback := slackStrField(el, "fallback"); fallback != "" {
			return fallback
		}
	}
	url := slackStrField(el, "url")
	label := slackStrField(el, "text")
	if label == "" {
		label = slackStrField(el, "fallback")
	}
	if url == "" && elType == "message_mention" {
		url = slackPermalinkPath(el["channel_id"], el["message_ts"])
	}
	if url != "" {
		if label != "" && label != url {
			return label + " (" + url + ")"
		}
		return url
	}
	return label
}

// Inline elements rendered and concatenated
func renderInlineElements(elements []any) string {
	var b strings.Builder
	for _, el := range elements {
		b.WriteString(renderSlackInlineElement(asMap(el)))
	}
	return b.String()
}

// Value as a JSON object, empty when it is anything else
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Value as a JSON array, nil when it is anything else
func asList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out
	}
	return nil
}

// Value under key, def when the key is absent
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Rich text blocks rendered to readable quote, list and code lines and table blocks to pipe rows
func extractTextFromSlackBlocks(blocks []any) string {
	if len(blocks) == 0 {
		return ""
	}
	var parts []string
	appendLine := func(text string, quoteDepth int, bullet string) {
		if text == "" || pystr.Strip(text) == "" {
			return
		}
		prefix := ""
		if quoteDepth > 0 {
			prefix = strings.Repeat(">", quoteDepth) + " "
		}
		parts = append(parts, pystr.Rstrip(prefix+bullet+text))
	}
	var walk func(elements []any, quoteDepth int, bullet string)
	walk = func(elements []any, quoteDepth int, bullet string) {
		for _, raw := range elements {
			elem := asMap(raw)
			elemType, _ := getOr(elem, "type", "").(string)
			switch elemType {
			case "rich_text_section":
				appendLine(renderInlineElements(asList(getOr(elem, "elements", []any{}))), quoteDepth, bullet)
			case "rich_text_quote":
				walk(asList(getOr(elem, "elements", []any{})), quoteDepth+1, "")
			case "rich_text_list":
				style := elem["style"]
				for idx, item := range asList(getOr(elem, "elements", []any{})) {
					itemBullet := fmt.Sprintf("%d. ", idx+1)
					if style == "bullet" {
						itemBullet = "• "
					}
					walk([]any{item}, quoteDepth, itemBullet)
				}
			case "rich_text_preformatted":
				var codeLines []string
				for _, rawChild := range asList(getOr(elem, "elements", []any{})) {
					child := asMap(rawChild)
					var line string
					if childType, _ := getOr(child, "type", "").(string); childType == "rich_text_section" {
						line = renderInlineElements(asList(getOr(child, "elements", []any{})))
					} else {
						line = renderInlineElements([]any{child})
					}
					if line != "" {
						codeLines = append(codeLines, line)
					}
				}
				if codeText := strings.Join(codeLines, "\n"); codeText != "" {
					lang := pyrepr.Str(getOr(elem, "language", ""))
					appendLine("```"+lang+"\n"+codeText+"\n```", quoteDepth, bullet)
				}
			default:
				appendLine(renderInlineElements([]any{elem}), quoteDepth, bullet)
			}
		}
	}
	for _, raw := range blocks {
		block := asMap(raw)
		switch block["type"] {
		case "rich_text":
			walk(asList(getOr(block, "elements", []any{})), 0, "")
		case "table":
			if tableText := renderSlackTableBlock(block, slackTableMaxChars); tableText != "" {
				parts = append(parts, tableText)
			}
		}
	}
	return strings.Join(parts, "\n")
}

const slackTableMaxChars = 20000

// Keys of a JSON object in iteration order
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Text leaves of a table cell subtree joined with spaces
func collectSlackTableCellText(value any) string {
	var parts []string
	var visit func(node any)
	visit = func(node any) {
		switch n := node.(type) {
		case []any:
			for _, item := range n {
				visit(item)
			}
		case map[string]any:
			if text, ok := n["text"].(string); ok {
				parts = append(parts, text)
			}
			for _, k := range sortedKeys(n) {
				visit(n[k])
			}
		}
	}
	visit(value)
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return pystr.Strip(strings.Join(kept, " "))
}

// Table block rendered as pipe separated rows, capped at maxChars with a visible truncation marker
func renderSlackTableBlock(block any, maxChars int) string {
	m, ok := block.(map[string]any)
	if !ok {
		return ""
	}
	rows, ok := m["rows"].([]any)
	if !ok {
		return ""
	}
	var lines []string
	for _, rawRow := range rows {
		row, ok := rawRow.([]any)
		if !ok {
			continue
		}
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = collectSlackTableCellText(cell)
		}
		rendered := strings.Join(cells, " | ")
		if pystr.StripChars(rendered, " |") != "" {
			lines = append(lines, rendered)
		}
	}
	text := strings.Join(lines, "\n")
	if text == "" {
		return ""
	}
	if runes := []rune(text); len(runes) > maxChars {
		text = pystr.Rstrip(string(runes[:max(maxChars-20, 0)])) + "\n[table truncated]"
	}
	return text
}

// Readable text of legacy attachments, structured fields first and fallback only when nothing else exists
func extractTextFromSlackAttachments(attachments []any) string {
	if len(attachments) == 0 {
		return ""
	}
	var lines []string
	for _, raw := range attachments {
		att, ok := raw.(map[string]any)
		if !ok || pyrepr.Truthy(att["is_msg_unfurl"]) {
			continue
		}
		var got []string
		for _, key := range []string{"pretext", "title", "text"} {
			if pyrepr.Truthy(att[key]) {
				got = append(got, pyrepr.Str(att[key]))
			}
		}
		for _, rawField := range asList(att["fields"]) {
			if field, ok := rawField.(map[string]any); ok {
				for _, k := range []string{"title", "value"} {
					if pyrepr.Truthy(field[k]) {
						got = append(got, pyrepr.Str(field[k]))
					}
				}
			}
		}
		if pyrepr.Truthy(att["blocks"]) {
			if blockText := extractTextFromSlackBlocks(asList(att["blocks"])); blockText != "" {
				got = append(got, blockText)
			}
		}
		if len(got) == 0 && pyrepr.Truthy(att["fallback"]) {
			got = append(got, pyrepr.Str(att["fallback"]))
		}
		lines = append(lines, got...)
	}
	var kept []string
	for _, line := range lines {
		if line != "" {
			kept = append(kept, line)
		}
	}
	return pystr.Strip(strings.Join(kept, "\n"))
}

var (
	slackEntityLabelRe = mustPyRe(`<([@#!][^>|]*)\|[^>]*>`)
	slackFencedCodeRe  = mustPyRe("(?s)(?<!`)\\n*```[ \\t]*\\n?(.*?)\\n?[ \\t]*```\\n*(?!`)")
	slackInlineCodeRe  = mustPyRe("`([^`\\n]+)`")
	slackDateRe        = mustPyRe(`<!date\^([^>|]*)(?:\|([^>]*))?>`)
	slackPermalinkRe   = mustPyRe(`https?://[^\s/]+/(archives/[A-Za-z0-9]+/p\d+)(?:\?[^\s)]*)?`)
	slackInlineStyleRe = mustPyRe(`([*_~])([^\n]+?)\1`)
	slackWhitespaceRe  = mustPyRe(`\s+`)
	slackHTMLEntityRe  = regexp.MustCompile(`&(amp|lt|gt);`)
	slackHTMLEntities  = map[string]string{"amp": "&", "lt": "<", "gt": ">"}
)

// Flat text with Slack's ampersand and angle bracket escaping undone
func unescapeSlackEntities(text string) string {
	return slackHTMLEntityRe.ReplaceAllStringFunc(text, func(m string) string {
		return slackHTMLEntities[m[1:len(m)-1]]
	})
}

// Slack text reduced to the form rendered rich text takes, for duplicate comparison
func normalizeSlackTextForDedupe(text, botUID string) string {
	canonical := unescapeSlackEntities(text)
	canonical = replaceAll(slackLinkRe, canonical, func(m reMatch) string {
		url, label := m.groups[1], m.groups[2]
		if label != "" && label != url {
			return label + " (" + url + ")"
		}
		return url
	})
	canonical = replaceAll(slackDateRe, canonical, func(m reMatch) string {
		if fallback := m.groups[2]; fallback != "" {
			return fallback
		}
		parts := strings.Split(m.groups[1], "^")
		if len(parts) > 2 {
			return parts[2]
		}
		return ""
	})
	canonical = replaceAll(slackPermalinkRe, canonical, func(m reMatch) string { return m.groups[1] })
	canonical = replaceAll(slackEntityLabelRe, canonical, func(m reMatch) string { return "<" + m.groups[1] + ">" })
	if botUID != "" {
		canonical = strings.ReplaceAll(canonical, "<@"+botUID+">", "")
	}
	canonical = replaceAll(slackFencedCodeRe, canonical, func(m reMatch) string { return m.groups[1] })
	canonical = replaceAll(slackInlineCodeRe, canonical, func(m reMatch) string { return m.groups[1] })
	for {
		unstyled := replaceAll(slackInlineStyleRe, canonical, func(m reMatch) string { return m.groups[2] })
		if unstyled == canonical {
			break
		}
		canonical = unstyled
	}
	return pystr.Strip(replaceAll(slackWhitespaceRe, canonical, func(reMatch) string { return " " }))
}

// Rich text and table content of the blocks not already present in the primary text
func extractAdditionalTextFromSlackBlocks(blocks []any, primaryText, botUID string) string {
	primary := normalizeSlackTextForDedupe(primaryText, botUID)
	primaryFenced := map[string]bool{}
	for _, m := range findAll(slackFencedCodeRe, primaryText) {
		primaryFenced[normalizeSlackTextForDedupe(m.groups[0], botUID)] = true
	}
	var parts []string
	for _, raw := range blocks {
		block := asMap(raw)
		blockType := block["type"]
		if blockType == "table" {
			if tableText := renderSlackTableBlock(block, slackTableMaxChars); tableText != "" {
				parts = append(parts, tableText)
			}
			continue
		}
		if blockType != "rich_text" {
			continue
		}
		for _, rawElement := range asList(getOr(block, "elements", []any{})) {
			element := asMap(rawElement)
			elementType, _ := getOr(element, "type", "").(string)
			rendered := pystr.Strip(extractTextFromSlackBlocks([]any{map[string]any{"type": "rich_text", "elements": []any{element}}}))
			if rendered == "" {
				continue
			}
			normalized := normalizeSlackTextForDedupe(rendered, botUID)
			var duplicate bool
			if elementType == "rich_text_preformatted" {
				duplicate = primaryFenced[normalized]
			} else {
				duplicate = normalized == primary || strings.Contains(primary, normalized)
			}
			if normalized != "" && duplicate {
				continue
			}
			parts = append(parts, rendered)
		}
	}
	return strings.Join(parts, "\n")
}

var (
	blockScalarKeys    = map[string]bool{"type": true, "block_id": true, "action_id": true, "style": true, "dispatch_action": true, "optional": true, "multiple": true, "emoji": true}
	blockRecursiveKeys = map[string]bool{"text": true, "title": true, "description": true, "label": true, "placeholder": true, "accessory": true, "fields": true, "elements": true, "options": true, "option_groups": true, "confirm": true, "submit": true, "close": true, "hint": true}
)

// Whether a sanitized value is one of the empty shapes the dump drops
func emptySanitized(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case *pyjson.Object:
		return t.Len() == 0
	}
	return false
}

// Allowlisted copy of a Block Kit value, scalars kept, containers recursed and empties dropped
func sanitizeBlockValue(value any) any {
	switch t := value.(type) {
	case []any:
		out := []any{}
		for _, v := range t {
			if item := sanitizeBlockValue(v); !emptySanitized(item) {
				out = append(out, item)
			}
		}
		return out
	case map[string]any:
		out := pyjson.NewObject()
		for _, key := range sortedKeys(t) {
			item := t[key]
			if blockScalarKeys[key] {
				out.Set(key, item)
			} else if blockRecursiveKeys[key] {
				if cleaned := sanitizeBlockValue(item); !emptySanitized(cleaned) {
					out.Set(key, cleaned)
				}
			}
		}
		return out
	case nil, string, bool, int, int64, float64, json.Number:
		return value
	}
	return pyrepr.Repr(value)
}

// Compact redacted JSON view of the non rich text, non table blocks, empty when none remain
func serializeSlackBlocksForAgent(blocks []any, maxChars int) string {
	var inspectable []any
	for _, raw := range blocks {
		blockType := asMap(raw)["type"]
		if blockType != "rich_text" && blockType != "table" {
			inspectable = append(inspectable, raw)
		}
	}
	if len(inspectable) == 0 {
		return ""
	}
	payload, err := pyjson.Dumps(sanitizeBlockValue(inspectable), pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		payload = pyrepr.Repr(inspectable)
	}
	payload = tools.Elide(payload, maxChars)
	return "[Slack Block Kit payload for this message]\n```json\n" + payload + "\n```"
}

// Deduplicated http and https URLs of a Block Kit tree in discovery order
func extractURLsFromSlackBlocks(blocks []any) []string {
	if len(blocks) == 0 {
		return nil
	}
	var found []string
	seen := map[string]bool{}
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			for _, key := range []string{"url", "image_url", "external_url"} {
				value, isStr := n[key].(string)
				if isStr && (strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")) && !seen[value] {
					seen[value] = true
					found = append(found, value)
				}
			}
			for _, k := range sortedKeys(n) {
				walk(n[k])
			}
		case []any:
			for _, item := range n {
				walk(item)
			}
		}
	}
	walk(blocks)
	return found
}

// Dedup window for Socket Mode replays, overridable by a positive SLACK_DEDUP_TTL_SECONDS
func slackDedupTTLSeconds(ctx context.Context) float64 {
	raw, _ := gateway.GetScopedSecret(ctx, "SLACK_DEDUP_TTL_SECONDS", "", false)
	if raw != "" {
		value, err := pyint.ParseFloat(raw)
		if err != nil {
			slog.Warn(fmt.Sprintf("[Slack] Invalid SLACK_DEDUP_TTL_SECONDS=%s; using default", pyrepr.StrRepr(raw)))
		} else if value > 0 {
			return value
		}
	}
	return 3600.0
}

var slackAudioMimeToExt = map[string]string{
	"audio/ogg": ".ogg", "audio/opus": ".ogg", "audio/mpeg": ".mp3", "audio/mp3": ".mp3",
	"audio/wav": ".wav", "audio/x-wav": ".wav", "audio/webm": ".webm", "audio/mp4": ".m4a",
	"audio/x-m4a": ".m4a", "audio/m4a": ".m4a", "audio/aac": ".m4a", "audio/flac": ".flac",
	"audio/x-flac": ".flac",
}

var slackSTTSupportedExts = map[string]bool{
	".mp3": true, ".mp4": true, ".mpeg": true, ".mpga": true, ".m4a": true, ".wav": true,
	".webm": true, ".ogg": true, ".aac": true, ".flac": true,
}

// Cache extension matching inbound audio bytes, from the filename, then the mimetype, else m4a
func resolveSlackAudioExt(fileObj map[string]any, mimetype string) string {
	_, ext := pypath.Splitext(pystr.Strip(strOrEmpty(fileObj["name"])))
	nameExt := pystr.Lower(ext)
	if slackSTTSupportedExts[nameExt] {
		return nameExt
	}
	mimeKey := pystr.Lower(pystr.Strip(strings.SplitN(mimetype, ";", 2)[0]))
	if ext, ok := slackAudioMimeToExt[mimeKey]; ok {
		return ext
	}
	return ".m4a"
}

// Whether a file is an audio only voice clip by its slack_audio subtype or audio_message name
func isSlackVoiceClip(fileObj map[string]any) bool {
	return pystr.Lower(pystr.Strip(strOrEmpty(fileObj["subtype"]))) == "slack_audio" ||
		strings.HasPrefix(pystr.Lower(pystr.Strip(strOrEmpty(fileObj["name"]))), "audio_message")
}

// Human label of a Slack file object for user facing diagnostics
func attachmentLabel(fileObj map[string]any) string {
	if v := firstTruthy(fileObj, "name", "id"); v != nil {
		return pyrepr.Str(v)
	}
	return "this attachment"
}

const slackUnfurlBlocksMaxChars = 6000

// Text with link unfurl previews appended, message unfurls skipped and nested blocks under one shared budget
func appendLinkUnfurls(text string, slackAttachments []any) string {
	var attParts []string
	blocksBudget := slackUnfurlBlocksMaxChars
	for _, raw := range slackAttachments {
		att := asMap(raw)
		attTitle := strOrEmpty(att["title"])
		attURL := strOrEmpty(firstTruthy(att, "title_link", "from_url"))
		attText := strOrEmpty(att["text"])
		attFooter := strOrEmpty(att["footer"])
		attFallback := strOrEmpty(att["fallback"])
		if pyrepr.Truthy(att["is_msg_unfurl"]) {
			continue
		}
		header := ""
		if attTitle != "" && attURL != "" {
			header = "📎 [" + attTitle + "](" + attURL + ")"
		} else if attTitle != "" || attURL != "" {
			header = "📎 " + attTitle + attURL
		}
		body := attText
		if body == "" {
			body = attFallback
		}
		body = pystr.Strip(body)
		if runes := []rune(body); len(runes) > 500 {
			body = string(runes[:497]) + "..."
		}
		nestedText := ""
		if blocksBudget > 0 {
			nestedText = extractTextFromSlackBlocks(asList(att["blocks"]))
			if pystr.Len(nestedText) > blocksBudget && blocksBudget <= tools.ElisionMarkerMaxLen() {
				nestedText = ""
			}
			nestedText = tools.Elide(nestedText, blocksBudget)
		}
		if nestedText != "" && !strings.Contains(body, nestedText) {
			blocksBudget -= pystr.Len(nestedText)
			if body != "" {
				body = pystr.Strip(body + "\n" + nestedText)
			} else {
				body = nestedText
			}
		}
		var section string
		switch {
		case header != "" && body != "":
			section = header + "\n   " + body
		case header != "":
			section = header
		case body != "":
			section = "📎 " + body
		default:
			continue
		}
		if strings.Contains(text, section) {
			continue
		}
		if attFooter != "" {
			section = section + "\n   _" + attFooter + "_"
		}
		attParts = append(attParts, section)
	}
	if len(attParts) > 0 {
		text = pystr.Strip(pystr.Strip(text) + "\n\n" + strings.Join(attParts, "\n\n"))
		slog.Debug(fmt.Sprintf("Slack: appended %d link unfurl(s) to message text", len(attParts)))
	}
	return text
}
