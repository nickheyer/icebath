package bedrock

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	emptyPlaceholder = "(empty)"
	IssuerType       = "bedrock.native_assistant"
	profileARNMarker = ":application-inference-profile/"

	policySigned   = "signed"
	policyStrip    = "strip"
	policyUnsigned = "unsigned"
	policyIssuer   = "issuer"
)

var (
	replayStripPatterns  = []string{"deepseek.r1", "deepseek-r1", "moonshotai.kimi-k3"}
	replayIssuerPatterns = []string{"openai.gpt-6"}
)

type converter struct {
	model   string
	resolve func(arn string) string
}

type turn struct {
	role   string
	blocks []*wire.Dict
}

func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func strip(s string) string { return strings.TrimFunc(s, isSpace) }

func displayText(v wire.Value) string {
	switch {
	case !v.Exists() || v.Kind() == wire.KindNull:
		return ""
	case v.True():
		return "True"
	case v.False():
		return "False"
	}
	return v.Display()
}

func safeText(text string) string {
	if strip(text) == "" {
		return emptyPlaceholder
	}
	return text
}

func textBlock(text string) *wire.Dict { return (&wire.Dict{}).SetString("text", text) }

func placeholderBlock() *wire.Dict { return textBlock(emptyPlaceholder) }

func containsAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func (c *converter) resolved(model string) string {
	if strings.Contains(model, profileARNMarker) && c.resolve != nil {
		return c.resolve(model)
	}
	return model
}

func (c *converter) replayPolicy(model string) string {
	lowered := strings.ToLower(c.resolved(model))
	switch {
	case strings.Contains(lowered, "claude"):
		return policySigned
	case containsAny(lowered, replayIssuerPatterns):
		return policyIssuer
	case containsAny(lowered, replayStripPatterns):
		return policyStrip
	}
	return policyUnsigned
}

func (c *converter) mintedOutsideClaude(issuer string) bool {
	if issuer == "" {
		return false
	}
	if strings.Contains(issuer, profileARNMarker) && c.resolved(issuer) == issuer {
		return false
	}
	return c.replayPolicy(issuer) != policySigned
}

func issuerOf(m wire.Value) string {
	for _, d := range m.Get("reasoning_details").Elems() {
		if d.IsObject() && d.Get("type").Text() == IssuerType && d.Get("model").IsString() {
			return d.Get("model").Text()
		}
	}
	return ""
}

func lenientBase64(data string) ([]byte, bool) {
	clean := strings.Map(func(r rune) rune {
		if r == '+' || r == '/' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return -1
	}, data)
	pads, tail := strings.Count(data, "="), len(clean)%4
	raw, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil || (tail == 2 && pads < 2) || (tail == 3 && pads < 1) {
		return nil, false
	}
	return raw, true
}

func imageBlock(url string) *wire.Dict {
	header, data, _ := strings.Cut(url, ",")
	media := ""
	if strings.HasPrefix(header, "data:") {
		media, _, _ = strings.Cut(header[5:], ";")
	}
	if media == "" {
		media = "image/jpeg"
	}
	format := "jpeg"
	if i := strings.LastIndex(media, "/"); i >= 0 {
		format = media[i+1:]
	}
	raw, ok := lenientBase64(data)
	if !ok {
		raw = []byte(data)
	}
	source := (&wire.Dict{}).SetString("bytes", base64.StdEncoding.EncodeToString(raw))
	image := (&wire.Dict{}).SetString("format", format).Set("source", source.JSON())
	return (&wire.Dict{}).Set("image", image.JSON())
}

