package anthropic

import (
	"encoding/json"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	mcpPrefix              = "mcp__"
	claudeCodeSystemPrefix = "You are Claude Code, Anthropic's official CLI for Claude."
)

var oauthAliases = map[string]string{"session_search": "chat_history_lookup", "memory": "context_notes"}

var oauthReverseAliases = map[string]string{"chat_history_lookup": "session_search", "context_notes": "memory"}

var oauthProseAliases = []string{"session_search"}

var unionMetaKeys = []string{"title", "description", "default", "examples"}

func isNullBranch(v wire.Value) bool {
	return v.IsObject() && v.Get("type").Text() == "null"
}

func rewriteSchema(v wire.Value, fn func(*wire.Dict) json.RawMessage) json.RawMessage {
	switch {
	case v.IsArray():
		items := make([]json.RawMessage, 0, len(v.Elems()))
		for _, item := range v.Elems() {
			items = append(items, rewriteSchema(item, fn))
		}
		return wire.Array(items...)
	case v.IsObject():
		d := &wire.Dict{}
		for _, f := range v.Fields() {
			d.Set(f.Key, rewriteSchema(f.Value, fn))
		}
		return fn(d)
	}
	return v.Raw()
}

func collapseNullable(d *wire.Dict) json.RawMessage {
	for _, key := range []string{"anyOf", "oneOf"} {
		variants := d.Get(key)
		if !variants.IsArray() {
			continue
		}
		var nonNull []wire.Value
		for _, item := range variants.Elems() {
			if !isNullBranch(item) {
				nonNull = append(nonNull, item)
			}
		}
		if len(nonNull) != 1 || len(nonNull) == len(variants.Elems()) {
			continue
		}
		replacement := &wire.Dict{}
		if nonNull[0].IsObject() {
			replacement = wire.DictOf(nonNull[0])
		}
		for _, meta := range unionMetaKeys {
			if d.Has(meta) && !replacement.Has(meta) && !(meta == "default" && replacement.Has("$ref")) {
				replacement.Set(meta, d.Get(meta).Raw())
			}
		}
		return rewriteSchema(replacement.Value(), collapseNullable)
	}
	return d.JSON()
}

func normalizeInputSchema(schema json.RawMessage) json.RawMessage {
	empty := json.RawMessage(`{"type":"object","properties":{}}`)
	v, err := wire.ParseValue(schema)
	if err != nil || !v.Truthy() {
		return empty
	}
	normalized, err := wire.ParseValue(rewriteSchema(v, collapseNullable))
	if err != nil || !normalized.IsObject() {
		return empty
	}
	d := wire.DictOf(normalized)
	banned := false
	for _, key := range []string{"oneOf", "allOf", "anyOf"} {
		if d.Delete(key) {
			banned = true
		}
	}
	if banned && !d.Has("type") {
		d.SetString("type", "object")
	}
	if d.Get("type").Text() == "object" && !d.Get("properties").IsObject() {
		d.Set("properties", json.RawMessage("{}"))
	}
	return d.JSON()
}

func cacheMarkerJSON(c *wire.CacheMarker) json.RawMessage {
	d := (&wire.Dict{}).SetString("type", "ephemeral")
	if c.TTL != "" {
		d.SetString("ttl", c.TTL)
	}
	return d.JSON()
}

func convertTools(tools []wire.Tool) []*wire.Dict {
	var out []*wire.Dict
	seen := map[string]bool{}
	for i := range tools {
		t := &tools[i]
		if t.Name != "" && seen[t.Name] {
			slog.Warn("dropping a duplicate tool name from the Anthropic request", "tool", t.Name)
			continue
		}
		if t.Name != "" {
			seen[t.Name] = true
		}
		d := (&wire.Dict{}).SetString("name", t.Name).SetString("description", t.Description).
			Set("input_schema", normalizeInputSchema(t.Parameters))
		if t.Cache != nil {
			d.Set("cache_control", cacheMarkerJSON(t.Cache))
		}
		out = append(out, d)
	}
	return out
}

