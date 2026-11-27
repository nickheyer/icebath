package transport

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What a reply carried that is replayed with it: its encrypted reasoning and compaction items stamped with the endpoint and model that
// made them, its message items with their ids, and the phase of its text
type carried struct {
	Reasoning []map[string]any `json:"reasoning,omitempty"`
	Messages  []map[string]any `json:"messages,omitempty"`
	Phase     string           `json:"phase,omitempty"`
}

// Longest id a replayed item may keep
const maxItemID = 64

// Text standing in for an image an assistant turn held, which the API takes no image in
const assistantImage = "[Assistant image omitted during replay]"

// Families of OpenAI models that keep the reasoning of every turn on api.openai.com when asked
var allTurnsFamilies = []string{"gpt-5.4", "gpt-5.5", "gpt-5.6", "gpt-6"}

// All turns family of the model, empty for any other
func allTurnsFamily(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, f := range allTurnsFamilies {
		if name == f || strings.HasPrefix(name, f+"-") || strings.HasPrefix(name, f+".") {
			return f
		}
	}
	return ""
}

// Model as the endpoint names it in a stamp: without the -900k a picker variant adds
func identity(model string) string {
	return strings.TrimSpace(strings.TrimSuffix(model, "-900k"))
}

// Endpoint that made an encrypted blob: xAI's, the kind's own name for it, the Codex backend, else any other by its address
func issuerOf(p *prepared) string {
	switch {
	case p.xai:
		return "xai_responses"
	case p.hosted.ResponsesIssuer != "":
		return p.hosted.ResponsesIssuer
	case p.codex:
		return "codex_backend"
	case strings.TrimSpace(p.route.BaseURL) == "":
		return "other"
	}
	return "other:" + strings.ToLower(strings.TrimRight(strings.TrimSpace(p.route.BaseURL), "/"))
}

// Whether a blob another model made can be read: one the same model made, or one of the same all turns family on api.openai.com
func readable(issuer, made, current string) bool {
	if current == "" || made == "" || made == current {
		return true
	}
	family := allTurnsFamily(current)
	return family != "" && family == allTurnsFamily(made) && strings.HasPrefix(issuer, "other:") &&
		provider.BaseHost(strings.TrimPrefix(issuer, "other:")) == "api.openai.com"
}

// Reserved tokens of OpenAI's Harmony format, which text sent to the Codex backend can't carry as they are
var harmonyToken = regexp.MustCompile(`<\|(start|end|channel|message|constrain|return|call)\|>`)

const fullwidthBar = "｜"

// Text with Harmony's reserved tokens written with fullwidth bars, matched across invisible format characters
func neutralize(text string) string {
	if !strings.Contains(text, "<") || !strings.Contains(text, "|") {
		return text
	}
	replace := func(name string) string { return "<" + fullwidthBar + name + fullwidthBar + ">" }
	format := strings.ContainsFunc(text, func(r rune) bool { return r > 0x7f && unicode.Is(unicode.Cf, r) })
	if !format {
		return harmonyToken.ReplaceAllStringFunc(text, func(m string) string { return replace(harmonyToken.FindStringSubmatch(m)[1]) })
	}
	runes := []rune(text)
	var positions []int
	var visible []rune
	for i, r := range runes {
		if !unicode.Is(unicode.Cf, r) {
			positions = append(positions, i)
			visible = append(visible, r)
		}
	}
	shown := string(visible)
	var out strings.Builder
	cursor := 0
	for _, loc := range harmonyToken.FindAllStringSubmatchIndex(shown, -1) {
		startRune, endRune := len([]rune(shown[:loc[0]])), len([]rune(shown[:loc[1]]))
		start, end := positions[startRune], positions[endRune-1]+1
		out.WriteString(string(runes[cursor:start]))
		out.WriteString(replace(shown[loc[2]:loc[3]]))
		cursor = end
	}
	out.WriteString(string(runes[cursor:]))
	return out.String()
}

