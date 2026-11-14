package anthropic

import (
	"encoding/json"
	"log/slog"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	emptyPlaceholder   = "(empty)"
	emptyUserText      = "(empty message)"
	noOutputText       = "(no output)"
	screenshotRemoved  = "[screenshot removed to save context]"
	converseIssuerType = "bedrock.native_assistant"
	profileARNMarker   = ":application-inference-profile/"
)

type turn struct {
	role    string
	text    string
	raw     json.RawMessage
	blocks  []*wire.Dict
	list    bool
	sigDead bool
}

type systemPrompt struct {
	text   string
	raw    json.RawMessage
	blocks []*wire.Dict
	list   bool
}

type converter struct {
	baseURL   string
	model     string
	portal    bool
	rasterize func(dataURL string) (string, bool)
	resolve   func(arn string) string
}

func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func strip(s string) string { return strings.TrimFunc(s, isSpace) }

func textBlock(text string) *wire.Dict {
	return (&wire.Dict{}).SetString("type", "text").SetString("text", text)
}

func blockType(d *wire.Dict) string { return d.Get("type").Text() }

func isThinkingType(t string) bool { return t == "thinking" || t == "redacted_thinking" }

func isCacheable(t string) bool { return t == "text" || t == "tool_use" }

func hasBlockType(blocks []*wire.Dict, match func(string) bool) bool {
	for _, b := range blocks {
		if match(blockType(b)) {
			return true
		}
	}
	return false
}

func blankTextValue(v wire.Value) bool {
	s, ok := v.Str()
	return !ok || strip(s) == ""
}

func isBlankText(d *wire.Dict) bool {
	return blockType(d) == "text" && blankTextValue(d.Get("text"))
}

func cacheControl(v wire.Value) json.RawMessage {
	if cc := v.Get("cache_control"); cc.IsObject() {
		return cc.Raw()
	}
	return nil
}

func displayText(v wire.Value) string {
	switch {
	case v.True():
		return "True"
	case v.False():
		return "False"
	case v.Kind() == wire.KindNull:
		return "None"
	}
	return v.Display()
}

func textWithCitations(text wire.Value, citations wire.Value) *wire.Dict {
	d := (&wire.Dict{}).SetString("type", "text")
	if text.Exists() {
		d.Set("text", text.Raw())
	} else {
		d.SetString("text", "")
	}
	if citations.IsArray() && len(citations.Elems()) > 0 {
		d.Set("citations", citations.Raw())
	}
	return d
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

func sanitizeToolID(id string) string {
	out := []byte(id)
	for i, c := range out {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			out[i] = '_'
		}
	}
	if len(out) == 0 {
		return "tool_0"
	}
	return string(out)
}

func toolUseBlock(id string, name wire.Value, input json.RawMessage) *wire.Dict {
	d := (&wire.Dict{}).SetString("type", "tool_use").SetString("id", sanitizeToolID(id))
	if name.Exists() {
		d.Set("name", name.Raw())
	} else {
		d.SetString("name", "")
	}
	return d.Set("input", input)
}

func unsupportedInlineImage(url string) string {
	header, _, _ := strings.Cut(url, ",")
	header = strings.ToLower(header)
	if !strings.HasPrefix(header, "data:image/") {
		return ""
	}
	subtype, _, _ := strings.Cut(header[len("data:image/"):], ";")
	subtype = strip(subtype)
	if subtype == "" {
		subtype = "unknown"
	}
	media := "image/" + subtype
	if subtype == "jpg" {
		media = "image/jpeg"
	}
	switch media {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return ""
	}
	return media
}

