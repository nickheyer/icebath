package tuigateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	screenshotPlaceholder  = "[screenshot]"
	autoContinueNotePrefix = "[System note: Your previous turn was interrupted mid-run"
	imageHintSeparator     = "\n\n[Image attached at: "
	defaultImageQuestion   = "What do you see in this image?"
	fsDataURLMaxBytes      = home.WebappAttachmentMaxBytes
	pyNonSpace             = `[^\s\x0b\x1c-\x1f\x85\p{Z}]`
)

var (
	discordTriggeringNoteRe = regexp.MustCompile("(^|\n)\\[Triggering message id: `[^`\n]*` — use as `message_id` for reply/react/pin via the discord tools\\.\\]\n*")
	imageHintRe             = regexp.MustCompile(`^\[Image attached at: ([^\n\]]+)\]$`)
	imageDataRe             = regexp.MustCompile(`^data:image/[^;,]+;base64,([A-Za-z0-9+/=]+)$`)
	imageRefRe              = regexp.MustCompile("^@image:(`[^`\n]+`|\"[^\"\n]+\"|'[^'\n]+'|" + pyNonSpace + "+)$")
	historyAssistantDetail  = []string{"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items"}
	historyRoles            = map[string]bool{"user": true, "assistant": true, "tool": true, "system": true}
	editCardTools           = map[string]bool{"write_file": true, "patch": true, "skill_manage": true}
)

// Text with its trailing screenshot marker lines removed and their count
func stripTrailingScreenshotPlaceholders(text string) (string, int) {
	lines := strings.Split(text, "\n")
	count := 0
	for len(lines) > 0 && lines[len(lines)-1] == screenshotPlaceholder {
		lines = lines[:len(lines)-1]
		count++
	}
	return strings.Join(lines, "\n"), count
}

// Whether the trailing markers describe none or all of the attached images
func screenshotCountMatches(placeholders, images int) bool {
	return placeholders == 0 || placeholders == images
}

// Image ref projection of the caption and paths of a hint form native vision row, false otherwise
func hintImageProjection(text string) (string, []string, bool) {
	caption, hints, _ := strings.Cut(text, imageHintSeparator)
	if caption == "" || caption == defaultImageQuestion {
		return "", nil, false
	}
	hints, placeholders := stripTrailingScreenshotPlaceholders(hints)
	var paths []string
	for _, line := range strings.Split("[Image attached at: "+hints, "\n") {
		m := imageHintRe.FindStringSubmatch(line)
		if m == nil {
			return "", nil, false
		}
		paths = append(paths, m[1])
	}
	if !screenshotCountMatches(placeholders, len(paths)) {
		return "", nil, false
	}
	refs := make([]string, len(paths))
	for i, p := range paths {
		refs[i] = "@image:" + state.FormatReferenceValue(p)
	}
	return caption + "\n\n" + strings.Join(refs, "\n"), paths, true
}

// Body and paths of a ref form native vision row whose trailing lines are image directives, false otherwise
func refImageProjection(text string) (string, []string, bool) {
	body, placeholders := stripTrailingScreenshotPlaceholders(text)
	lines := pystr.Splitlines(body, false)
	var refLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "@image:") {
			refLines = append(refLines, line)
		}
	}
	if len(refLines) == 0 || len(refLines) > len(lines) || !slices.Equal(lines[len(lines)-len(refLines):], refLines) {
		return "", nil, false
	}
	var paths []string
	for _, line := range refLines {
		m := imageRefRe.FindStringSubmatch(line)
		if m == nil {
			return "", nil, false
		}
		ref, p := m[1], m[1]
		if strings.ContainsRune("`\"'", rune(ref[0])) {
			p = ref[1 : len(ref)-1]
		}
		if state.FormatReferenceValue(p) != ref {
			return "", nil, false
		}
		paths = append(paths, p)
	}
	if !screenshotCountMatches(placeholders, len(paths)) {
		return "", nil, false
	}
	return body, paths, true
}