// Reserved tokens in a key can't be neutralized without changing what the object means
var errHarmonyKey = errors.New("A field name in the request holds words the model reserves, and they can't be made safe without changing the field.")

// Every text of a JSON like value neutralized, an object key that holds a reserved token refused
func neutralizeAll(value any) (any, error) {
	switch v := value.(type) {
	case string:
		return neutralize(v), nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			n, err := neutralizeAll(item)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if neutralize(k) != k {
				return nil, errHarmonyKey
			}
			n, err := neutralizeAll(item)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	}
	return value, nil
}

// Image part of a picture of a message, a note for a format the API refuses and for an assistant's
func imagePart(media *sessionsv1.Media, role string) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": assistantImage}
	}
	mime := media.GetMimeType()
	if !strings.HasPrefix(mime, "image/") || mime == "image/svg+xml" {
		noun, _, _ := strings.Cut(mime, "/")
		return map[string]any{"type": "input_text", "text": "[" + noun + " omitted: " + mime + " is not a supported " + noun + " format]"}
	}
	return map[string]any{"type": "input_image", "image_url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(media.GetData())}
}

// Parts of a message with pictures: its text, then its pictures
func messageParts(m *sessionsv1.Message, role string, sanitize func(string) string) []map[string]any {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	var parts []map[string]any
	if text := m.GetText(); text != "" {
		parts = append(parts, map[string]any{"type": kind, "text": sanitize(text)})
	}
	for _, media := range m.GetMedia() {
		parts = append(parts, imagePart(media, role))
	}
	return parts
}