func (c *converter) imageBlock(url string) *wire.Dict {
	url = strip(url)
	if !strings.HasPrefix(url, "data:") {
		source := (&wire.Dict{}).SetString("type", "url").SetString("url", url)
		return (&wire.Dict{}).SetString("type", "image").Set("source", source.JSON())
	}
	unsupported := unsupportedInlineImage(url)
	if unsupported == "image/svg+xml" && c.rasterize != nil {
		if png, ok := c.rasterize(url); ok {
			url, unsupported = png, ""
		}
	}
	if unsupported != "" {
		return textBlock("[image omitted: " + unsupported + " is not a supported image format]")
	}
	header, data, _ := strings.Cut(url, ",")
	mime, _, _ := strings.Cut(header[len("data:"):], ";")
	mime = strip(mime)
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg"
	}
	if strings.ToLower(mime) == "image/jpg" {
		mime = "image/jpeg"
	}
	source := (&wire.Dict{}).SetString("type", "base64").SetString("media_type", mime).SetString("data", data)
	return (&wire.Dict{}).SetString("type", "image").Set("source", source.JSON())
}

func (c *converter) convertPart(part wire.Value) *wire.Dict {
	if !part.Exists() || part.Kind() == wire.KindNull {
		return nil
	}
	if !part.IsObject() {
		return textBlock(displayText(part))
	}
	var block *wire.Dict
	switch ptype := part.Get("type").Text(); ptype {
	case "input_text", "text":
		citations := wire.Value{}
		if ptype == "text" {
			citations = part.Get("citations")
		}
		block = textWithCitations(part.Get("text"), citations)
	case "image_url", "input_image":
		image := part.Get("image_url")
		url := ""
		switch {
		case image.IsObject():
			url = image.Get("url").Display()
		case image.Truthy():
			url = image.Display()
		}
		block = c.imageBlock(url)
	default:
		block = wire.DictOf(part)
	}
	if cc := cacheControl(part); cc != nil && !block.Has("cache_control") {
		block.Set("cache_control", cc)
	}
	return block
}

func (c *converter) convertContent(content wire.Value) []*wire.Dict {
	var out []*wire.Dict
	for _, part := range content.Elems() {
		if b := c.convertPart(part); b != nil {
			out = append(out, b)
		}
	}
	return out
}

func (c *converter) toolResultBlocks(parts wire.Value) []*wire.Dict {
	var out []*wire.Dict
	for _, part := range parts.Elems() {
		block := c.convertPart(part)
		if block == nil || block.Len() == 0 {
			continue
		}
		switch blockType(block) {
		case "text":
			if text, ok := block.Get("text").Str(); ok && text != "" {
				out = append(out, textBlock(text))
			}
		case "image":
			if src := block.Get("source"); src.IsObject() && len(src.Fields()) > 0 {
				out = append(out, (&wire.Dict{}).SetString("type", "image").Set("source", src.Raw()))
			}
		}
	}
	return out
}

func sanitizeReplayBlock(b wire.Value) *wire.Dict {
	if !b.IsObject() {
		return nil
	}
	switch b.Get("type").Text() {
	case "text":
		if blankTextValue(b.Get("text")) {
			return nil
		}
		d := textWithCitations(b.Get("text"), b.Get("citations"))
		if cc := cacheControl(b); cc != nil {
			d.Set("cache_control", cc)
		}
		return d
	case "thinking":
		d := (&wire.Dict{}).SetString("type", "thinking")
		if thinking := b.Get("thinking"); thinking.Exists() {
			d.Set("thinking", thinking.Raw())
		} else {
			d.SetString("thinking", "")
		}
		if sig := b.Get("signature"); sig.Truthy() {
			d.Set("signature", sig.Raw())
		}
		return d
	case "redacted_thinking":
		if data := b.Get("data"); data.Truthy() {
			return (&wire.Dict{}).SetString("type", "redacted_thinking").Set("data", data.Raw())
		}
		return nil
	case "tool_use":
		input := json.RawMessage("{}")
		if v := b.Get("input"); v.Exists() {
			input = v.Raw()
		}
		d := toolUseBlock(b.Get("id").Text(), b.Get("name"), input)
		if cc := cacheControl(b); cc != nil {
			d.Set("cache_control", cc)
		}
		return d
	case "image":
		if src := b.Get("source"); src.IsObject() {
			return (&wire.Dict{}).SetString("type", "image").Set("source", src.Raw())
		}
	}
	return nil
}

