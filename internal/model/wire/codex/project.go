package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	MaxHistorySeedChars = 32000
	toolPreviewChars    = 400
	seedHeader          = "Prior conversation from this icebath session (the thread you are continuing was started fresh; " +
		"treat these turns as already having happened):"
)

var toolItemTypes = map[string]bool{"commandExecution": true, "fileChange": true, "mcpToolCall": true, "dynamicToolCall": true, "webSearch": true}

var reasoningDeltaMethods = map[string]bool{
	"item/reasoning/delta": true, "item/reasoning/summaryDelta": true, "item/reasoning/textDelta": true, "item/reasoning/summaryTextDelta": true,
}

type projection struct {
	messages      []wire.Message
	toolIteration bool
	finalText     string
	hasFinal      bool
}

type projector struct {
	pendingReasoning []string
}

func dumps(raw json.RawMessage, sorted bool) string {
	text, ok := wire.FormatJSON(raw, wire.TextLayout{Spaced: true, UTF8: true, Sorted: sorted})
	if !ok {
		return string(raw)
	}
	return text
}

func orText(v wire.Value, fallback string) string {
	if v.Truthy() {
		return v.Display()
	}
	return fallback
}

func deterministicCallID(itemType, itemID string) string {
	if itemID == "" {
		sum := sha256.Sum256([]byte(itemType))
		itemID = hex.EncodeToString(sum[:])[:16]
	}
	return "codex_" + itemType + "_" + itemID
}

func dictArgs(raw wire.Value) json.RawMessage {
	switch {
	case !raw.Truthy():
		return json.RawMessage("{}")
	case raw.IsObject():
		return raw.Raw()
	}
	return (&wire.Dict{}).Set("arguments", raw.Raw()).JSON()
}

func changeSummary(item wire.Value) json.RawMessage {
	var changes []json.RawMessage
	for _, change := range item.Get("changes").Elems() {
		if !change.IsObject() {
			continue
		}
		entry := (&wire.Dict{}).SetString("kind", orText(change.Path("kind", "type"), "update")).SetString("path", orText(change.Get("path"), ""))
		changes = append(changes, entry.JSON())
	}
	return (&wire.Dict{}).Set("changes", wire.Array(changes...)).JSON()
}

func (p *projector) assistant(text string, calls []wire.ToolCall) wire.Message {
	m := wire.Message{Role: wire.RoleAssistant, Text: text, ToolCalls: calls}
	if len(p.pendingReasoning) > 0 {
		m.Reasoning = strings.Join(p.pendingReasoning, "\n")
		p.pendingReasoning = nil
	}
	return m
}

func toolSpec(item wire.Value) (idType, name string, args json.RawMessage, content string) {
	switch item.Get("type").Text() {
	case "commandExecution":
		args = (&wire.Dict{}).SetString("command", orText(item.Get("command"), "")).SetString("cwd", orText(item.Get("cwd"), "")).JSON()
		output := orText(item.Get("aggregatedOutput"), "")
		if code := item.Get("exitCode"); code.Exists() && code.Kind() != wire.KindNull {
			output = dumps((&wire.Dict{}).Set("exit_code", code.Raw()).SetString("output", output).JSON(), false)
		}
		return "exec", "exec_command", args, output
	case "fileChange":
		content = "apply_patch status=" + orText(item.Get("status"), "unknown") + ", " + itoa(len(item.Get("changes").Elems())) + " change(s)"
		return "apply_patch", "apply_patch", changeSummary(item), content
	case "mcpToolCall":
		server, tool := orText(item.Get("server"), "mcp"), orText(item.Get("tool"), "unknown")
		if fault := item.Get("error"); fault.Truthy() {
			content = "[error] " + truncate(dumps(fault.Raw(), false), 1000)
		} else if result := item.Get("result"); result.Exists() && result.Kind() != wire.KindNull {
			content = truncate(dumps(result.Raw(), false), 4000)
		}
		return "mcp__" + server + "__" + tool, "mcp." + server + "." + tool, dictArgs(item.Get("arguments")), content
	case "dynamicToolCall":
		tool := orText(item.Get("tool"), "unknown")
		content = "success=" + pyDisplay(item.Get("success"))
		if items := item.Get("contentItems"); items.IsArray() && len(items.Elems()) > 0 {
			content = truncate(dumps(items.Raw(), false), 4000)
		}
		return "dyn_" + tool, tool, dictArgs(item.Get("arguments")), content
	}
	result := (&wire.Dict{}).SetString("provider", "codex")
	if status := item.Get("status"); status.Truthy() {
		result.Set("status", status.Raw())
	}
	args = (&wire.Dict{}).SetString("query", orText(item.Get("query"), "")).JSON()
	return "web_search", "web_search", args, dumps(result.JSON(), false)
}