// Call id within the 64 character cap, a hash of a longer one
func clampID(id string) string {
	if len(id) <= maxItemID {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "call_" + hex.EncodeToString(sum[:])[:32]
}

// Call id of an fc_ item id, empty for any other
func fromItemID(item string) string {
	if strings.HasPrefix(item, "fc_") && len(item) > 3 {
		return "call_" + item[3:]
	}
	return ""
}

// Stored tool id as its call id and its response item id
func splitToolID(raw string) (string, string) {
	value := strings.TrimSpace(raw)
	if call, item, ok := strings.Cut(value, "|"); ok {
		return strings.TrimSpace(call), strings.TrimSpace(item)
	}
	if strings.HasPrefix(value, "fc_") {
		return "", value
	}
	return value, ""
}

var (
	toolNameShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	toolNameChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	underscores   = regexp.MustCompile(`_+`)
)

// Replayed tool name in the grammar the API takes, the fallback when nothing of it survives
func toolName(name, fallback string) string {
	if toolNameShape.MatchString(name) {
		return name
	}
	coerced := strings.Trim(underscores.ReplaceAllString(toolNameChars.ReplaceAllString(strings.TrimSpace(name), "_"), "_"), "_")
	coerced = strs.Head(coerced, 64)
	if coerced == "" {
		return fallback
	}
	return coerced
}

// Wire ids of the request's calls, a call id met again numbered so each replayed pair stays apart
type wireIDs struct {
	seen  map[string]int
	queue map[string][]string
}

func (w *wireIDs) call(id string) string {
	base := clampID(id)
	n := w.seen[base]
	w.seen[base] = n + 1
	wire := base
	if n > 0 {
		wire = clampID(fmt.Sprintf("%s_dup%d", base, n))
	}
	w.queue[base] = append(w.queue[base], wire)
	return wire
}

func (w *wireIDs) output(id string) string {
	base := clampID(id)
	q := w.queue[base]
	if len(q) == 0 {
		return base
	}
	w.queue[base] = q[1:]
	return q[0]
}

// How a replay is built for the endpoint
type replayRules struct {
	issuer, model string
	// Encrypted reasoning is replayed, item ids kept, compaction checkpoints kept, text sent as typed parts only
	encrypted, ids, compaction, typed bool
	sanitize                          func(string) string
	// Only the newest assistant turn keeps its encrypted reasoning, as Azure wants
	newestOnly bool
}

// Status of a replayed assistant message in the API's vocabulary
func status(value string) string {
	s := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_"), " ", "_")
	switch s {
	case "completed", "incomplete", "in_progress":
		return s
	}
	return "completed"
}

// Input items of the conversation: the person's messages, the replies with their encrypted reasoning, message items and calls, and the
// results of the calls
func input(messages []*sessionsv1.Message, rules replayRules) []any {
	var items []any
	seen := map[string]bool{}
	wire := &wireIDs{seen: map[string]int{}, queue: map[string][]string{}}
	newest := -1
	if rules.newestOnly {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].GetRole() == sessionsv1.Role_ROLE_ASSISTANT && len(transport.Carried[carried](messages[i], carrier).Reasoning) > 0 {
				newest = i
				break
			}
		}
	}
	text := func(role, s string) any {
		if rules.typed {
			kind := "input_text"
			if role == "assistant" {
				kind = "output_text"
			}
			return []map[string]any{{"type": kind, "text": s}}
		}
		return s
	}
	for i, m := range messages {
		switch m.GetRole() {
		case sessionsv1.Role_ROLE_TOOL:
			call, item := splitToolID(m.GetToolCallId())
			call = cmp.Or(call, fromItemID(item))
			if call == "" {
				call = strings.TrimSpace(m.GetToolCallId())
			}
			if call == "" {
				continue
			}
			var output any = rules.sanitize(m.GetText())
			if len(m.GetMedia()) > 0 {
				output = messageParts(m, "user", rules.sanitize)
			}
			items = append(items, map[string]any{"type": "function_call_output", "call_id": wire.output(call), "output": output})
		case sessionsv1.Role_ROLE_USER:
			var content any = text("user", rules.sanitize(m.GetText()))
			if len(m.GetMedia()) > 0 {
				content = messageParts(m, "user", rules.sanitize)
			}
			items = append(items, map[string]any{"type": "message", "role": "user", "content": content})
		case sessionsv1.Role_ROLE_ASSISTANT:
			kept := transport.Carried[carried](m, carrier)
			reasoning := kept.Reasoning
			trimmed := false
			if rules.newestOnly && i != newest {
				var checkpoints []map[string]any
				for _, item := range reasoning {
					if item["type"] == "compaction" {
						checkpoints = append(checkpoints, item)
					}
				}
				trimmed = len(checkpoints) != len(reasoning)
				reasoning = checkpoints
			}
			var replayed []any
			if rules.encrypted {
				replayed = replayReasoning(reasoning, seen, rules)
			}
			items = append(items, replayed...)
			messageItems := replayMessages(kept, trimmed, rules)
			items = append(items, messageItems...)
			var calls []any
			for _, call := range m.GetToolCalls() {
				if strings.TrimSpace(call.GetName()) == "" {
					continue
				}
				arguments := strings.TrimSpace(call.GetArguments())
				arguments = cmp.Or(arguments, "{}")
				id, _ := splitToolID(call.GetId())
				if id == "" {
					_, item := splitToolID(call.GetId())
					id = fromItemID(item)
				}
				id = cmp.Or(id, transport.CallID(call.GetName(), arguments, len(items)+len(calls)))
				calls = append(calls, map[string]any{"type": "function_call", "call_id": wire.call(id), "name": toolName(call.GetName(), "fn"),
					"arguments": rules.sanitize(arguments)})
			}
			if len(messageItems) == 0 {
				var fallback any
				switch {
				case len(m.GetMedia()) > 0:
					fallback = messageParts(m, "assistant", rules.sanitize)
				case strings.TrimSpace(m.GetText()) != "":
					fallback = text("assistant", rules.sanitize(m.GetText()))
				case len(replayed) > 0 && len(calls) == 0:
					fallback = text("assistant", " ")
				}
				if fallback != nil {
					item := map[string]any{"type": "message", "role": "assistant", "content": fallback}
					if phase := strings.ToLower(kept.Phase); phase == "commentary" || phase == "final_answer" {
						item["phase"] = phase
					}
					items = append(items, item)
				}
			}
			items = append(items, calls...)
		}
	}
	return items
}