func applyCache(blocks []*wire.Dict, cc json.RawMessage) {
	if cc == nil {
		return
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if isCacheable(blockType(blocks[i])) {
			if !blocks[i].Has("cache_control") {
				blocks[i].Set("cache_control", cc)
			}
			return
		}
	}
}

func splitBlank(blocks []*wire.Dict) (kept []*wire.Dict, relocated json.RawMessage, dropped []int) {
	for i, b := range blocks {
		if !isBlankText(b) {
			kept = append(kept, b)
			continue
		}
		dropped = append(dropped, i)
		if cc := cacheControl(b.Value()); cc != nil {
			relocated = cc
		}
	}
	return kept, relocated, dropped
}

func fixBlank(blocks []*wire.Dict, placeholder string, index int, role, location string) []*wire.Dict {
	kept, relocated, dropped := splitBlank(blocks)
	for _, at := range dropped {
		slog.Warn("pre-call sanitizer dropped a blank text content block", "message_index", index, "role", role, "location", location, "block_index", at)
	}
	if len(kept) == 0 {
		kept = []*wire.Dict{textBlock(placeholder)}
	}
	applyCache(kept, relocated)
	return kept
}

func (c *converter) mintedOutsideClaude(m wire.Value) bool {
	issuer := ""
	for _, d := range m.Get("reasoning_details").Elems() {
		if d.IsObject() && d.Get("type").Text() == converseIssuerType && d.Get("model").IsString() {
			issuer = d.Get("model").Text()
			break
		}
	}
	if issuer == "" {
		return false
	}
	if strings.Contains(issuer, profileARNMarker) {
		resolved := issuer
		if c.resolve != nil {
			resolved = c.resolve(issuer)
		}
		if resolved == issuer {
			return false
		}
		issuer = resolved
	}
	return !strings.Contains(strings.ToLower(issuer), "claude")
}

func (c *converter) preservedThinking(m wire.Value) []*wire.Dict {
	details := m.Get("reasoning_details")
	if !details.IsArray() || c.mintedOutsideClaude(m) {
		return nil
	}
	var out []*wire.Dict
	for _, d := range details.Elems() {
		if d.IsObject() && isThinkingType(strings.ToLower(strip(d.Get("type").Display()))) {
			out = append(out, wire.DictOf(d))
		}
	}
	return out
}

func (c *converter) replayOrdered(m wire.Value, ordered []wire.Value) []*wire.Dict {
	redacted := map[string]json.RawMessage{}
	for _, tc := range m.Get("tool_calls").Elems() {
		if tc.IsObject() {
			redacted[sanitizeToolID(tc.Get("id").Text())] = parseArgs(tc.Path("function", "arguments"))
		}
	}
	var replayed []*wire.Dict
	var relocated json.RawMessage
	droppedBlank := false
	for _, b := range ordered {
		clean := sanitizeReplayBlock(b)
		if clean == nil {
			droppedBlank = droppedBlank || b.Get("type").Text() == "text"
			if cc := cacheControl(b); cc != nil {
				relocated = cc
			}
			continue
		}
		if blockType(clean) == "tool_use" {
			if input, ok := redacted[clean.Get("id").Text()]; ok && string(input) != "null" {
				clean.Set("input", input)
			}
		}
		replayed = append(replayed, clean)
	}
	if !hasBlockType(replayed, isCacheable) && (droppedBlank || relocated != nil) {
		replayed = append(replayed, textBlock(emptyPlaceholder))
	}
	if len(replayed) == 0 {
		return nil
	}
	applyCache(replayed, relocated)
	applyCache(replayed, cacheControl(m))
	if content := m.Get("content"); content.IsArray() {
		for _, part := range content.Elems() {
			if cc := cacheControl(part); cc != nil {
				applyCache(replayed, cc)
				break
			}
		}
	}
	return replayed
}

func (c *converter) replayCarrier(m wire.Value) (bool, []*wire.Dict) {
	if ordered := m.Get("anthropic_content_blocks"); ordered.IsArray() && len(ordered.Elems()) > 0 {
		if replayed := c.replayOrdered(m, ordered.Elems()); len(replayed) > 0 {
			return true, replayed
		}
	}
	return false, c.preservedThinking(m)
}