func pyDisplay(v wire.Value) string {
	switch {
	case !v.Exists() || v.Kind() == wire.KindNull:
		return "None"
	case v.True():
		return "True"
	case v.False():
		return "False"
	}
	return v.Display()
}

func itoa(n int) string { return strconv.Itoa(n) }

func (p *projector) project(note wire.Value) projection {
	if note.Get("method").Text() != "item/completed" {
		return projection{}
	}
	item := note.Path("params", "item")
	itemType, itemID := item.Get("type").Text(), orText(item.Get("id"), "")
	switch {
	case itemType == "agentMessage":
		text := orText(item.Get("text"), "")
		return projection{messages: []wire.Message{p.assistant(text, nil)}, finalText: text, hasFinal: true}
	case itemType == "reasoning":
		for _, key := range []string{"summary", "content"} {
			for _, part := range item.Get(key).Elems() {
				p.pendingReasoning = append(p.pendingReasoning, part.Display())
			}
		}
		return projection{}
	case itemType == "userMessage":
		var parts []string
		for _, fragment := range item.Get("content").Elems() {
			if !fragment.IsObject() {
				continue
			}
			if fragment.Get("type").Text() == "text" {
				parts = append(parts, orText(fragment.Get("text"), ""))
			} else if fragment.Get("text").Exists() {
				parts = append(parts, pyDisplay(fragment.Get("text")))
			}
		}
		return projection{messages: []wire.Message{{Role: wire.RoleUser, Text: strings.Join(parts, "\n")}}}
	case toolItemTypes[itemType]:
		idType, name, args, content := toolSpec(item)
		callID := deterministicCallID(idType, itemID)
		call := wire.ToolCall{ID: callID, Name: name, Arguments: dumps(args, true)}
		return projection{messages: []wire.Message{p.assistant("", []wire.ToolCall{call}), {Role: wire.RoleTool, ToolCallID: callID, Text: content}}, toolIteration: true}
	}
	raw := json.RawMessage("{}")
	if item.Exists() {
		raw = item.Raw()
	}
	return projection{messages: []wire.Message{{Role: wire.RoleAssistant, Text: "[codex " + itemType + "] " + truncate(dumps(raw, false), 1500)}}}
}

func (s *Session) toolName(item wire.Value) string {
	switch itemType := item.Get("type").Text(); itemType {
	case "mcpToolCall":
		server, tool := orText(item.Get("server"), "mcp"), orText(item.Get("tool"), "unknown")
		if s.o.ToolsServerName != "" && server == s.o.ToolsServerName {
			return tool
		}
		return "mcp." + server + "." + tool
	case "dynamicToolCall":
		return orText(item.Get("tool"), "dynamic")
	case "commandExecution":
		return "exec_command"
	case "fileChange":
		return "apply_patch"
	case "webSearch":
		return "web_search"
	default:
		if itemType == "" {
			return "unknown"
		}
		return itemType
	}
}

func itemArgs(item wire.Value) json.RawMessage {
	switch item.Get("type").Text() {
	case "commandExecution":
		return (&wire.Dict{}).SetString("command", orText(item.Get("command"), "")).SetString("cwd", orText(item.Get("cwd"), "")).JSON()
	case "fileChange":
		return changeSummary(item)
	case "mcpToolCall", "dynamicToolCall":
		return dictArgs(item.Get("arguments"))
	case "webSearch":
		return (&wire.Dict{}).SetString("query", orText(item.Get("query"), "")).JSON()
	}
	return json.RawMessage("{}")
}

func itemPreview(item wire.Value) string {
	switch item.Get("type").Text() {
	case "commandExecution":
		return truncate(orText(item.Get("command"), ""), 120)
	case "webSearch":
		return truncate(orText(item.Get("query"), ""), 120)
	case "fileChange":
		var paths []string
		for _, change := range item.Get("changes").Elems() {
			if path := change.Get("path"); change.IsObject() && path.Truthy() {
				paths = append(paths, path.Display())
			}
		}
		if len(paths) == 0 {
			return ""
		}
		preview := strings.Join(paths[:min(3, len(paths))], ", ")
		if len(paths) > 3 {
			preview += ", +" + itoa(len(paths)-3) + " more"
		}
		return preview
	case "mcpToolCall", "dynamicToolCall":
		if args := item.Get("arguments"); args.IsObject() && len(args.Fields()) > 0 {
			return truncate(dumps(args.Raw(), false), 120)
		}
	}
	return ""
}