func convertContent(content wire.Value) []*wire.Dict {
	if !content.IsArray() {
		return []*wire.Dict{textBlock(safeText(displayText(content)))}
	}
	var blocks []*wire.Dict
	for _, part := range content.Elems() {
		switch {
		case part.IsString():
			blocks = append(blocks, textBlock(safeText(part.Text())))
		case part.IsObject() && part.Get("type").Text() == "text":
			blocks = append(blocks, textBlock(safeText(displayText(part.Get("text")))))
		case part.IsObject() && part.Get("type").Text() == "image_url":
			url := ""
			if image := part.Get("image_url"); image.IsObject() {
				url = image.Get("url").Display()
			}
			if strings.HasPrefix(url, "data:") {
				blocks = append(blocks, imageBlock(url))
			} else {
				blocks = append(blocks, textBlock("[Image: "+url+"]"))
			}
		}
	}
	if len(blocks) == 0 {
		return []*wire.Dict{placeholderBlock()}
	}
	return blocks
}

func systemBlocks(content wire.Value) []*wire.Dict {
	var parts []wire.Value
	switch {
	case content.IsString():
		parts = []wire.Value{content}
	case content.IsArray():
		parts = content.Elems()
	}
	var out []*wire.Dict
	for _, part := range parts {
		text := part
		if part.IsObject() && part.Get("type").Text() == "text" {
			text = part.Get("text")
			if !text.Exists() {
				continue
			}
		}
		if s, ok := text.Str(); ok && strip(s) != "" {
			out = append(out, textBlock(s))
		}
	}
	return out
}

func toolUseBlock(id, name wire.Value, input json.RawMessage) *wire.Dict {
	use := &wire.Dict{}
	if id.Exists() {
		use.Set("toolUseId", id.Raw())
	} else {
		use.SetString("toolUseId", "")
	}
	if name.Exists() {
		use.Set("name", name.Raw())
	} else {
		use.SetString("name", "")
	}
	return (&wire.Dict{}).Set("toolUse", use.Set("input", input).JSON())
}

func parseArgs(v wire.Value) json.RawMessage {
	if !v.Exists() {
		return json.RawMessage("{}")
	}
	s, ok := v.Str()
	if !ok {
		return v.Raw()
	}
	parsed, err := wire.ParseValue([]byte(s))
	if err != nil {
		return json.RawMessage("{}")
	}
	return parsed.Raw()
}