// Encrypted reasoning and compaction items of a reply fit to replay: the same endpoint's, of a model that can read them, each once; their
// stamps and ids left off as the API wants them
func replayReasoning(reasoning []map[string]any, seen map[string]bool, rules replayRules) []any {
	var out []any
	for _, item := range reasoning {
		encrypted, _ := item["encrypted_content"].(string)
		if encrypted == "" {
			continue
		}
		id, _ := item["id"].(string)
		kind, _ := item["type"].(string)
		if id != "" && seen[id] || kind == "compaction" && !rules.compaction {
			continue
		}
		made, _ := item["_issuer_kind"].(string)
		madeModel, _ := item["_issuer_model"].(string)
		if rules.issuer != "" && made != "" && made != rules.issuer || !readable(rules.issuer, madeModel, rules.model) {
			slog.Warn(fmt.Sprintf("Dropping reasoning item minted by %s/%s while calling %s/%s — encrypted_content is sealed to its issuer and model. "+
				"This happens when a session switches model mid-conversation.", made, madeModel, rules.issuer, rules.model))
			continue
		}
		if id != "" {
			seen[id] = true
		}
		if kind == "compaction" {
			out = append(out, map[string]any{"type": "compaction", "encrypted_content": encrypted})
			continue
		}
		var summary any = []any{}
		if list, ok := item["summary"].([]any); ok {
			summary = list
		}
		if rules.typed {
			if clean, err := neutralizeAll(summary); err == nil {
				summary = clean
			}
		}
		out = append(out, map[string]any{"type": "reasoning", "encrypted_content": encrypted, "summary": summary})
	}
	return out
}

// Message items a reply came with, replayed as they were so the prompt cache holds: their ids kept where the endpoint takes them, dropped
// on a turn whose reasoning is replayed with it
func replayMessages(kept carried, trimmed bool, rules replayRules) []any {
	linked := trimmed
	for _, item := range kept.Reasoning {
		if s, _ := item["encrypted_content"].(string); s != "" {
			linked = true
		}
	}
	var out []any
	for _, raw := range kept.Messages {
		if raw["type"] != "message" || raw["role"] != "assistant" {
			continue
		}
		var content []map[string]any
		parts, _ := raw["content"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if kind, _ := part["type"].(string); kind == "output_text" || kind == "text" {
				text, _ := part["text"].(string)
				content = append(content, map[string]any{"type": "output_text", "text": rules.sanitize(text)})
			}
		}
		if len(content) == 0 {
			continue
		}
		item := map[string]any{"type": "message", "role": "assistant", "status": status(fmt.Sprint(raw["status"])), "content": content}
		id, _ := raw["id"].(string)
		id = strings.TrimSpace(id)
		keep := rules.ids && !linked && id != "" && len(id) <= maxItemID && (rules.issuer != "codex_backend" || strings.HasPrefix(id, "msg"))
		if keep {
			item["id"] = id
		}
		if phase, _ := raw["phase"].(string); strings.TrimSpace(phase) != "" {
			item["phase"] = strings.TrimSpace(phase)
		}
		out = append(out, item)
	}
	return out
}

// Whether the conversation ends on the results of the tool calls the last reply made
func afterTools(messages []*sessionsv1.Message) bool {
	trailing := map[string]bool{}
	ids := func(raw string) []string {
		call, item := splitToolID(raw)
		var out []string
		if call != "" {
			out = append(out, call)
		} else if strings.TrimSpace(raw) != "" {
			out = append(out, strings.TrimSpace(raw))
		}
		if canonical := fromItemID(item); canonical != "" {
			out = append(out, canonical)
		}
		return out
	}
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].GetRole() {
		case sessionsv1.Role_ROLE_SYSTEM:
			continue
		case sessionsv1.Role_ROLE_TOOL:
			found := ids(messages[i].GetToolCallId())
			if len(found) == 0 {
				return false
			}
			for _, id := range found {
				trailing[id] = true
			}
			continue
		case sessionsv1.Role_ROLE_ASSISTANT:
			for _, call := range messages[i].GetToolCalls() {
				for _, id := range ids(call.GetId()) {
					if trailing[id] {
						return true
					}
				}
			}
		}
		return false
	}
	return false
}