func completionPayload(item wire.Value) (string, bool) {
	switch item.Get("type").Text() {
	case "commandExecution":
		out, code := orText(item.Get("aggregatedOutput"), ""), item.Get("exitCode")
		n, isNumber := code.Int()
		if code.Exists() && code.Kind() != wire.KindNull && !(isNumber && n == 0) {
			return "[exit " + pyDisplay(code) + "]\n" + out, true
		}
		return out, false
	case "fileChange":
		status := orText(item.Get("status"), "unknown")
		text := "apply_patch status=" + status + ", " + itoa(len(item.Get("changes").Elems())) + " change(s)"
		return text, status != "completed" && status != "applied" && status != "success"
	case "mcpToolCall":
		if fault := item.Get("error"); fault.Truthy() {
			return "[error] " + truncate(dumps(fault.Raw(), false), 1000), true
		}
		if result := item.Get("result"); result.Exists() && result.Kind() != wire.KindNull {
			return truncate(dumps(result.Raw(), false), 4000), false
		}
		return "", false
	case "dynamicToolCall":
		success := item.Get("success")
		failed := success.Exists() && !success.Truthy()
		if items := item.Get("contentItems"); items.IsArray() && len(items.Elems()) > 0 {
			return truncate(dumps(items.Raw(), false), 4000), failed
		}
		if !success.Exists() {
			return "success=True", false
		}
		return "success=" + pyDisplay(success), failed
	}
	return "", false
}

func stableCallID(item wire.Value, name string) string {
	tool := orText(item.Get("tool"), "unknown")
	prefix := name
	switch item.Get("type").Text() {
	case "mcpToolCall":
		prefix = "mcp__" + orText(item.Get("server"), "mcp") + "__" + tool
	case "dynamicToolCall":
		prefix = "dyn_" + tool
	case "commandExecution":
		prefix = "exec"
	case "fileChange":
		prefix = "apply_patch"
	}
	return deterministicCallID(prefix, orText(item.Get("id"), ""))
}

func deltaText(params wire.Value) string {
	text := params.Get("delta")
	if !text.Truthy() {
		text = params.Get("text")
	}
	return text.Text()
}

func (s *Session) bridge(note wire.Value, sink wire.Sink) {
	if sink == nil || !note.IsObject() {
		return
	}
	emit := func(e wire.Event) {
		e.At = time.Now()
		sink(e)
	}
	method, params := note.Get("method").Text(), note.Get("params")
	switch {
	case method == "item/agentMessage/delta":
		if text := deltaText(params); text != "" {
			emit(wire.Event{Kind: wire.EventText, Text: text})
		}
	case reasoningDeltaMethods[method]:
		if text := deltaText(params); text != "" {
			emit(wire.Event{Kind: wire.EventReasoning, Text: text})
		}
	case method == "item/commandExecution/outputDelta" || method == "item/fileChange/outputDelta":
		if deltaText(params) != "" {
			emit(wire.Event{Kind: wire.EventKeepalive})
		}
	case method == "item/started" || method == "item/completed":
		item := params.Get("item")
		if !item.IsObject() {
			return
		}
		itemType, itemID := item.Get("type").Text(), orText(item.Get("id"), "")
		completed := method == "item/completed"
		switch {
		case toolItemTypes[itemType]:
			name := s.toolName(item)
			out := &wire.Item{ID: stableCallID(item, name), Type: itemType, Name: name, Args: itemArgs(item)}
			if !completed {
				if itemID != "" {
					s.started[itemID] = startedItem{args: out.Args, at: time.Now()}
				}
				out.Preview = itemPreview(item)
				emit(wire.Event{Kind: wire.EventItemStart, Item: out})
				return
			}
			prior, known := s.started[itemID]
			delete(s.started, itemID)
			if ms, ok := item.Get("durationMs").Float(); ok && ms >= 0 {
				out.Duration, out.Timed = time.Duration(ms*float64(time.Millisecond)), true
			} else if known {
				out.Duration, out.Timed = time.Since(prior.at), true
			}
			if known {
				out.Args = prior.args
			}
			out.Result, out.IsError = completionPayload(item)
			emit(wire.Event{Kind: wire.EventItemDone, Item: out})
		case completed && itemType == "agentMessage":
			if text := item.Get("text").Text(); strings.TrimSpace(text) != "" {
				emit(wire.Event{Kind: wire.EventMessage, Message: &wire.Message{Role: wire.RoleAssistant, Text: text}})
			} else {
				emit(wire.Event{Kind: wire.EventKeepalive})
			}
		case itemType == "reasoning":
			emit(wire.Event{Kind: wire.EventKeepalive})
		}
	}
}