func mcpWireName(name string) string {
	if strings.HasPrefix(name, mcpPrefix) {
		return name
	}
	return mcpPrefix + strings.TrimPrefix(name, "mcp_")
}

func oauthNamer(tools []*wire.Dict) func(string) string {
	claimed := map[string]bool{}
	for _, tool := range tools {
		if name, ok := tool.Get("name").Str(); ok {
			if _, aliased := oauthAliases[name]; !aliased {
				claimed[mcpWireName(name)] = true
			}
		}
	}
	return func(name string) string {
		if aliased, ok := oauthAliases[name]; ok && !claimed[mcpPrefix+aliased] {
			name = aliased
		}
		return mcpWireName(name)
	}
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func replaceBounded(text, word, replacement string, before, after func(prev rune, hasPrev bool, rest string) bool) string {
	if !strings.Contains(text, word) {
		return text
	}
	var b strings.Builder
	i := 0
	for {
		at := strings.Index(text[i:], word)
		if at < 0 {
			b.WriteString(text[i:])
			return b.String()
		}
		at += i
		prev, hasPrev := rune(0), false
		if at > 0 {
			prev, _ = utf8.DecodeLastRuneInString(text[:at])
			hasPrev = true
		}
		rest := text[at+len(word):]
		if before(prev, hasPrev, rest) && after(prev, hasPrev, rest) {
			b.WriteString(text[i:at])
			b.WriteString(replacement)
		} else {
			b.WriteString(text[i : at+len(word)])
		}
		i = at + len(word)
	}
}

func firstRune(s string) (rune, bool) {
	for _, r := range s {
		return r, true
	}
	return 0, false
}

func applyProseAliases(text string) string {
	wordStart := func(prev rune, hasPrev bool, _ string) bool { return !hasPrev || !isWordRune(prev) }
	wordEnd := func(_ rune, _ bool, rest string) bool {
		r, ok := firstRune(rest)
		return !ok || !isWordRune(r)
	}
	for _, name := range oauthProseAliases {
		text = replaceBounded(text, name, oauthAliases[name], wordStart, wordEnd)
	}
	return text
}

func sanitizeProductNames(text string) string {
	text = strings.ReplaceAll(text, "heyer.app", "Anthropic")
	free := func(prev rune, hasPrev bool, _ string) bool {
		return !hasPrev || !(isWordRune(prev) || strings.ContainsRune("./:@'\"`-", prev))
	}
	standalone := func(_ rune, _ bool, rest string) bool {
		r, ok := firstRune(rest)
		if !ok {
			return true
		}
		if isWordRune(r) || r == '/' || r == '@' || r == '-' {
			return false
		}
		if r == '.' {
			next, more := firstRune(rest[1:])
			return !more || !isWordRune(next)
		}
		return true
	}
	for _, word := range []string{"Icebath", "icebath"} {
		text = replaceBounded(text, word, "Claude Code", free, standalone)
	}
	return text
}

func applyClaudeCodeIdentity(system *systemPrompt, tools []*wire.Dict, turns []turn, toWire func(string) string) *systemPrompt {
	out := &systemPrompt{list: true, blocks: []*wire.Dict{textBlock(claudeCodeSystemPrefix)}}
	switch {
	case system == nil:
	case system.list:
		out.blocks = append(out.blocks, system.blocks...)
	case system.raw == nil && system.text != "":
		out.blocks = append(out.blocks, textBlock(system.text))
	}
	for _, block := range out.blocks {
		if blockType(block) != "text" {
			continue
		}
		text := sanitizeProductNames(block.Get("text").Display())
		block.SetString("text", applyProseAliases(text))
	}
	for _, tool := range tools {
		if name := tool.Get("name"); name.Exists() {
			tool.SetString("name", toWire(name.Display()))
		}
		if description, ok := tool.Get("description").Str(); ok {
			tool.SetString("description", applyProseAliases(description))
		}
	}
	for i := range turns {
		if !turns[i].list {
			continue
		}
		for _, block := range turns[i].blocks {
			if blockType(block) == "tool_use" && block.Has("name") {
				block.SetString("name", toWire(block.Get("name").Display()))
			}
		}
	}
	return out
}