// Assistant text in content of a reply that should have been a structured tool call
var (
	toolCallLeak = regexp.MustCompile(`(?i)(?:^|[\s>|])to=functions\.[A-Za-z_][\pL\pN_.]*`)
	shellLeak    = regexp.MustCompile(`(?:^|\n)\s*\{\s*"cmd"\s*:\s*"(?:\\.|[^"\\])*"(?:\s*,\s*"[A-Za-z_][\w-]*"\s*:\s*(?:"(?:\\.|[^"\\])*"|true|false|null|-?\d+(?:\.\d+)?))*\s*\}\s*$`)
	shellLeadIn  = regexp.MustCompile(`(?i)^(?:(?:next|first|then|okay|ok|alright)\b[\s,—–-]*)?(?:sure,\s*)?(?:now\s+)?(?:let\s+me\s+|i(?:'|’)?ll\s+|` +
		`i\s+will\s+|i(?:'|’)?m\s+|i\s+am\s+)?(?:creat(?:e|ing)|writ(?:e|ing)|runn?(?:ing)?|execut(?:e|ing)|check(?:ing)?|verif(?:y|ying)|updat(?:e|ing)|` +
		`install(?:ing)?|edit(?:ing)?|mak(?:e|ing))\b`)
)

// Whether reply text carries a tool call the model didn't make as one
func leaked(text string) bool {
	if toolCallLeak.MatchString(text) {
		return true
	}
	loc := shellLeak.FindStringIndex(text)
	if loc == nil {
		return false
	}
	lines := strings.FieldsFunc(strings.TrimSpace(text[:loc[0]]), func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\v' || r == '\f' ||
			r == '\x1c' || r == '\x1d' || r == '\x1e' || r == '\u0085' || r == '\u2028' || r == '\u2029'
	})
	return len(lines) > 0 && shellLeadIn.MatchString(strings.TrimSpace(lines[len(lines)-1]))
}

// Statuses of a response or item that isn't finished
var unfinished = map[string]bool{"queued": true, "in_progress": true, "incomplete": true}

// Call items the provider's server makes itself, whose status says nothing of the reply
var serverCalls = map[string]bool{"web_search_call": true, "file_search_call": true, "code_interpreter_call": true, "image_generation_call": true,
	"computer_call": true, "local_shell_call": true, "mcp_call": true}

func lower(v any) string {
	s, _ := v.(string)
	return strings.ToLower(strings.TrimSpace(s))
}

// Text of a message item, refusals counted as text
func messageText(item map[string]any) string {
	var b strings.Builder
	parts, _ := item["content"].([]any)
	for _, p := range parts {
		part, _ := p.(map[string]any)
		switch part["type"] {
		case "refusal":
			s, _ := part["refusal"].(string)
			b.WriteString(s)
		case "output_text", "text":
			s, _ := part["text"].(string)
			b.WriteString(s)
		}
	}
	return strings.TrimSpace(b.String())
}

// Text of a reasoning item, its summary first
func reasoningText(item map[string]any) string {
	var chunks []string
	parts, _ := item["summary"].([]any)
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if s, _ := part["text"].(string); s != "" {
			chunks = append(chunks, s)
		}
	}
	if len(chunks) > 0 {
		return strings.TrimSpace(strings.Join(chunks, "\n"))
	}
	s, _ := item["text"].(string)
	return strings.TrimSpace(s)
}