func (c *converter) assistant(m wire.Value) turn {
	ordered, blocks := c.replayCarrier(m)
	if ordered {
		return turn{role: "assistant", list: true, blocks: blocks}
	}
	var relocated json.RawMessage
	content := m.Get("content")
	if content.IsArray() {
		kept, moved, _ := splitBlank(c.convertContent(content))
		relocated = moved
		blocks = append(blocks, kept...)
	} else if content.Truthy() && strip(displayText(content)) != "" {
		blocks = append(blocks, textBlock(displayText(content)))
	}
	for _, tc := range m.Get("tool_calls").Elems() {
		if !tc.IsObject() || !tc.Truthy() {
			continue
		}
		fn := tc.Get("function")
		blocks = append(blocks, toolUseBlock(tc.Get("id").Text(), fn.Get("name"), parseArgs(fn.Get("arguments"))))
	}
	if rc := m.Get("reasoning_content"); rc.IsString() && !hasBlockType(blocks, isThinkingType) {
		thinking := (&wire.Dict{}).SetString("type", "thinking").Set("thinking", rc.Raw())
		blocks = append([]*wire.Dict{thinking}, blocks...)
	}
	if len(blocks) == 0 {
		blocks = []*wire.Dict{textBlock(emptyPlaceholder)}
	}
	applyCache(blocks, relocated)
	applyCache(blocks, cacheControl(m))
	return turn{role: "assistant", list: true, blocks: blocks}
}

func (c *converter) toolResultContent(m wire.Value) json.RawMessage {
	content := m.Get("content")
	var multimodal []*wire.Dict
	decided := false
	if content.IsObject() && content.Get("_multimodal").Truthy() {
		multimodal, decided = c.toolResultBlocks(content.Get("content")), true
		if len(multimodal) == 0 && content.Get("text_summary").Truthy() {
			multimodal = []*wire.Dict{textBlock(displayText(content.Get("text_summary")))}
		}
	} else if content.IsArray() {
		if converted := c.toolResultBlocks(content); hasBlockType(converted, func(t string) bool { return t == "image" }) {
			multimodal, decided = converted, true
		}
	}
	if !decided {
		if stashed := m.Get("_anthropic_content_blocks"); stashed.IsArray() && len(stashed.Elems()) > 0 {
			if text, ok := content.Str(); ok && strip(text) != "" {
				multimodal = append(multimodal, textBlock(text))
			}
			for _, b := range stashed.Elems() {
				multimodal = append(multimodal, wire.DictOf(b))
			}
		}
	}
	if len(multimodal) > 0 {
		return blocksJSON(multimodal)
	}
	text := ""
	if s, ok := content.Str(); ok {
		text = s
	} else if content.Truthy() {
		text, _ = wire.FormatJSON(content.Raw(), wire.TextLayout{Spaced: true})
	}
	if text == "" {
		text = noOutputText
	}
	return wire.Quote(text)
}

func blocksJSON(blocks []*wire.Dict) json.RawMessage {
	items := make([]json.RawMessage, len(blocks))
	for i, b := range blocks {
		items[i] = b.JSON()
	}
	return wire.Array(items...)
}

func (c *converter) appendToolResult(result []turn, m wire.Value) []turn {
	block := (&wire.Dict{}).SetString("type", "tool_result").SetString("tool_use_id", sanitizeToolID(m.Get("tool_call_id").Text())).
		Set("content", c.toolResultContent(m))
	if cc := cacheControl(m); cc != nil {
		block.Set("cache_control", cc)
	}
	if n := len(result); n > 0 {
		last := &result[n-1]
		if last.role == "user" && last.list && len(last.blocks) > 0 && blockType(last.blocks[0]) == "tool_result" {
			last.blocks = append(last.blocks, block)
			return result
		}
	}
	return append(result, turn{role: "user", list: true, blocks: []*wire.Dict{block}})
}