func strictRedacted(v wire.Value) (string, bool) {
	encoded, ok := v.Str()
	if !ok || encoded == "" {
		return "", false
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(raw), true
}

func redactedBlock(v wire.Value) *wire.Dict {
	encoded, ok := strictRedacted(v)
	if !ok {
		return nil
	}
	inner := (&wire.Dict{}).SetString("redactedContent", encoded)
	return (&wire.Dict{}).Set("reasoningContent", inner.JSON())
}

func reasoningTextBlock(text, signature wire.Value) *wire.Dict {
	rt := (&wire.Dict{}).SetString("text", text.Text())
	if sig, ok := signature.Str(); ok && sig != "" {
		rt.SetString("signature", sig)
	}
	inner := (&wire.Dict{}).Set("reasoningText", rt.JSON())
	return (&wire.Dict{}).Set("reasoningContent", inner.JSON())
}

func replayOrdered(ordered wire.Value) []*wire.Dict {
	var out []*wire.Dict
	for _, block := range ordered.Elems() {
		if !block.IsObject() {
			continue
		}
		switch {
		case block.Get("text").IsString():
			out = append(out, textBlock(block.Get("text").Text()))
		case block.Get("reasoningContent").Exists():
			reasoning := block.Get("reasoningContent")
			if !reasoning.IsObject() {
				continue
			}
			if reasoning.Get("text").IsString() {
				out = append(out, reasoningTextBlock(reasoning.Get("text"), reasoning.Get("signature")))
			}
			if b := redactedBlock(reasoning.Get("redactedContentBase64")); b != nil {
				out = append(out, b)
			}
		case block.Get("toolUse").IsObject():
			use := block.Get("toolUse")
			input := json.RawMessage("{}")
			if v := use.Get("input"); v.Exists() {
				input = v.Raw()
			}
			out = append(out, toolUseBlock(use.Get("toolUseId"), use.Get("name"), input))
		}
	}
	return out
}

func anthropicOrdered(m wire.Value) []*wire.Dict {
	args := map[string]json.RawMessage{}
	for _, tc := range m.Get("tool_calls").Elems() {
		if tc.IsObject() {
			args[tc.Get("id").Display()] = parseArgs(tc.Path("function", "arguments"))
		}
	}
	var out []*wire.Dict
	for _, b := range m.Get("anthropic_content_blocks").Elems() {
		if !b.IsObject() {
			continue
		}
		switch b.Get("type").Text() {
		case "thinking":
			out = append(out, reasoningTextBlock(b.Get("thinking"), b.Get("signature")))
		case "redacted_thinking":
			if block := redactedBlock(b.Get("data")); block != nil {
				out = append(out, block)
			}
		case "text":
			if text, ok := b.Get("text").Str(); ok && strip(text) != "" {
				out = append(out, textBlock(text))
			}
		case "tool_use":
			input, ok := args[b.Get("id").Display()]
			if !ok {
				input = json.RawMessage("{}")
				if v := b.Get("input"); v.Exists() {
					input = v.Raw()
				}
			}
			out = append(out, toolUseBlock(b.Get("id"), b.Get("name"), input))
		}
	}
	return out
}

func reasoningDetailsBlocks(m wire.Value, claude, own bool) []*wire.Dict {
	var blocks []*wire.Dict
	for _, d := range m.Get("reasoning_details").Elems() {
		if !d.IsObject() {
			continue
		}
		switch d.Get("type").Text() {
		case "thinking":
			thinking, ok := d.Get("thinking").Str()
			if !ok || strip(thinking) == "" {
				continue
			}
			if (claude || own) && d.Get("signature").Truthy() {
				blocks = append(blocks, reasoningTextBlock(d.Get("thinking"), d.Get("signature")))
			} else if !claude {
				blocks = append(blocks, reasoningTextBlock(d.Get("thinking"), wire.Value{}))
			}
		case "redacted_thinking":
			data := d.Get("data")
			if !data.Truthy() {
				data = d.Get("redactedContentBase64")
			}
			if block := redactedBlock(data); block != nil {
				blocks = append(blocks, block)
			}
		}
	}
	if text, ok := m.Get("reasoning_content").Str(); len(blocks) == 0 && !claude && ok && strip(text) != "" {
		blocks = append(blocks, reasoningTextBlock(m.Get("reasoning_content"), wire.Value{}))
	}
	return blocks
}

func assistantBlocks(m wire.Value, claude, own bool) []*wire.Dict {
	if ordered := m.Get("bedrock_content_blocks"); ordered.IsArray() {
		if blocks := replayOrdered(ordered); len(blocks) > 0 {
			return blocks
		}
	}
	if claude {
		if blocks := anthropicOrdered(m); len(blocks) > 0 {
			return blocks
		}
	}
	blocks := reasoningDetailsBlocks(m, claude, own)
	content := m.Get("content")
	if text, ok := content.Str(); ok && strip(text) != "" {
		blocks = append(blocks, textBlock(text))
	} else if content.IsArray() {
		blocks = append(blocks, convertContent(content)...)
	}
	for _, tc := range m.Get("tool_calls").Elems() {
		fn := tc.Get("function")
		blocks = append(blocks, toolUseBlock(tc.Get("id"), fn.Get("name"), parseArgs(fn.Get("arguments"))))
	}
	return blocks
}

type reasoningFilter uint8

const (
	keepVerbatim reasoningFilter = iota
	keepUnsigned
	keepNone
)

func priorFilter(policy string) reasoningFilter {
	switch policy {
	case policyStrip, policyIssuer:
		return keepNone
	case policyUnsigned:
		return keepUnsigned
	}
	return keepVerbatim
}

func (c *converter) filterFor(policy, issuer string, own, inLoop bool) reasoningFilter {
	prior := priorFilter(policy)
	loop := keepVerbatim
	if policy == policyIssuer {
		loop = keepNone
	}
	if own {
		if inLoop || policy == policyUnsigned || policy == policyIssuer {
			return keepVerbatim
		}
		return prior
	}
	if issuer != "" {
		if policy == policySigned && c.mintedOutsideClaude(issuer) {
			return keepNone
		}
		return prior
	}
	if inLoop {
		return loop
	}
	return prior
}

func applyFilter(blocks []*wire.Dict, keep reasoningFilter) []*wire.Dict {
	if keep == keepVerbatim {
		return blocks
	}
	var out []*wire.Dict
	for _, b := range blocks {
		reasoning := b.Get("reasoningContent")
		if !b.Has("reasoningContent") {
			out = append(out, b)
			continue
		}
		if keep == keepNone {
			continue
		}
		if !reasoning.IsObject() {
			out = append(out, b)
			continue
		}
		if text, ok := reasoning.Path("reasoningText", "text").Str(); ok && strip(text) != "" {
			rt := (&wire.Dict{}).SetString("text", text)
			inner := (&wire.Dict{}).Set("reasoningText", rt.JSON())
			out = append(out, (&wire.Dict{}).Set("reasoningContent", inner.JSON()))
		}
	}
	return out
}

func (c *converter) convertMessages(messages []wire.Message) ([]*wire.Dict, []turn) {
	policy := c.replayPolicy(c.model)
	parsed := make([]wire.Value, len(messages))
	loopStart := -1
	for i := range messages {
		v, err := wire.ParseValue(messages[i].JSON())
		if err != nil {
			continue
		}
		parsed[i] = v
		if v.Get("role").Text() == "user" {
			loopStart = i
		}
	}
	var system []*wire.Dict
	var turns []turn
	appendTurn := func(role string, blocks []*wire.Dict) {
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			return
		}
		turns = append(turns, turn{role: role, blocks: blocks})
	}
	for idx, m := range parsed {
		content := m.Get("content")
		switch m.Get("role").Text() {
		case "system":
			system = append(system, systemBlocks(content)...)
		case "tool":
			text, ok := content.Str()
			if !ok {
				text = "null"
				if content.Exists() {
					text, _ = wire.FormatJSON(content.Raw(), wire.TextLayout{Spaced: true})
				}
			}
			id := json.RawMessage(`""`)
			if v := m.Get("tool_call_id"); v.Exists() {
				id = v.Raw()
			}
			result := (&wire.Dict{}).Set("toolUseId", id).Set("content", wire.Array(textBlock(safeText(text)).JSON()))
			appendTurn("user", []*wire.Dict{(&wire.Dict{}).Set("toolResult", result.JSON())})
		case "assistant":
			issuer := issuerOf(m)
			own := issuer != "" && issuer == c.model
			keep := c.filterFor(policy, issuer, own, idx >= loopStart)
			blocks := applyFilter(assistantBlocks(m, policy == policySigned, own), keep)
			if len(blocks) == 0 {
				blocks = []*wire.Dict{placeholderBlock()}
			}
			appendTurn("assistant", blocks)
		case "user":
			appendTurn("user", convertContent(content))
		}
	}
	if len(turns) > 0 && turns[0].role != "user" {
		turns = append([]turn{{role: "user", blocks: []*wire.Dict{placeholderBlock()}}}, turns...)
	}
	if len(turns) > 0 && turns[len(turns)-1].role != "user" {
		turns = append(turns, turn{role: "user", blocks: []*wire.Dict{placeholderBlock()}})
	}
	return system, turns
}

func blocksJSON(blocks []*wire.Dict) json.RawMessage {
	items := make([]json.RawMessage, len(blocks))
	for i, b := range blocks {
		items[i] = b.JSON()
	}
	return wire.Array(items...)
}