// Whether an attached image part carries exactly the bytes of the regular file at path
func imagePartMatchesFile(ctx context.Context, p string, raw any) bool {
	part, isDict := raw.(map[string]any)
	if !pypath.IsAbs(p) || !isDict || part["type"] != "image_url" {
		return false
	}
	m := imageDataRe.FindStringSubmatch(historyPartImageURL(part))
	if m == nil || len(m[1]) > ((fsDataURLMaxBytes+2)/3)*4 {
		return false
	}
	target, st, herr := FsRegularFile(ctx, p, "")
	if herr != nil || st.Size() > fsDataURLMaxBytes {
		return false
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(m[1])
	return err == nil && bytes.Equal(data, decoded)
}

// Verified native vision images of a user row projected as image refs, false when the row is anything else
func userImageDisplayText(ctx context.Context, content any) (string, bool) {
	parts, isList := content.([]any)
	if !isList || len(parts) < 2 {
		return "", false
	}
	first, isDict := parts[0].(map[string]any)
	if !isDict || first["type"] != "text" {
		return "", false
	}
	text, isStr := first["text"].(string)
	if !isStr {
		return "", false
	}
	var projected string
	var paths []string
	var found bool
	if strings.Contains(text, imageHintSeparator) {
		projected, paths, found = hintImageProjection(text)
	} else {
		projected, paths, found = refImageProjection(text)
	}
	if !found {
		return "", false
	}
	n := len(paths)
	if len(parts) < 1+n || (len(parts) != 1+n && len(parts) != 2+n) {
		return "", false
	}
	if len(parts) > 1+n {
		memory, isDict := parts[len(parts)-1].(map[string]any)
		if !isDict || memory["type"] != "text" {
			return "", false
		}
		memoryText, isStr := memory["text"].(string)
		if !isStr || pystr.Strip(state.SanitizeContext(memoryText)) != "" {
			return "", false
		}
	}
	for i, p := range paths {
		if !imagePartMatchesFile(ctx, p, parts[1+i]) {
			return "", false
		}
	}
	return projected, true
}

// Display kind of a synthetic row persisted untyped, empty when the row is an ordinary one
func legacyDisplayKind(role, text string) string {
	if kind, ok := state.UntypedFailedTurnDisplayKind(role, text); ok && kind != "" {
		return kind
	}
	if role == "user" && strings.HasPrefix(pystr.Lstrip(text), autoContinueNotePrefix) {
		return "auto_continue"
	}
	return ""
}

// Tool name and decoded arguments of one assistant call
type toolCallArgs struct {
	name string
	args any
}

// Dict member of a mapping with a default, raising like a Python get on a non dict
func dictGet(v any, key string, def any) any {
	d, isDict := v.(map[string]any)
	if !isDict {
		panic("AttributeError: '" + pyrepr.TypeName(v) + "' object has no attribute 'get'")
	}
	if value, present := d[key]; present {
		return value
	}
	return def
}

// Indexes the calls of an assistant row by id so their tool rows render with name and args
func rememberToolCallArgs(toolCalls any, index map[any]toolCallArgs) {
	calls, isList := toolCalls.([]any)
	if !isList {
		return
	}
	for _, tc := range calls {
		fn, tcID := dictGet(tc, "function", map[string]any{}), dictGet(tc, "id", "")
		name := dictGet(fn, "name", nil)
		if !pyrepr.Truthy(tcID) || !pyrepr.Truthy(name) {
			continue
		}
		var args any = map[string]any{}
		if raw, isStr := dictGet(fn, "arguments", "{}").(string); isStr {
			if decoded, err := pyjson.Loads(raw); err == nil {
				args = pyjson.Plain(decoded)
			}
		}
		index[hashableKey(tcID)] = toolCallArgs{pyrepr.Str(name), args}
	}
}

// Map key of a hashable Python value, raising TypeError for a list or dict
func hashableKey(v any) any {
	switch x := v.(type) {
	case []any, map[string]any:
		panic("TypeError: unhashable type: '" + pyrepr.TypeName(v) + "'")
	case *big.Int:
		return x.String()
	}
	return v
}

// Arguments of a tool row as the dict the label and preview builders read, raising for any other truthy value
func toolArgsDict(args any) map[string]any {
	if d, isDict := args.(map[string]any); isDict {
		return d
	}
	if !pyrepr.Truthy(args) {
		return map[string]any{}
	}
	panic("AttributeError: '" + pyrepr.TypeName(args) + "' object has no attribute 'get'")
}

// Unix seconds of a numeric timestamp above zero, false for anything else
func positiveTimestamp(ts any) (float64, bool) {
	var f float64
	switch x := ts.(type) {
	case bool:
		if x {
			f = 1
		}
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case float64:
		f = x
	case *big.Int:
		f, _ = new(big.Float).SetInt(x).Float64()
	default:
		return 0, false
	}
	return f, f > 0
}

// Display projection of stored history rows, image parts rendered as stand in labels when imageURLs is false
func HistoryToMessages(ctx context.Context, history []any, profileHome string, imageURLs bool) ([]any, error) {
	messages := []any{}
	index := map[any]toolCallArgs{}
	for _, raw := range history {
		if _, isDict := raw.(map[string]any); !isDict {
			continue
		}
		m := state.ProjectCompactionMessageForDisplay(raw)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		if !historyRoles[role] {
			continue
		}
		if role == "assistant" && pyrepr.Truthy(m["tool_calls"]) {
			rememberToolCallArgs(m["tool_calls"], index)
		}
		if m["display_kind"] == "hidden" {
			continue
		}
		contentText, projected := "", false
		if role == "user" {
			contentText, projected = userImageDisplayText(ctx, m["content"])
		}
		if !projected {
			contentText = CoerceMessageText(m["content"], imageURLs)
		}
		if IsDisplayHiddenMarker(role, contentText) {
			continue
		}
		if role == "user" {
			contentText = discordTriggeringNoteRe.ReplaceAllString(contentText, "${1}")
			if withheld, ok := state.WithheldPromptText(m); ok && withheld != "" {
				contentText = withheld
			}
		}
		if role == "user" && m["display_kind"] == state.SteerDisplayKind {
			if steer, ok := state.ExtractSteerTextFromMessage(m); ok && steer != "" {
				contentText = steer
			}
		}
		if role == "tool" {
			messages = append(messages, toolHistoryRow(m, index))
			continue
		}
		hasDetail := role == "assistant" && slices.ContainsFunc(historyAssistantDetail, func(k string) bool { return pyrepr.Truthy(m[k]) })
		if pystr.Strip(contentText) == "" && !hasDetail {
			continue
		}
		msg := map[string]any{"role": role, "text": contentText}
		if ts, ok := positiveTimestamp(m["timestamp"]); ok {
			msg["timestamp"] = ts
		}
		if rowID := m["_row_id"]; rowID != nil {
			msg["row_id"] = rowID
		}
		if role == "user" {
			if invocation := SkillScaffoldProjection(contentText); invocation != "" {
				msg["text"], msg["display_kind"] = invocation, "skill_invocation"
			}
		}
		if role == "assistant" {
			for _, key := range historyAssistantDetail {
				if m[key] != nil {
					msg[key] = m[key]
				}
			}
		}
		displayKind := m["display_kind"]
		if !pyrepr.Truthy(displayKind) {
			if legacy := legacyDisplayKind(role, contentText); legacy != "" {
				displayKind = legacy
			}
		}
		if pyrepr.Truthy(displayKind) {
			msg["display_kind"] = displayKind
		}
		if pyrepr.Truthy(m["display_metadata"]) {
			msg["display_metadata"] = m["display_metadata"]
		}
		messages = append(messages, msg)
	}
	return ProjectHistoryCommentary(ctx, messages, profileHome)
}

// Compact display row of a tool result labelled from the call that produced it
func toolHistoryRow(m map[string]any, index map[any]toolCallArgs) map[string]any {
	key := m["tool_call_id"]
	if !pyrepr.Truthy(key) {
		key = ""
	}
	call, found := index[hashableKey(key)]
	name := ""
	if found && call.name != "" {
		name = call.name
	} else if pyrepr.Truthy(m["tool_name"]) {
		name = pyrepr.Str(m["tool_name"])
	} else {
		name = "tool"
	}
	var args any = map[string]any{}
	if found && pyrepr.Truthy(call.args) {
		args = call.args
	}
	argsDict := toolArgsDict(args)
	labels := BridgedToolLabels(name, argsDict)
	row := map[string]any{"role": "tool", "name": name, "context": ToolCtx(name, argsDict)}
	if editCardTools[name] {
		row["content"] = m["content"]
	}
	for _, k := range []string{"tool_call_id", "timestamp", "display_metadata"} {
		if m[k] != nil {
			row[k] = m[k]
		}
	}
	if pyrepr.Truthy(args) {
		row["args"] = args
	}
	if len(labels) > 0 {
		payload := make([]any, len(labels))
		for i, label := range labels {
			entry := make(map[string]any, len(label))
			for k, v := range label {
				entry[k] = v
			}
			payload[i] = entry
		}
		row["labels"] = payload
	}
	return row
}