// Message of an error object of a response, by its code and message, else its status
func responseError(object any, state string) string {
	field := func(name string) string {
		m, _ := object.(map[string]any)
		if s, ok := m[name].(string); ok {
			return strings.TrimSpace(s)
		}
		if v := m[name]; v != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	code, message := field("code"), field("message")
	switch {
	case code != "" && message != "":
		return code + ": " + message
	case message != "":
		return message
	case code != "":
		return code
	case object != nil:
		return fmt.Sprint(object)
	}
	return fmt.Sprintf("The provider ended the reply with the state %s.", state)
}

// Reply of a Responses object
type reply struct {
	text, reasoning string
	calls           []*sessionsv1.ToolCall
	kept            carried
	finish          string
}

// Reply of a Responses object: its text, its reasoning and commentary, its calls under the names the agent knows, the items replayed with
// it stamped with the endpoint and model that made them, and its finish: incomplete when it isn't done, holds only reasoning from an
// endpoint whose completed status isn't trusted, or leaked a tool call into its text
func normalize(p *prepared, response map[string]any, issuer, model string) (*reply, error) {
	state := lower(response["status"])
	reason := ""
	if details, ok := response["incomplete_details"].(map[string]any); ok {
		reason = lower(details["reason"])
	}
	filtered := state == "incomplete" && reason == "content_filter"
	output, _ := response["output"].([]any)
	if len(output) == 0 {
		outText, _ := response["output_text"].(string)
		outText = strings.TrimSpace(outText)
		switch {
		case outText != "":
			output = []any{map[string]any{"type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": outText}}}}
		case filtered:
			output = []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{}}}
		default:
			return nil, errors.New("The provider sent back a reply with nothing in it. Send your message again.")
		}
	}
	if state == "failed" || state == "cancelled" {
		return nil, errors.New(responseError(response["error"], state))
	}
	var texts, thoughts []string
	var messages, encrypted []map[string]any
	incomplete := unfinished[state]
	streaming := state == "queued" || state == "in_progress"
	commentary, final, reasoned := false, false, false
	out := &reply{}
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		kind, _ := item["type"].(string)
		itemState := lower(item["status"])
		if unfinished[itemState] && !serverCalls[kind] {
			incomplete, streaming = true, true
		}
		switch {
		case kind == "message":
			phase := lower(item["phase"])
			isCommentary := phase == "commentary" || phase == "analysis"
			commentary = commentary || isCommentary
			final = final || phase == "final_answer" || phase == "final"
			text := messageText(item)
			if text == "" {
				continue
			}
			if isCommentary {
				thoughts = append(thoughts, text)
			} else {
				texts = append(texts, text)
			}
			if phase != "" {
				out.kept.Phase = phase
			}
			entry := map[string]any{"type": "message", "role": "assistant", "status": status(itemState),
				"content": []any{map[string]any{"type": "output_text", "text": text}}}
			if id, _ := item["id"].(string); id != "" {
				entry["id"] = id
			}
			if phase != "" {
				entry["phase"] = phase
			}
			messages = append(messages, entry)
		case kind == "reasoning" || kind == "compaction":
			if kind == "reasoning" {
				reasoned = true
				if text := reasoningText(item); text != "" {
					thoughts = append(thoughts, text)
				}
			}
			if captured := capture(item, kind, issuer, model); captured != nil {
				encrypted = append(encrypted, captured)
				if kind == "compaction" {
					slog.Info(fmt.Sprintf("Native Responses compaction item captured (%d chars encrypted).", len(captured["encrypted_content"].(string))))
				}
			}
		case kind == "custom_tool_call" || kind == "function_call" && !unfinished[itemState]:
			out.calls = append(out.calls, p.toolCall(item, kind, len(out.calls)))
		}
	}
	text := strings.TrimSpace(strings.Join(texts, "\n"))
	if outText, _ := response["output_text"].(string); text == "" && (final || !commentary) {
		text = strings.TrimSpace(outText)
	}
	leak := text != "" && len(out.calls) == 0 && leaked(text)
	if leak {
		slog.Warn("Codex response contains leaked tool-call text in assistant content (no structured function_call items). Treating as incomplete " +
			"so the continuation path can re-elicit a proper tool call. Leaked snippet: " + fmt.Sprintf("%q", strs.Head(text, 300)))
		text = ""
	}
	if issuer == "xai_responses" && text == "" && len(out.calls) == 0 && len(thoughts) > 0 {
		joined := strings.Join(thoughts, "\n\n")
		if marker := strings.LastIndex(joined, "<response>"); marker != -1 {
			tail, _, _ := strings.Cut(joined[marker+len("<response>"):], "</response>")
			if salvaged := strings.TrimSpace(tail); salvaged != "" {
				slog.Warn(fmt.Sprintf("xAI response delivered its final answer inside the reasoning channel (<response> delimiter); promoting %d chars "+
					"to assistant content.", len(salvaged)))
				text = salvaged
				thoughts = nil
				if prefix := strings.TrimSpace(joined[:marker]); prefix != "" {
					thoughts = []string{prefix}
				}
			}
		}
	}
	out.text = text
	out.reasoning = strings.TrimSpace(strings.Join(thoughts, "\n\n"))
	out.kept.Reasoning = encrypted
	if !leak {
		out.kept.Messages = messages
	}
	only := (len(encrypted) > 0 || len(thoughts) > 0 || reasoned) && text == ""
	trusted := state == "completed" && issuer != "codex_backend" && issuer != "xai_responses" && !p.hosted.ResponsesDistrustCompletedStatus
	switch {
	case len(out.calls) > 0:
		out.finish = "tool_calls"
	case filtered:
		out.finish = "content_filter"
	case leak || streaming || (incomplete || commentary) && !final || only && !trusted:
		out.finish = "incomplete"
	default:
		out.finish = "stop"
	}
	return out, nil
}