func imageInput(part wire.Value) json.RawMessage {
	ref := wire.Value{}
	for _, key := range []string{"image_url", "url", "path", "image"} {
		if v := part.Get(key); v.Truthy() {
			ref = v
			break
		}
	}
	if ref.IsObject() {
		inner := ref.Get("url")
		if !inner.Truthy() {
			inner = ref.Get("path")
		}
		ref = inner
	}
	target := strings.TrimSpace(ref.Text())
	if target == "" {
		return nil
	}
	for _, scheme := range []string{"data:", "http://", "https://"} {
		if strings.HasPrefix(target, scheme) {
			return (&wire.Dict{}).SetString("type", "image").SetString("url", target).JSON()
		}
	}
	return (&wire.Dict{}).SetString("type", "localImage").SetString("path", strings.TrimPrefix(target, "file://")).JSON()
}

func buildTurnInput(m *wire.Message) ([]json.RawMessage, string) {
	textItem := func(text string) json.RawMessage {
		return (&wire.Dict{}).SetString("type", "text").SetString("text", text).JSON()
	}
	if m.Parts == nil {
		return []json.RawMessage{textItem(m.Text)}, m.Text
	}
	var texts []string
	var images []json.RawMessage
	for i := range m.Parts {
		part, err := wire.ParseValue(m.Parts[i].JSON())
		if err != nil {
			continue
		}
		if !part.IsObject() {
			if s, ok := part.Str(); ok && strings.TrimSpace(s) == "" {
				continue
			}
			if part.Kind() != wire.KindNull {
				texts = append(texts, pyDisplay(part))
			}
			continue
		}
		switch part.Get("type").Text() {
		case "text", "input_text":
			text := part.Get("text")
			if !text.Truthy() {
				text = part.Get("content")
			}
			if text.Truthy() {
				texts = append(texts, text.Display())
			}
		case "image", "image_url", "input_image":
			if mapped := imageInput(part); mapped != nil {
				images = append(images, mapped)
			}
		}
	}
	text := strings.TrimSpace(strings.Join(texts, "\n\n"))
	if text == "" && len(images) > 0 {
		text = "What do you see in this image?"
	}
	var items []json.RawMessage
	if text != "" || len(images) == 0 {
		items = append(items, textItem(text))
	}
	return append(items, images...), text
}

func seedText(content wire.Value) string {
	if s, ok := content.Str(); ok {
		return s
	}
	var parts []string
	for _, p := range content.Elems() {
		text := ""
		switch {
		case p.IsString():
			text = p.Text()
		case p.IsObject():
			text = p.Get("text").Display()
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func renderSeedRow(m wire.Value) string {
	text := strings.TrimSpace(seedText(m.Get("content")))
	switch m.Get("role").Text() {
	case "user":
		if text != "" {
			return "[USER]\n" + text
		}
	case "assistant":
		var lines, calls []string
		if text != "" {
			lines = append(lines, "[ASSISTANT]\n"+text)
		}
		named := false
		for _, call := range m.Get("tool_calls").Elems() {
			if !call.IsObject() {
				continue
			}
			named = true
			if name := call.Path("function", "name"); name.Truthy() {
				calls = append(calls, name.Display())
			}
		}
		if named {
			lines = append(lines, "[ASSISTANT called tools: "+strings.Join(calls, ", ")+"]")
		}
		return strings.Join(lines, "\n")
	case "tool":
		if runes := []rune(text); len(runes) > toolPreviewChars {
			text = string(runes[:toolPreviewChars]) + " …"
		}
		if text != "" {
			return "[TOOL RESULT]\n" + text
		}
	}
	return ""
}

func RenderHistorySeed(messages []wire.Message) string {
	if n := len(messages); n > 0 && messages[n-1].Role == wire.RoleUser {
		messages = messages[:n-1]
	}
	var rendered []string
	for i := range messages {
		m, err := wire.ParseValue(messages[i].JSON())
		if err != nil {
			continue
		}
		if row := renderSeedRow(m); row != "" {
			rendered = append(rendered, row)
		}
	}
	if len(rendered) == 0 {
		return ""
	}
	body := strings.Join(rendered, "\n\n")
	if runes := []rune(body); len(runes) > MaxHistorySeedChars {
		body = "[… earlier turns omitted …]\n\n" + string(runes[len(runes)-MaxHistorySeedChars:])
	}
	return seedHeader + "\n\n" + body
}