func (c *converter) user(content wire.Value) turn {
	if content.IsArray() {
		return turn{role: "user", list: true, blocks: fixBlank(c.convertContent(content), emptyUserText, -1, "user", "convert_user_message")}
	}
	if s, ok := content.Str(); ok {
		if strip(s) == "" {
			s = emptyUserText
		}
		return turn{role: "user", text: s}
	}
	if !content.Truthy() {
		return turn{role: "user", text: emptyUserText}
	}
	return turn{role: "user", raw: content.Raw()}
}

func blockIDs(blocks []*wire.Dict, btype, key string) map[string]bool {
	ids := map[string]bool{}
	for _, b := range blocks {
		if blockType(b) == btype {
			ids[b.Get(key).Display()] = true
		}
	}
	return ids
}

func stripOrphans(result []turn) {
	for i := range result {
		m := &result[i]
		if m.role != "assistant" || !m.list {
			continue
		}
		uses := blockIDs(m.blocks, "tool_use", "id")
		if len(uses) == 0 {
			continue
		}
		adjacent := map[string]bool{}
		if i+1 < len(result) && result[i+1].role == "user" && result[i+1].list {
			adjacent = blockIDs(result[i+1].blocks, "tool_result", "tool_use_id")
		}
		var kept []*wire.Dict
		for _, b := range m.blocks {
			if blockType(b) == "tool_use" && !adjacent[b.Get("id").Display()] {
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) == len(m.blocks) {
			continue
		}
		if hasBlockType(m.blocks, isThinkingType) {
			m.sigDead = true
		}
		if len(kept) == 0 {
			kept = []*wire.Dict{textBlock("(tool call removed)")}
		}
		m.blocks = kept
	}
	surviving := map[string]bool{}
	for i := range result {
		if result[i].role == "assistant" && result[i].list {
			for id := range blockIDs(result[i].blocks, "tool_use", "id") {
				surviving[id] = true
			}
		}
	}
	for i := range result {
		m := &result[i]
		if m.role != "user" || !m.list {
			continue
		}
		var kept []*wire.Dict
		for _, b := range m.blocks {
			if blockType(b) != "tool_result" || surviving[b.Get("tool_use_id").Display()] {
				kept = append(kept, b)
			}
		}
		if len(kept) != len(m.blocks) {
			if len(kept) == 0 {
				kept = []*wire.Dict{textBlock("(tool result removed)")}
			}
			m.blocks = kept
		}
	}
}

func (t *turn) asBlocks() []*wire.Dict {
	switch {
	case t.list:
		return t.blocks
	case t.raw != nil:
		text, _ := wire.FormatJSON(t.raw, wire.TextLayout{Spaced: true})
		return []*wire.Dict{textBlock(text)}
	}
	return []*wire.Dict{textBlock(t.text)}
}

func mergeRoles(result []turn) []turn {
	var fixed []turn
	for _, m := range result {
		if len(fixed) == 0 || fixed[len(fixed)-1].role != m.role {
			fixed = append(fixed, m)
			continue
		}
		prev := &fixed[len(fixed)-1]
		if m.role != "user" {
			if m.sigDead {
				prev.sigDead = true
			}
			if m.list {
				var kept []*wire.Dict
				for _, b := range m.blocks {
					if !isThinkingType(blockType(b)) {
						kept = append(kept, b)
					}
				}
				m.blocks = kept
			}
		}
		merged := append(append([]*wire.Dict{}, prev.asBlocks()...), m.asBlocks()...)
		prev.blocks, prev.list, prev.text, prev.raw = merged, true, "", nil
	}
	return fixed
}

func keepValidThinking(blocks []*wire.Dict, dead bool) []*wire.Dict {
	var out []*wire.Dict
	for _, b := range blocks {
		t := blockType(b)
		if !isThinkingType(t) {
			out = append(out, b)
			continue
		}
		redactedBlock := t == "redacted_thinking"
		signed := b.Get("signature")
		if redactedBlock {
			signed = b.Get("data")
		}
		thinking := b.Get("thinking")
		if signed.Truthy() && !dead {
			out = append(out, b)
		} else if (dead || !redactedBlock) && thinking.Truthy() {
			out = append(out, textBlock(thinking.Display()))
		}
	}
	return out
}

func toolLoopStart(result []turn) int {
	for i := len(result) - 1; i >= 0; i-- {
		m := &result[i]
		if m.role != "user" {
			continue
		}
		if !(m.list && hasBlockType(m.blocks, func(t string) bool { return t == "tool_result" })) {
			return i + 1
		}
	}
	return 0
}

func (c *converter) manageThinking(result []turn) {
	route := ThinkingRoute(c.baseURL, c.model, c.portal)
	lastAssistant := len(result)
	for i := len(result) - 1; i >= 0; i-- {
		if result[i].role == "assistant" {
			lastAssistant = i
			break
		}
	}
	loopStart := min(toolLoopStart(result), lastAssistant)
	preservePrior := RouteReplaysPriorThinking(route, c.model)
	for i := range result {
		m := &result[i]
		if m.role != "assistant" || !m.list {
			continue
		}
		switch {
		case route == RouteKimi:
		case route == RouteDeepSeek:
			var kept []*wire.Dict
			for _, b := range m.blocks {
				if !isThinkingType(blockType(b)) || !(b.Get("signature").Truthy() || b.Get("data").Truthy()) {
					kept = append(kept, b)
				}
			}
			if len(kept) == 0 {
				kept = []*wire.Dict{textBlock(emptyPlaceholder)}
			}
			m.blocks = kept
		case route == RouteThirdParty || (i < loopStart && !preservePrior):
			var kept []*wire.Dict
			for _, b := range m.blocks {
				if !isThinkingType(blockType(b)) {
					kept = append(kept, b)
				}
			}
			if len(kept) == 0 {
				kept = []*wire.Dict{textBlock("(thinking elided)")}
			}
			m.blocks = kept
		default:
			kept := keepValidThinking(m.blocks, m.sigDead)
			if len(kept) == 0 {
				kept = []*wire.Dict{textBlock(emptyPlaceholder)}
			}
			m.blocks = kept
		}
		for _, b := range m.blocks {
			if isThinkingType(blockType(b)) {
				b.Delete("cache_control")
			}
		}
		m.sigDead = false
	}
}

func imageData(b wire.Value) (string, bool) {
	return b.Path("source", "data").Str()
}

type carrier struct {
	block  *wire.Dict
	images int
	bytes  int
}

func evictScreenshots(result []turn) {
	var datas []string
	reservedBlocks, reservedBytes := 0, 0
	for i := range result {
		for _, b := range result[i].blocks {
			if blockType(b) == "image" {
				data, _ := imageData(b.Value())
				datas = append(datas, data)
				reservedBlocks++
				reservedBytes += len(data)
			}
		}
	}
	var carriers []carrier
	for i := len(result) - 1; i >= 0; i-- {
		blocks := result[i].blocks
		for j := len(blocks) - 1; j >= 0; j-- {
			inner := blocks[j].Get("content")
			if blockType(blocks[j]) != "tool_result" || !inner.IsArray() {
				continue
			}
			found := carrier{block: blocks[j]}
			for _, b := range inner.Elems() {
				if b.IsObject() && b.Get("type").Text() == "image" {
					data, _ := imageData(b)
					datas = append(datas, data)
					found.images++
					found.bytes += len(data)
				}
			}
			if found.images > 0 {
				carriers = append(carriers, found)
			}
		}
	}
	load := wire.ImageLoad{ReservedBlocks: reservedBlocks, ReservedBytes: reservedBytes, ManyImageSafe: wire.ImagesWithinManyImageLimit(datas)}
	for _, found := range carriers {
		load.CarrierBlocks = append(load.CarrierBlocks, found.images)
		load.CarrierBytes = append(load.CarrierBytes, found.bytes)
	}
	retire := min(max(wire.ImageRetireCount(load), 0), len(carriers))
	for _, found := range carriers[len(carriers)-retire:] {
		var inner []json.RawMessage
		for _, b := range found.block.Get("content").Elems() {
			if b.IsObject() && b.Get("type").Text() == "image" {
				inner = append(inner, textBlock(screenshotRemoved).JSON())
			} else {
				inner = append(inner, b.Raw())
			}
		}
		found.block.Set("content", wire.Array(inner...))
	}
}

func scrubBlank(result []turn) {
	for i := range result {
		m := &result[i]
		if !m.list || len(m.blocks) == 0 {
			continue
		}
		placeholder := emptyUserText
		if m.role == "assistant" {
			placeholder = emptyPlaceholder
		}
		m.blocks = fixBlank(m.blocks, placeholder, i, m.role, "content")
		for _, b := range m.blocks {
			inner := b.Get("content")
			if blockType(b) != "tool_result" || !inner.IsArray() || len(inner.Elems()) == 0 {
				continue
			}
			var blocks []*wire.Dict
			for _, item := range inner.Elems() {
				if item.IsObject() {
					blocks = append(blocks, wire.DictOf(item))
				} else {
					blocks = append(blocks, textBlock(displayText(item)))
				}
			}
			b.Set("content", blocksJSON(fixBlank(blocks, noOutputText, i, m.role, "tool_result")))
		}
	}
}

func convertSystem(content wire.Value) *systemPrompt {
	if !content.Exists() {
		return &systemPrompt{}
	}
	if s, ok := content.Str(); ok {
		return &systemPrompt{text: s}
	}
	if !content.IsArray() {
		return &systemPrompt{raw: content.Raw()}
	}
	marked := false
	for _, p := range content.Elems() {
		if p.IsObject() && p.Get("cache_control").Truthy() {
			marked = true
		}
	}
	if !marked {
		var texts []string
		for _, p := range content.Elems() {
			if p.Get("type").Text() == "text" {
				texts = append(texts, p.Get("text").Display())
			}
		}
		return &systemPrompt{text: strings.Join(texts, "\n")}
	}
	out := &systemPrompt{list: true}
	for _, p := range content.Elems() {
		if !p.IsObject() {
			continue
		}
		d := wire.DictOf(p)
		if text, ok := p.Get("text").Str(); ok && p.Get("type").Text() == "text" && strip(text) == "" {
			d.SetString("text", emptyPlaceholder)
		}
		out.blocks = append(out.blocks, d)
	}
	return out
}

func (s *systemPrompt) truthy() bool {
	switch {
	case s == nil:
		return false
	case s.list:
		return len(s.blocks) > 0
	case s.raw != nil:
		v, err := wire.ParseValue(s.raw)
		return err == nil && v.Truthy()
	}
	return s.text != ""
}

func (s *systemPrompt) json() json.RawMessage {
	switch {
	case s.list:
		return blocksJSON(s.blocks)
	case s.raw != nil:
		return s.raw
	}
	return wire.Quote(s.text)
}

func (c *converter) convertMessages(messages []wire.Message) (*systemPrompt, []turn) {
	var system *systemPrompt
	var result []turn
	for i := range messages {
		m, err := wire.ParseValue(messages[i].JSON())
		if err != nil {
			continue
		}
		role := "user"
		if r := m.Get("role"); r.Exists() {
			role = r.Text()
		}
		switch role {
		case "system":
			system = convertSystem(m.Get("content"))
		case "assistant":
			result = append(result, c.assistant(m))
		case "tool":
			result = c.appendToolResult(result, m)
		default:
			result = append(result, c.user(m.Get("content")))
		}
	}
	stripOrphans(result)
	result = mergeRoles(result)
	if len(result) > 0 && result[0].role != "user" {
		result = append([]turn{{role: "user", list: true, blocks: []*wire.Dict{textBlock(emptyPlaceholder)}}}, result...)
	}
	c.manageThinking(result)
	evictScreenshots(result)
	scrubBlank(result)
	return system, result
}

func turnsJSON(result []turn) json.RawMessage {
	items := make([]json.RawMessage, len(result))
	for i := range result {
		t := &result[i]
		d := (&wire.Dict{}).SetString("role", t.role)
		switch {
		case t.list:
			d.Set("content", blocksJSON(t.blocks))
		case t.raw != nil:
			d.Set("content", t.raw)
		default:
			d.SetString("content", t.text)
		}
		items[i] = d.JSON()
	}
	return wire.Array(items...)
}