// Reasoning or compaction item kept to replay, stamped with the endpoint and model that made it; nil for one without a blob, and for a
// transient reasoning item
func capture(item map[string]any, kind, issuer, model string) map[string]any {
	encrypted, _ := item["encrypted_content"].(string)
	if encrypted == "" {
		return nil
	}
	out := map[string]any{"type": kind, "encrypted_content": encrypted}
	if issuer != "" {
		out["_issuer_kind"] = issuer
	}
	if model != "" {
		out["_issuer_model"] = model
	}
	if kind != "reasoning" {
		return out
	}
	id, _ := item["id"].(string)
	if strings.HasPrefix(id, "rs_tmp_") {
		slog.Debug("Skipping transient Codex reasoning item during normalization: " + id)
		return nil
	}
	if id != "" {
		out["id"] = id
	}
	if summary, ok := item["summary"].([]any); ok {
		parts := []any{}
		for _, s := range summary {
			if part, _ := s.(map[string]any); part != nil {
				if text, ok := part["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "summary_text", "text": text})
				}
			}
		}
		out["summary"] = parts
	}
	return out
}

// Tool call of an output item, its id the call id it came with, its name the one the agent knows
func (p *prepared) toolCall(item map[string]any, kind string, index int) *sessionsv1.ToolCall {
	name, _ := item["name"].(string)
	key := "input"
	if kind == "function_call" {
		key = "arguments"
	}
	arguments, ok := item[key].(string)
	if !ok {
		if value, has := item[key]; has {
			raw, _ := json.Marshal(value)
			arguments = string(raw)
		} else {
			arguments = "{}"
		}
	}
	call, _ := item["call_id"].(string)
	raw, _ := item["id"].(string)
	embedded, _ := splitToolID(raw)
	id := strings.TrimSpace(call)
	id = cmp.Or(id, embedded)
	if id == "" {
		id = transport.CallID(name, arguments, index)
	}
	if original, ok := p.aliases[name]; ok {
		name = original
	}
	return &sessionsv1.ToolCall{Id: id, Name: name, Arguments: arguments}
}
