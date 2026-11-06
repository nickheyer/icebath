package provider

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var codexLog = slog.Default()

// ---- endpoint quirks and issuer kinds ----

// Responses wire quirks of one endpoint, from the profile claiming it or the generic wire
type responsesEndpointQuirks struct {
	replayItemIDs    bool
	promptCacheKey   bool
	issuerKind       string
	nativeCompaction bool
	profileReasoning bool
}

var genericResponsesEndpoint = responsesEndpointQuirks{replayItemIDs: true, promptCacheKey: true, nativeCompaction: true}

// Quirks of the endpoint at the base URL, the generic wire when no profile claims the host
func responsesEndpointQuirksFor(baseURL string) responsesEndpointQuirks {
	p, ok := profileForHost(strings.TrimSpace(baseURL))
	if !ok {
		return genericResponsesEndpoint
	}
	return responsesEndpointQuirks{
		replayItemIDs: p.ResponsesReplayItemIDs(), promptCacheKey: p.ResponsesPromptCacheKey(), issuerKind: p.ResponsesIssuerKind(),
		nativeCompaction: p.ResponsesNativeCompaction(), profileReasoning: p.ResponsesProfileReasoning(),
	}
}

// Reports whether the endpoint binds replayed message item ids to a connection
func dropsReplayItemIDs(baseURL string) bool {
	return !responsesEndpointQuirksFor(baseURL).replayItemIDs
}

// Stable identifier of the endpoint minting encrypted reasoning blobs
func classifyResponsesIssuer(isXAI, isCodexBackend bool, baseURL string, endpoint *responsesEndpointQuirks) string {
	endpointKind := ""
	if endpoint != nil {
		endpointKind = endpoint.issuerKind
	}
	switch {
	case isXAI:
		return "xai_responses"
	case endpointKind != "":
		return endpointKind
	case isCodexBackend:
		return "codex_backend"
	}
	if strings.TrimSpace(baseURL) == "" {
		return "other"
	}
	return "other:" + NormalizeRouteBaseURL(strings.TrimSpace(baseURL))
}

// Canonical form of a persisted other url issuer stamp
func canonicalIssuerKind(kind string) string {
	if strings.HasPrefix(kind, "other:") {
		return classifyResponsesIssuer(false, false, kind[len("other:"):], nil)
	}
	return kind
}

var crossIssuerWarnOnce sync.Once

// Canonical wire model stamped on encrypted reasoning
func wireModelIdentity(model string) string {
	return strings.TrimSpace(StripCodexContextVariantSuffix(model))
}

var openAIAllTurnsFamilies = []string{"gpt-5.4", "gpt-5.5", "gpt-5.6", "gpt-6"}

// OpenAIReasoningFamily is the all_turns family a model belongs to, empty for every other model
func OpenAIReasoningFamily(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, f := range openAIAllTurnsFamilies {
		if name == f || strings.HasPrefix(name, f+"-") || strings.HasPrefix(name, f+".") {
			return f
		}
	}
	return ""
}

func isOpenAIAPIIssuer(issuerKind string) bool {
	return strings.HasPrefix(issuerKind, "other:") && strings.ToLower(baseURLHostname(issuerKind[len("other:"):])) == "api.openai.com"
}

// Exact model everywhere, same all_turns family on api.openai.com
func modelCanReadBlob(issuerKind, itemModel, currentModel string) bool {
	if currentModel == "" || itemModel == "" || itemModel == currentModel {
		return true
	}
	family := OpenAIReasoningFamily(currentModel)
	return family != "" && family == OpenAIReasoningFamily(itemModel) && isOpenAIAPIIssuer(issuerKind)
}

// Decoded replay items of a message sidecar, nil when absent or malformed
func decodeCodexItems(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func encodeCodexItems(items []map[string]any) json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	raw, _ := json.Marshal(items)
	return raw
}

// StripUnverifiedReasoningItems drops replayed items without an exact issuer and model stamp, returning the count
func StripUnverifiedReasoningItems(msgs []*state.Message, issuerKind, issuerModel string) int {
	unverifiedButSent := func(item map[string]any) bool {
		if stringOf(item["type"]) == "compaction" {
			return false
		}
		kind, hasKind := item["_issuer_kind"]
		model := stringOf(item["_issuer_model"])
		canon := ""
		if hasKind && kind != nil {
			canon = canonicalIssuerKind(stringOf(kind))
		}
		if canon == issuerKind && model == issuerModel {
			return false
		}
		return (!hasKind || kind == nil || canon == issuerKind) && modelCanReadBlob(issuerKind, model, issuerModel)
	}
	removed := 0
	for _, m := range msgs {
		if m == nil || m.Role != "assistant" {
			continue
		}
		items := decodeCodexItems(m.CodexReasoningItems)
		if len(items) == 0 {
			continue
		}
		kept := items[:0:0]
		for _, it := range items {
			if !unverifiedButSent(it) {
				kept = append(kept, it)
			}
		}
		removed += len(items) - len(kept)
		m.CodexReasoningItems = encodeCodexItems(kept)
	}
	return removed
}

// ---- leaked tool call text ----

var toolCallLeakRe = regexp.MustCompile(`(?i)(?:^|[\s>|])to=functions\.[A-Za-z_][\pL\pN_.]*`)
var shellJSONLeakRe = regexp.MustCompile(`(?:^|\n)\s*\{\s*"cmd"\s*:\s*"(?:\\.|[^"\\])*"(?:\s*,\s*"[A-Za-z_][\w-]*"\s*:\s*(?:"(?:\\.|[^"\\])*"|true|false|null|-?\d+(?:\.\d+)?))*\s*\}\s*$`)

const codexActionVerbs = `creat(?:e|ing)|writ(?:e|ing)|runn?(?:ing)?|execut(?:e|ing)|check(?:ing)?|verif(?:y|ying)|updat(?:e|ing)|install(?:ing)?|edit(?:ing)?|mak(?:e|ing)`

var shellJSONLeakLeadInRe = regexp.MustCompile(`(?i)^(?:(?:next|first|then|okay|ok|alright)\b[\s,—–-]*)?(?:sure,\s*)?(?:now\s+)?(?:let\s+me\s+|i(?:'|’)?ll\s+|i\s+will\s+|i(?:'|’)?m\s+|i\s+am\s+)?(?:` + codexActionVerbs + `)\b`)

// Reports whether assistant text carries a tool call the model failed to emit structurally
func leakedToolCallText(text string) bool {
	if toolCallLeakRe.MatchString(text) {
		return true
	}
	loc := shellJSONLeakRe.FindStringIndex(text)
	if loc == nil {
		return false
	}
	lead := pySplitLines(strings.TrimSpace(text[:loc[0]]))
	if len(lead) == 0 {
		return false
	}
	return shellJSONLeakLeadInRe.MatchString(strings.TrimSpace(lead[len(lead)-1]))
}

// Python str.splitlines without keepends
func pySplitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029', '\r':
			out = append(out, string(runes[start:i]))
			if r == '\r' && i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}

// ---- harmony tokens ----

var harmonyControlTokenRe = regexp.MustCompile(`<\|(start|end|channel|message|constrain|return|call)\|>`)

const fullwidthPipe = "\uff5c"

var (
	codexTextPartTypes   = map[string]bool{"text": true, "input_text": true, "output_text": true}
	codexImagePartTypes  = map[string]bool{"image_url": true, "input_image": true}
	codexVideoPartTypes  = map[string]bool{"video": true, "video_url": true, "input_video": true}
	codexOutputTextTypes = map[string]bool{"output_text": true, "text": true}
)

const assistantImagePlaceholder = "[Assistant image omitted during replay]"

var (
	codexIncompleteStatuses      = map[string]bool{"queued": true, "in_progress": true, "incomplete": true}
	codexResponseMessageStatuses = map[string]bool{"completed": true, "incomplete": true, "in_progress": true}
)

const maxResponsesItemIDLength = 64

var responsesBuiltinToolTypes = map[string]bool{
	"web_search": true, "web_search_preview": true, "file_search": true, "code_interpreter": true, "image_generation": true,
	"computer_use_preview": true, "local_shell": true,
}

var serverSideToolCallTypes = map[string]bool{
	"web_search_call": true, "file_search_call": true, "code_interpreter_call": true, "image_generation_call": true,
	"computer_call": true, "local_shell_call": true, "mcp_call": true,
}

func nonblank(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

func nonemptyStr(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// Python str of a scalar or empty for nil
func strOrEmpty(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case json.Number:
		return t.String()
	case []any, map[string]any:
		raw, _ := json.Marshal(t)
		return string(raw)
	}
	return fmt.Sprintf("%v", v)
}

func lowerOrEmpty(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func codexList(v any) []any {
	l, _ := anyListOK(v)
	return l
}

// List of any JSON list shape, false for everything else
func anyListOK(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	case []string:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

func partType(part map[string]any) string {
	return lowerOrEmpty(part["type"])
}

func textTypeFor(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

// Replayed tool call arguments as a non empty JSON string
func coerceArguments(arguments any) string {
	var s string
	switch t := arguments.(type) {
	case map[string]any:
		raw, _ := json.Marshal(t)
		s = string(raw)
	case string:
		s = t
	default:
		s = strOrEmpty(arguments)
	}
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return strings.TrimSpace(s)
}

// Rewrites reserved Harmony wire tokens with fullwidth bars, matching across Unicode format controls
func neutralizeHarmonyTokens(text string) string {
	if text == "" || !strings.Contains(text, "<") || !strings.Contains(text, "|") {
		return text
	}
	hasFormat := false
	for _, r := range text {
		if r > 0x7f && unicode.Is(unicode.Cf, r) {
			hasFormat = true
			break
		}
	}
	replace := func(name string) string { return "<" + fullwidthPipe + name + fullwidthPipe + ">" }
	if !hasFormat {
		return harmonyControlTokenRe.ReplaceAllStringFunc(text, func(m string) string {
			return replace(harmonyControlTokenRe.FindStringSubmatch(m)[1])
		})
	}
	runes := []rune(text)
	positions := make([]int, 0, len(runes))
	var visible strings.Builder
	for i, r := range runes {
		if !unicode.Is(unicode.Cf, r) {
			positions = append(positions, i)
			visible.WriteRune(r)
		}
	}
	vis := visible.String()
	visRunes := []rune(vis)
	var out strings.Builder
	cursor := 0
	for _, loc := range harmonyControlTokenRe.FindAllStringSubmatchIndex(vis, -1) {
		startRune := len([]rune(vis[:loc[0]]))
		endRune := len([]rune(vis[:loc[1]]))
		start, end := positions[startRune], positions[endRune-1]+1
		out.WriteString(string(runes[cursor:start]))
		out.WriteString(replace(string(visRunes[len([]rune(vis[:loc[2]])):len([]rune(vis[:loc[3]]))])))
		cursor = end
	}
	out.WriteString(string(runes[cursor:]))
	return out.String()
}

var errHarmonyKey = errors.New("Reserved Harmony tokens in a JSON object key cannot be neutralized without changing its contract.")

// Neutralizes every string inside a JSON like value, rejecting reserved tokens in object keys
func neutralizeHarmonyStructure(value any) (any, error) {
	switch t := value.(type) {
	case string:
		return neutralizeHarmonyTokens(t), nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			v, err := neutralizeHarmonyStructure(item)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case []map[string]any:
		out := make([]any, len(t))
		for i, item := range t {
			v, err := neutralizeHarmonyStructure(item)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case map[string]any:
		for k := range t {
			if neutralizeHarmonyTokens(k) != k {
				return nil, errHarmonyKey
			}
		}
		out := make(map[string]any, len(t))
		for k, item := range t {
			v, err := neutralizeHarmonyStructure(item)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	}
	return value, nil
}

// ---- multimodal content ----

type codexContentPart struct {
	kind string
	text string
	part map[string]any
}

// Recognized text and image parts of a chat content list
func iterContentParts(content []any) []codexContentPart {
	var out []codexContentPart
	for _, p := range content {
		switch t := p.(type) {
		case string:
			if t != "" {
				out = append(out, codexContentPart{kind: "text", text: t})
			}
		case map[string]any:
			pt := partType(t)
			if codexTextPartTypes[pt] && nonemptyStr(t["text"]) {
				out = append(out, codexContentPart{kind: "text", text: t["text"].(string)})
			} else if codexImagePartTypes[pt] {
				out = append(out, codexContentPart{kind: "image", part: t})
			}
		}
	}
	return out
}

// Responses image part from a chat image part, text placeholders for assistant or unsupported images
func inputImagePart(part map[string]any, role string, keepEmptyURL bool) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": assistantImagePlaceholder}
	}
	urlVal, detail := part["image_url"], part["detail"]
	if m, ok := urlVal.(map[string]any); ok {
		urlVal = m["url"]
		if d, has := m["detail"]; has {
			detail = d
		}
	}
	if !nonemptyStr(urlVal) && !keepEmptyURL {
		return nil
	}
	u := strOrEmpty(urlVal)
	mime := unsupportedInlineImageMediaType(u)
	if mime == "image/svg+xml" {
		if png := rasterizeSVGDataURL(u); png != "" {
			u, mime = png, ""
		}
	}
	if mime != "" {
		return map[string]any{"type": "input_text", "text": fmt.Sprintf("[image omitted: %s is not a supported image format]", mime)}
	}
	image := map[string]any{"type": "input_image", "image_url": u}
	if nonblank(detail) {
		image["detail"] = strings.TrimSpace(detail.(string))
	}
	return image
}

// Chat multimodal content as Responses input parts, empty unless the content is a list
func chatContentToResponsesParts(content any, role string) ([]map[string]any, error) {
	list := codexList(content)
	for _, p := range list {
		if m, ok := p.(map[string]any); ok && codexVideoPartTypes[partType(m)] {
			return nil, fmt.Errorf("Codex Responses does not support %s input; use a video-capable provider.", partType(m))
		}
	}
	textType := textTypeFor(role)
	var converted []map[string]any
	for _, cp := range iterContentParts(list) {
		if cp.kind == "text" {
			converted = append(converted, map[string]any{"type": textType, "text": cp.text})
		} else if part := inputImagePart(cp.part, role, false); part != nil {
			converted = append(converted, part)
		}
	}
	return converted, nil
}

// SummarizeUserMessageForLog flattens content to text with an image count marker
func SummarizeUserMessageForLog(content any, sep string) string {
	list, ok := content.([]any)
	if !ok {
		return strOrEmpty(content)
	}
	parts := iterContentParts(list)
	var texts []string
	for _, p := range parts {
		if p.kind == "text" {
			texts = append(texts, p.text)
		}
	}
	images := len(parts) - len(texts)
	note := ""
	if images > 0 {
		plural := "s"
		if images == 1 {
			plural = ""
		}
		note = fmt.Sprintf("[%d image%s]", images, plural)
	}
	var bits []string
	for _, b := range []string{note, strings.TrimSpace(strings.Join(texts, sep))} {
		if b != "" {
			bits = append(bits, b)
		}
	}
	return strings.Join(bits, " ")
}

// ---- id helpers ----

// Keeps a call id within the 64 char cap with a deterministic surrogate
func clampResponsesCallID(callID string) string {
	if len(callID) <= maxResponsesItemIDLength {
		return callID
	}
	sum := sha256.Sum256([]byte(callID))
	return "call_" + hex.EncodeToString(sum[:])[:32]
}

// Canonical call id of an fc item id, empty for other ids
func canonicalCallIDFromFC(responseItemID string) string {
	if strings.HasPrefix(responseItemID, "fc_") && len(responseItemID) > 3 {
		return "call_" + responseItemID[3:]
	}
	return ""
}

// Stored tool id split into the call id and the response item id
func splitResponsesToolID(raw string) (string, string) {
	value := strings.TrimSpace(raw)
	if strings.Contains(value, "|") {
		callID, itemID, _ := strings.Cut(value, "|")
		return strings.TrimSpace(callID), strings.TrimSpace(itemID)
	}
	if value == "" {
		return "", ""
	}
	if strings.HasPrefix(value, "fc_") {
		return "", value
	}
	return value, ""
}

// Non blank call id from the explicit id, the embedded id, the canonical fc suffix, or a deterministic hash
func resolveCallID(rawCallID, rawItemID, fnName, arguments string, index int, canonicalizeFC bool) string {
	embeddedCallID, embeddedItemID := splitResponsesToolID(rawItemID)
	callID := embeddedCallID
	if strings.TrimSpace(rawCallID) != "" {
		callID = rawCallID
	}
	if strings.TrimSpace(callID) == "" && canonicalizeFC {
		callID = canonicalCallIDFromFC(embeddedItemID)
	}
	if strings.TrimSpace(callID) == "" {
		callID = tools.DeterministicCallID(fnName, arguments, index)
	}
	return strings.TrimSpace(callID)
}

var nonToolIDCharsRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Valid Responses function_call id starting with fc_
func deriveResponsesFunctionCallID(callID, responseItemID string) string {
	if strings.HasPrefix(strings.TrimSpace(responseItemID), "fc_") {
		return strings.TrimSpace(responseItemID)
	}
	source := strings.TrimSpace(callID)
	sanitized := nonToolIDCharsRe.ReplaceAllString(source, "")
	for _, candidate := range []string{source, sanitized} {
		if strings.HasPrefix(candidate, "fc_") {
			return candidate
		}
		if strings.HasPrefix(candidate, "call_") && len(candidate) > len("call_") {
			return "fc_" + candidate[len("call_"):]
		}
	}
	if sanitized != "" {
		if len(sanitized) > 48 {
			sanitized = sanitized[:48]
		}
		return "fc_" + sanitized
	}
	seed := source
	if seed == "" {
		seed = responseItemID
	}
	if seed == "" {
		seed = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	sum := sha1.Sum([]byte(seed))
	return "fc_" + hex.EncodeToString(sum[:])[:24]
}

var validToolNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var invalidToolNameCharsRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)
var underscoreRunRe = regexp.MustCompile(`_+`)

// CoerceToolName coerces a replayed tool name into the API name grammar, the fallback when nothing survives
func CoerceToolName(name any, fallback string) string {
	s, ok := name.(string)
	if !ok {
		return fallback
	}
	if validToolNameRe.MatchString(s) {
		return s
	}
	coerced := strings.Trim(underscoreRunRe.ReplaceAllString(invalidToolNameCharsRe.ReplaceAllString(strings.TrimSpace(s), "_"), "_"), "_")
	if len(coerced) > 64 {
		coerced = coerced[:64]
	}
	if coerced == "" {
		return fallback
	}
	return coerced
}

// ---- schema conversion ----

// Chat completions tool schemas as Responses function tools, nil when none has a name
func responsesTools(toolDefs []map[string]any) []map[string]any {
	var converted []map[string]any
	for _, item := range toolDefs {
		fn, _ := item["function"].(map[string]any)
		if !nonblank(fn["name"]) {
			continue
		}
		strict := false
		if b, ok := fn["strict"].(bool); ok {
			strict = b
		}
		params, has := fn["parameters"]
		if !has {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		desc := ""
		if d, has := fn["description"]; has {
			desc = strOrEmpty(d)
		}
		converted = append(converted, map[string]any{
			"type": "function", "name": fn["name"], "description": desc, "strict": strict, "parameters": params,
		})
	}
	return converted
}

// ---- message items ----

// Replayed assistant message status normalized to the API vocabulary
func normalizeResponsesMessageStatus(value any, def string) string {
	s, ok := value.(string)
	if !ok {
		return def
	}
	status := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_"), " ", "_")
	if codexResponseMessageStatuses[status] {
		return status
	}
	return def
}

// Assistant message item with id and phase added only when non empty
func messageItem(content []map[string]any, status, itemID, phase string) map[string]any {
	item := map[string]any{"type": "message", "role": "assistant", "status": status, "content": content}
	if itemID != "" {
		item["id"] = itemID
	}
	if phase != "" {
		item["phase"] = phase
	}
	return item
}

var roleMessagePhases = map[string]bool{"commentary": true, "final_answer": true}

// Typed role message input item, assistant phase kept only for values the API accepts
func roleMessageItem(role string, content any, phase any) map[string]any {
	item := map[string]any{"type": "message", "role": role, "content": content}
	if role == "assistant" {
		if cleaned := lowerOrEmpty(phase); roleMessagePhases[cleaned] {
			item["phase"] = cleaned
		}
	}
	return item
}

// Replayable assistant message item from a stored one with the id policy of the endpoint
func assistantMessageItem(raw map[string]any, content []map[string]any, dropReplayItemIDs bool, currentIssuerKind string) map[string]any {
	itemID, phase := raw["id"], raw["phase"]
	keepID := !dropReplayItemIDs && nonblank(itemID) && len(strings.TrimSpace(itemID.(string))) <= maxResponsesItemIDLength
	if keepID && currentIssuerKind == "codex_backend" && !strings.HasPrefix(strings.TrimSpace(itemID.(string)), "msg") {
		keepID = false
	}
	id := ""
	if keepID {
		id = strings.TrimSpace(itemID.(string))
	}
	ph := ""
	if nonblank(phase) {
		ph = strings.TrimSpace(phase.(string))
	}
	return messageItem(content, normalizeResponsesMessageStatus(raw["status"], "completed"), id, ph)
}

// One chat message with its decoded Responses sidecars
type codexMsg struct {
	m              *state.Message
	reasoningItems []map[string]any
	messageItems   []map[string]any
	trimmed        bool
}

func codexMsgOf(m *state.Message) *codexMsg {
	return &codexMsg{m: m, reasoningItems: decodeCodexItems(m.CodexReasoningItems), messageItems: decodeCodexItems(m.CodexMessageItems)}
}

func codexMsgsOf(msgs []*state.Message) []*codexMsg {
	out := make([]*codexMsg, 0, len(msgs))
	for _, m := range msgs {
		if m != nil {
			out = append(out, codexMsgOf(m))
		}
	}
	return out
}

func (c *codexMsg) phase() any {
	if c.m.DisplayMetadata == nil {
		return nil
	}
	return c.m.DisplayMetadata["phase"]
}

// Options steering chat history conversion to Responses input
type codexConvertOptions struct {
	isXAI                    bool
	dropReplayItemIDs        bool
	replayEncryptedReasoning bool
	currentIssuerKind        string
	currentIssuerModel       string
	nativeCompactionEligible bool
}

// Persisted encrypted reasoning and compaction items of one assistant turn fit for replay
func replayReasoningItems(msg *codexMsg, seen map[string]bool, opts codexConvertOptions) []map[string]any {
	var replayed []map[string]any
	for _, ri := range msg.reasoningItems {
		if !pyTruthy(ri["encrypted_content"]) {
			continue
		}
		itemID := stringOf(ri["id"])
		if (itemID != "" && seen[itemID]) || (stringOf(ri["type"]) == "compaction" && !opts.nativeCompactionEligible) {
			continue
		}
		rawKind, hasKind := ri["_issuer_kind"]
		itemIssuer := ""
		if hasKind && rawKind != nil {
			itemIssuer = canonicalIssuerKind(stringOf(rawKind))
		}
		itemModel := stringOf(ri["_issuer_model"])
		foreignIssuer := opts.currentIssuerKind != "" && itemIssuer != "" && itemIssuer != opts.currentIssuerKind
		foreignModel := !modelCanReadBlob(opts.currentIssuerKind, itemModel, opts.currentIssuerModel)
		if foreignIssuer || foreignModel {
			crossIssuerWarnOnce.Do(func() {
				codexLog.Warn(fmt.Sprintf("Dropping reasoning item minted by %s/%s while calling %s/%s — encrypted_content is sealed to its issuer and model. This happens when a session switches model mid-conversation.",
					itemIssuer, itemModel, opts.currentIssuerKind, opts.currentIssuerModel))
			})
			continue
		}
		item := make(map[string]any, len(ri))
		for k, v := range ri {
			if k != "id" && k != "_issuer_kind" && k != "_issuer_model" {
				item[k] = v
			}
		}
		replayed = append(replayed, item)
		if itemID != "" {
			seen[itemID] = true
		}
	}
	return replayed
}

// Exact assistant message items replayed for prefix cache hits, ids dropped on reasoning linked turns
func replayMessageItems(msg *codexMsg, dropReplayItemIDs bool, currentIssuerKind string) []map[string]any {
	var replayed []map[string]any
	linked := msg.trimmed
	for _, ri := range msg.reasoningItems {
		if enc, ok := ri["encrypted_content"].(string); ok && enc != "" {
			linked = true
		}
	}
	for _, raw := range msg.messageItems {
		if stringOf(raw["type"]) != "message" || stringOf(raw["role"]) != "assistant" {
			continue
		}
		var content []map[string]any
		for _, p := range codexList(raw["content"]) {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if codexOutputTextTypes[strings.TrimSpace(stringOf(part["type"]))] {
				text := ""
				if t, has := part["text"]; has {
					text = strOrEmpty(t)
				}
				content = append(content, map[string]any{"type": "output_text", "text": text})
			}
		}
		if len(content) == 0 {
			continue
		}
		item := raw
		if linked && raw["id"] != nil && raw["id"] != "" {
			item = make(map[string]any, len(raw))
			for k, v := range raw {
				if k != "id" {
					item[k] = v
				}
			}
		}
		replayed = append(replayed, assistantMessageItem(item, content, dropReplayItemIDs, currentIssuerKind))
	}
	return replayed
}

// Per request wire ids that suffix repeated call ids so replayed pairs stay unique
type wireCallIDs struct {
	seen  map[string]int
	queue map[string][]string
}

func newWireCallIDs() *wireCallIDs {
	return &wireCallIDs{seen: map[string]int{}, queue: map[string][]string{}}
}

func (w *wireCallIDs) forCall(callID string) string {
	base := clampResponsesCallID(callID)
	n := w.seen[base]
	w.seen[base] = n + 1
	wire := base
	if n > 0 {
		wire = clampResponsesCallID(fmt.Sprintf("%s_dup%d", base, n))
	}
	w.queue[base] = append(w.queue[base], wire)
	return wire
}

func (w *wireCallIDs) forOutput(callID string) string {
	base := clampResponsesCallID(callID)
	q := w.queue[base]
	if len(q) == 0 {
		return base
	}
	w.queue[base] = q[1:]
	return q[0]
}

// Assistant tool calls as function_call items
func replayToolCallItems(msg *codexMsg, startIndex int, wire *wireCallIDs) []map[string]any {
	var replayed []map[string]any
	for _, tc := range msg.m.ToolCalls {
		if strings.TrimSpace(tc.Function.Name) == "" {
			continue
		}
		index := startIndex + len(replayed)
		arguments := tc.Function.Arguments
		if arguments == "" {
			arguments = "{}"
		}
		callID := resolveCallID("", tc.ID, tc.Function.Name, arguments, index, true)
		wireID := clampResponsesCallID(callID)
		if wire != nil {
			wireID = wire.forCall(callID)
		}
		replayed = append(replayed, map[string]any{
			"type": "function_call", "call_id": wireID, "name": CoerceToolName(tc.Function.Name, "fn"), "arguments": coerceArguments(arguments),
		})
	}
	return replayed
}

// Tool role message as a function_call_output item, none when unpairable
func toolOutputItems(msg *codexMsg, wire *wireCallIDs) ([]map[string]any, error) {
	rawID := msg.m.ToolCallID
	callID, itemID := splitResponsesToolID(rawID)
	if strings.TrimSpace(callID) == "" {
		callID = canonicalCallIDFromFC(itemID)
		if callID == "" && strings.TrimSpace(rawID) != "" {
			callID = strings.TrimSpace(rawID)
		}
	}
	if strings.TrimSpace(callID) == "" {
		return nil, nil
	}
	var output any
	if list, ok := msg.m.Content.([]any); ok {
		parts, err := chatContentToResponsesParts(list, "user")
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			output = ""
		} else {
			output = parts
		}
	} else {
		output = strOrEmpty(msg.m.Content)
	}
	wireID := clampResponsesCallID(callID)
	if wire != nil {
		wireID = wire.forOutput(callID)
	}
	return []map[string]any{{"type": "function_call_output", "call_id": wireID, "output": output}}, nil
}

// Chat history converted to Responses input items with the source message of each item
func chatMessagesToResponsesInput(msgs []*codexMsg, opts codexConvertOptions) ([]map[string]any, error) {
	var items []map[string]any
	var sources []*codexMsg
	seen := map[string]bool{}
	wire := newWireCallIDs()
	typedTextOnly := opts.currentIssuerKind == "codex_backend"
	emit := func(newItems []map[string]any, msg *codexMsg) {
		items = append(items, newItems...)
		for range newItems {
			sources = append(sources, msg)
		}
	}
	for _, msg := range msgs {
		role := msg.m.Role
		if role == "tool" {
			out, err := toolOutputItems(msg, wire)
			if err != nil {
				return nil, err
			}
			emit(out, msg)
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}
		content := msg.m.Content
		contentParts, err := chatContentToResponsesParts(content, role)
		if err != nil {
			return nil, err
		}
		textType := textTypeFor(role)
		contentText := ""
		if _, isList := content.([]any); isList {
			var b strings.Builder
			for _, p := range contentParts {
				if p["type"] == textType {
					b.WriteString(p["text"].(string))
				}
			}
			contentText = b.String()
		} else {
			contentText = strOrEmpty(content)
		}
		wireContent := func(value any) any {
			if s, ok := value.(string); ok && typedTextOnly {
				return []map[string]any{{"type": textType, "text": s}}
			}
			return value
		}
		partsOrText := func() any {
			if len(contentParts) > 0 {
				return contentParts
			}
			return contentText
		}
		if role == "user" {
			emit([]map[string]any{roleMessageItem(role, wireContent(partsOrText()), nil)}, msg)
			continue
		}
		var reasoningItems []map[string]any
		if opts.replayEncryptedReasoning {
			reasoningItems = replayReasoningItems(msg, seen, opts)
		}
		emit(reasoningItems, msg)
		messageItems := replayMessageItems(msg, opts.dropReplayItemIDs, opts.currentIssuerKind)
		emit(messageItems, msg)
		var fallback any
		hasFallback := false
		if len(messageItems) == 0 {
			switch {
			case len(contentParts) > 0:
				fallback, hasFallback = contentParts, true
			case strings.TrimSpace(contentText) != "":
				fallback, hasFallback = contentText, true
			case len(reasoningItems) > 0:
				fallback, hasFallback = "", true
			}
		}
		start := len(items)
		if hasFallback {
			start++
		}
		toolItems := replayToolCallItems(msg, start, wire)
		if hasFallback && !(fallback == "" && len(toolItems) > 0) {
			follower := fallback
			if fallback == "" {
				follower = " "
			}
			emit([]map[string]any{roleMessageItem("assistant", wireContent(follower), msg.phase())}, msg)
		}
		emit(toolItems, msg)
	}
	if !opts.nativeCompactionEligible {
		return items, nil
	}
	return prunePreCheckpointItems(items, retainedUserMessageTokenBudget, retainedSummaryTokenBudget, true, sources), nil
}

// Which special Responses route a client talks to
type responsesRouteFlags struct {
	isCodexBackend bool
	isXAI          bool
	endpoint       responsesEndpointQuirks
}

// Route flags from provider and base URL with exact host or subdomain checks
func classifyResponsesRoute(provider, baseURL string) responsesRouteFlags {
	host := strings.ToLower(baseURLHostname(baseURL))
	lower := strings.ToLower(baseURL)
	hostIs := func(domain string) bool { return host == domain || strings.HasSuffix(host, "."+domain) }
	return responsesRouteFlags{
		isCodexBackend: provider == "openai-codex" || (hostIs("chatgpt.com") && strings.Contains(lower, "/backend-api/codex")),
		isXAI:          provider == "xai" || provider == "xai-oauth" || host == "api.x.ai",
		endpoint:       responsesEndpointQuirksFor(baseURL),
	}
}

// ---- preflight ----

type codexPreflightCtx struct {
	sanitizeText      func(string) string
	sanitizeHarmony   bool
	dropReplayItemIDs bool
	seenIDs           map[string]bool
}

func preflightFunctionCall(item map[string]any, idx int, ctx *codexPreflightCtx) (map[string]any, error) {
	callID, name := item["call_id"], item["name"]
	if !nonblank(callID) {
		return nil, fmt.Errorf("Codex Responses input[%d] function_call is missing call_id.", idx)
	}
	if !nonblank(name) {
		return nil, fmt.Errorf("Codex Responses input[%d] function_call is missing name.", idx)
	}
	args, has := item["arguments"]
	if !has {
		args = "{}"
	}
	return map[string]any{
		"type": "function_call", "call_id": strings.TrimSpace(callID.(string)), "name": CoerceToolName(name, "fn"),
		"arguments": ctx.sanitizeText(coerceArguments(args)),
	}, nil
}

func preflightFunctionCallOutput(item map[string]any, idx int, ctx *codexPreflightCtx) (map[string]any, error) {
	callID := item["call_id"]
	if !nonblank(callID) {
		return nil, fmt.Errorf("Codex Responses input[%d] function_call_output is missing call_id.", idx)
	}
	output, has := item["output"]
	if !has {
		output = ""
	}
	var outputValue any
	if list, ok := anyListOK(output); ok {
		var cleaned []map[string]any
		for _, p := range list {
			part, _ := p.(map[string]any)
			if part == nil {
				continue
			}
			switch part["type"] {
			case "input_text":
				if nonemptyStr(part["text"]) {
					cleaned = append(cleaned, map[string]any{"type": "input_text", "text": ctx.sanitizeText(part["text"].(string))})
				}
			case "input_image":
				if nonemptyStr(part["image_url"]) {
					cleaned = append(cleaned, inputImagePart(part, "user", false))
				}
			}
		}
		if len(cleaned) == 0 {
			outputValue = ""
		} else {
			outputValue = cleaned
		}
	} else {
		outputValue = ctx.sanitizeText(strOrEmpty(output))
	}
	return map[string]any{"type": "function_call_output", "call_id": strings.TrimSpace(callID.(string)), "output": outputValue}, nil
}

// Reasoning and compaction items forwarded with API defined fields only
func preflightEncrypted(item map[string]any, idx int, ctx *codexPreflightCtx) (map[string]any, error) {
	encrypted := item["encrypted_content"]
	if !nonemptyStr(encrypted) {
		return nil, nil
	}
	if item["type"] == "compaction" {
		return map[string]any{"type": "compaction", "encrypted_content": encrypted}, nil
	}
	if itemID, ok := item["id"].(string); ok && itemID != "" {
		if ctx.seenIDs[itemID] {
			return nil, nil
		}
		ctx.seenIDs[itemID] = true
	}
	summary := codexList(item["summary"])
	if summary == nil {
		summary = []any{}
	}
	var summaryOut any = summary
	if ctx.sanitizeHarmony {
		v, err := neutralizeHarmonyStructure(summary)
		if err != nil {
			return nil, err
		}
		summaryOut = v
	}
	return map[string]any{"type": "reasoning", "encrypted_content": encrypted, "summary": summaryOut}, nil
}

func preflightMessage(item map[string]any, idx int, ctx *codexPreflightCtx) (map[string]any, error) {
	content := item["content"]
	list, isList := anyListOK(content)
	_, hasID := item["id"]
	_, hasStatus := item["status"]
	if !(item["role"] == "assistant" && isList && (hasID || hasStatus)) {
		return preflightRoleMessage(item, idx, ctx)
	}
	var normalized []map[string]any
	for partIdx, p := range list {
		part, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Codex Responses input[%d] message content[%d] must be an object.", idx, partIdx)
		}
		pt, _ := part["type"].(string)
		if !codexOutputTextTypes[pt] {
			return nil, fmt.Errorf("Codex Responses input[%d] message content[%d] has unsupported type %s.", idx, partIdx, pyRepr(part["type"]))
		}
		text := ""
		if t, has := part["text"]; has {
			text = strOrEmpty(t)
		}
		normalized = append(normalized, map[string]any{"type": "output_text", "text": ctx.sanitizeText(text)})
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("Codex Responses input[%d] message item must contain at least one text part.", idx)
	}
	return assistantMessageItem(item, normalized, ctx.dropReplayItemIDs, ""), nil
}

// Python repr of a scalar for error messages
func pyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		if strings.Contains(t, "'") && !strings.Contains(t, `"`) {
			return `"` + t + `"`
		}
		return "'" + strings.ReplaceAll(t, "'", `\'`) + "'"
	}
	return strOrEmpty(v)
}

// User or assistant role message with string content or Responses parts validated and retyped
func preflightRoleMessage(item map[string]any, idx int, ctx *codexPreflightCtx) (map[string]any, error) {
	role, _ := item["role"].(string)
	if role != "user" && role != "assistant" {
		return nil, fmt.Errorf("Codex Responses input[%d] has unsupported item shape (type=%s, role=%s).", idx, pyRepr(item["type"]), pyRepr(item["role"]))
	}
	content, has := item["content"]
	if !has {
		content = ""
	}
	list, isList := anyListOK(content)
	if !isList {
		return roleMessageItem(role, ctx.sanitizeText(strOrEmpty(content)), item["phase"]), nil
	}
	textType := textTypeFor(role)
	validated := []map[string]any{}
	for partIdx, p := range list {
		switch part := p.(type) {
		case string:
			if part != "" {
				validated = append(validated, map[string]any{"type": textType, "text": ctx.sanitizeText(part)})
			}
		case map[string]any:
			pt := partType(part)
			switch {
			case codexTextPartTypes[pt]:
				text, has := part["text"]
				if !has {
					text = ""
				}
				s, isStr := text.(string)
				if !isStr {
					s = strOrEmpty(text)
				}
				validated = append(validated, map[string]any{"type": textType, "text": ctx.sanitizeText(s)})
			case codexImagePartTypes[pt]:
				validated = append(validated, inputImagePart(part, role, true))
			default:
				return nil, fmt.Errorf("Codex Responses input[%d].content[%d] has unsupported type %s.", idx, partIdx, pyRepr(part["type"]))
			}
		default:
			return nil, fmt.Errorf("Codex Responses input[%d].content[%d] must be an object or string.", idx, partIdx)
		}
	}
	return roleMessageItem(role, validated, item["phase"]), nil
}

// Validated and sanitized Responses input items
func preflightCodexInputItems(rawItems any, dropReplayItemIDs, sanitizeHarmony bool) ([]map[string]any, error) {
	list, ok := rawItems.([]any)
	if !ok {
		if typed, isTyped := rawItems.([]map[string]any); isTyped {
			list = make([]any, len(typed))
			for i, m := range typed {
				list[i] = m
			}
		} else {
			return nil, errors.New("Codex Responses input must be a list of input items.")
		}
	}
	sanitize := func(s string) string { return s }
	if sanitizeHarmony {
		sanitize = neutralizeHarmonyTokens
	}
	ctx := &codexPreflightCtx{sanitizeText: sanitize, sanitizeHarmony: sanitizeHarmony, dropReplayItemIDs: dropReplayItemIDs, seenIDs: map[string]bool{}}
	normalized := []map[string]any{}
	for idx, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Codex Responses input[%d] must be an object.", idx)
		}
		var out map[string]any
		var err error
		switch item["type"] {
		case "function_call":
			out, err = preflightFunctionCall(item, idx, ctx)
		case "function_call_output":
			out, err = preflightFunctionCallOutput(item, idx, ctx)
		case "reasoning", "compaction":
			out, err = preflightEncrypted(item, idx, ctx)
		case "message":
			out, err = preflightMessage(item, idx, ctx)
		default:
			out, err = preflightRoleMessage(item, idx, ctx)
		}
		if err != nil {
			return nil, err
		}
		if out != nil {
			normalized = append(normalized, out)
		}
	}
	return normalized, nil
}

func preflightTool(tool any, idx int) (map[string]any, error) {
	t, ok := tool.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Codex Responses tools[%d] must be an object.", idx)
	}
	toolType, _ := t["type"].(string)
	if responsesBuiltinToolTypes[toolType] {
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = v
		}
		return out, nil
	}
	if toolType != "function" {
		return nil, fmt.Errorf("Codex Responses tools[%d] has unsupported type %s.", idx, pyRepr(t["type"]))
	}
	name, params := t["name"], t["parameters"]
	if !nonblank(name) {
		return nil, fmt.Errorf("Codex Responses tools[%d] is missing a valid name.", idx)
	}
	if _, ok := params.(map[string]any); !ok {
		return nil, fmt.Errorf("Codex Responses tools[%d] is missing valid parameters.", idx)
	}
	desc := ""
	if d, has := t["description"]; has {
		desc = strOrEmpty(d)
	}
	strict := false
	if s, has := t["strict"]; has {
		strict = pyTruthy(s)
	}
	return map[string]any{"type": "function", "name": strings.TrimSpace(name.(string)), "description": desc, "strict": strict, "parameters": params}, nil
}

// Python truthiness of a JSON value
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case []any:
		return len(t) > 0
	case []map[string]any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// Finite positive number from any JSON numeric shape
func finitePositive(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		return 0, false
	case int:
		return float64(t), t > 0
	case int64:
		return float64(t), t > 0
	case float64:
		return t, t > 0 && t < 1e308 && t == t
	}
	return 0, false
}

func isNumber(v any) bool {
	switch v.(type) {
	case int, int64, float64:
		return true
	}
	return false
}

var preflightOptionalKeys = []string{
	"reasoning", "include", "service_tier", "text", "max_output_tokens", "timeout", "temperature",
	"tool_choice", "parallel_tool_calls", "prompt_cache_key", "prompt_cache_retention", "context_management",
}

var preflightAllowedKeys = func() map[string]bool {
	allowed := map[string]bool{"model": true, "instructions": true, "input": true, "tools": true, "store": true, "extra_headers": true, "extra_body": true}
	for _, k := range preflightOptionalKeys {
		allowed[k] = true
	}
	return allowed
}()

// Optional scalar request field accepted and coerced, false when it is silently dropped
func preflightOptionalField(key string, v any) (any, bool) {
	switch key {
	case "reasoning":
		_, ok := v.(map[string]any)
		return v, ok
	case "include":
		_, ok := v.([]any)
		return v, ok
	case "service_tier":
		if nonblank(v) {
			return strings.TrimSpace(v.(string)), true
		}
		return nil, false
	case "text":
		m, ok := v.(map[string]any)
		return v, ok && len(m) > 0
	case "max_output_tokens":
		if b, isBool := v.(bool); isBool {
			return int(boolInt(b)), b
		}
		if isNumber(v) {
			f := toFloat(v)
			return int(f), f > 0
		}
		return nil, false
	case "timeout":
		f, ok := finitePositive(v)
		return f, ok
	case "temperature":
		if isNumber(v) {
			return toFloat(v), true
		}
		return nil, false
	case "tool_choice", "parallel_tool_calls", "prompt_cache_key", "prompt_cache_retention":
		return v, v != nil
	case "context_management":
		l, ok := v.([]any)
		if !ok {
			if typed, isTyped := v.([]map[string]any); isTyped {
				return v, len(typed) > 0
			}
		}
		return v, ok && len(l) > 0
	}
	return nil, false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case float64:
		return t
	}
	return 0
}

// Validated and sanitized Codex request kwargs in the preflight contract shape
func preflightCodexAPIKwargs(apiKwargs map[string]any, allowStream, dropReplayItemIDs, sanitizeHarmony bool) (map[string]any, error) {
	if apiKwargs == nil {
		return nil, errors.New("Codex Responses request must be a dict.")
	}
	var missing []string
	for _, k := range []string{"input", "instructions", "model"} {
		if _, has := apiKwargs[k]; !has {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("Codex Responses request missing required field(s): %s.", strings.Join(missing, ", "))
	}
	model := apiKwargs["model"]
	if !nonblank(model) {
		return nil, errors.New("Codex Responses request 'model' must be a non-empty string.")
	}
	instructions := strings.TrimSpace(strOrEmpty(apiKwargs["instructions"]))
	if instructions == "" {
		instructions = config.DefaultIdentity
	}
	if sanitizeHarmony {
		instructions = neutralizeHarmonyTokens(instructions)
	}
	inputItems, err := preflightCodexInputItems(apiKwargs["input"], dropReplayItemIDs, sanitizeHarmony)
	if err != nil {
		return nil, err
	}
	normalized := map[string]any{"model": strings.TrimSpace(model.(string)), "instructions": instructions, "input": inputItems, "store": false}
	if rawTools, has := apiKwargs["tools"]; has && rawTools != nil {
		var list []any
		switch t := rawTools.(type) {
		case []any:
			list = t
		case []map[string]any:
			for _, m := range t {
				list = append(list, m)
			}
			if list == nil {
				list = []any{}
			}
		default:
			return nil, errors.New("Codex Responses request 'tools' must be a list when provided.")
		}
		normalizedTools := make([]map[string]any, 0, len(list))
		for idx, tool := range list {
			nt, err := preflightTool(tool, idx)
			if err != nil {
				return nil, err
			}
			normalizedTools = append(normalizedTools, nt)
		}
		if sanitizeHarmony {
			v, err := neutralizeHarmonyStructure(normalizedTools)
			if err != nil {
				return nil, err
			}
			normalized["tools"] = mapsOfList(v.([]any))
		} else {
			normalized["tools"] = normalizedTools
		}
	}
	if store, has := apiKwargs["store"]; has && store != false {
		return nil, errors.New("Codex Responses contract requires 'store' to be false.")
	}
	for _, key := range preflightOptionalKeys {
		if v, ok := preflightOptionalField(key, apiKwargs[key]); ok {
			normalized[key] = v
		}
	}
	extraHeaders, err := optionalDict(apiKwargs, "extra_headers")
	if err != nil {
		return nil, err
	}
	for k := range extraHeaders {
		if strings.TrimSpace(k) == "" {
			return nil, errors.New("Codex Responses request 'extra_headers' keys must be non-empty strings.")
		}
	}
	headers := map[string]string{}
	for k, v := range extraHeaders {
		if v != nil {
			headers[strings.TrimSpace(k)] = strOrEmpty(v)
		}
	}
	if len(headers) > 0 {
		normalized["extra_headers"] = headers
	}
	extraBody, err := optionalDict(apiKwargs, "extra_body")
	if err != nil {
		return nil, err
	}
	if len(extraBody) > 0 {
		copyBody := make(map[string]any, len(extraBody))
		for k, v := range extraBody {
			copyBody[k] = v
		}
		normalized["extra_body"] = copyBody
	}
	stream, hasStream := apiKwargs["stream"]
	if !allowStream && hasStream {
		return nil, errors.New("Codex Responses stream flag is only allowed in fallback streaming requests.")
	}
	if allowStream && hasStream && stream != nil {
		if stream != true {
			return nil, errors.New("Codex Responses 'stream' must be true when set.")
		}
		normalized["stream"] = true
	}
	lowerModel := strings.ToLower(strOrEmpty(apiKwargs["model"]))
	if strings.HasPrefix(lowerModel, "grok-") || strings.HasPrefix(lowerModel, "x-ai/grok-") {
		if toolList, ok := normalized["tools"].([]map[string]any); ok && len(toolList) > 0 {
			normalized["tools"] = codexStripSlashEnum(toolList)
		}
	}
	var unexpected []string
	for k := range apiKwargs {
		if !preflightAllowedKeys[k] && !(allowStream && k == "stream") {
			unexpected = append(unexpected, k)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return nil, fmt.Errorf("Codex Responses request has unsupported field(s): %s.", strings.Join(unexpected, ", "))
	}
	return normalized, nil
}

func mapsOfList(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func optionalDict(apiKwargs map[string]any, key string) (map[string]any, error) {
	v, has := apiKwargs[key]
	if !has || v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case map[string]any:
		return t, nil
	case map[string]string:
		out := make(map[string]any, len(t))
		for k, s := range t {
			out[k] = s
		}
		return out, nil
	}
	return nil, fmt.Errorf("Codex Responses request '%s' must be an object.", key)
}

// ---- response extraction ----

// Non empty text of each part, filtered by type when types is non nil
func textChunks(parts any, types map[string]bool) []string {
	var out []string
	for _, p := range codexList(parts) {
		part, _ := p.(map[string]any)
		if part == nil {
			continue
		}
		if types != nil && !types[stringOf(part["type"])] {
			continue
		}
		if nonemptyStr(part["text"]) {
			out = append(out, part["text"].(string))
		}
	}
	return out
}

// Assistant text of a message output item with refusal parts counted as text
func extractResponsesMessageText(item map[string]any) string {
	var b strings.Builder
	for _, p := range codexList(item["content"]) {
		part, _ := p.(map[string]any)
		if part == nil {
			continue
		}
		var text any
		switch stringOf(part["type"]) {
		case "refusal":
			text = part["refusal"]
		case "output_text", "text":
			text = part["text"]
		}
		if nonemptyStr(text) {
			b.WriteString(text.(string))
		}
	}
	return strings.TrimSpace(b.String())
}

// Compact reasoning text of a reasoning item, summary first then text
func extractResponsesReasoningText(item map[string]any) string {
	chunks := textChunks(item["summary"], nil)
	if len(chunks) > 0 {
		return strings.TrimSpace(strings.Join(chunks, "\n"))
	}
	if t, ok := item["text"].(string); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// Code and message of a response error payload or a status based default
func formatResponsesError(errObj any, status string) string {
	field := func(name string) string {
		m, _ := errObj.(map[string]any)
		if m == nil {
			return ""
		}
		v := m[name]
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		if pyTruthy(v) {
			return strings.TrimSpace(strOrEmpty(v))
		}
		return ""
	}
	code, message := field("code"), field("message")
	if code != "" && message != "" {
		return code + ": " + message
	}
	if message != "" {
		return message
	}
	if code != "" {
		return code
	}
	if pyTruthy(errObj) {
		return strOrEmpty(errObj)
	}
	return fmt.Sprintf("Responses API returned status '%s'", status)
}

// ---- full response normalization ----

// One tool call recovered from a Responses output item
type codexToolCall struct {
	ID             string
	CallID         string
	ResponseItemID string
	Name           string
	Arguments      string
}

func responseToolCall(item map[string]any, itemType string, index int) codexToolCall {
	name := stringOf(item["name"])
	argsKey := "input"
	if itemType == "function_call" {
		argsKey = "arguments"
	}
	args, has := item[argsKey]
	if !has {
		args = "{}"
	}
	arguments, isStr := args.(string)
	if !isStr {
		raw, _ := json.Marshal(args)
		arguments = string(raw)
	}
	rawItemID := stringOf(item["id"])
	callID := resolveCallID(stringOf(item["call_id"]), rawItemID, name, arguments, index, false)
	return codexToolCall{ID: callID, CallID: callID, ResponseItemID: deriveResponsesFunctionCallID(callID, rawItemID), Name: name, Arguments: arguments}
}

// Replayable reasoning or compaction item captured with provenance stamps, nil without a blob
func captureEncryptedItem(item map[string]any, itemType, issuerKind, issuerModel string) map[string]any {
	encrypted, ok := item["encrypted_content"].(string)
	if !ok || encrypted == "" {
		return nil
	}
	raw := map[string]any{"type": itemType, "encrypted_content": encrypted}
	if issuerKind != "" {
		raw["_issuer_kind"] = issuerKind
	}
	if issuerModel != "" {
		raw["_issuer_model"] = issuerModel
	}
	if itemType != "reasoning" {
		return raw
	}
	itemID, _ := item["id"].(string)
	if strings.HasPrefix(itemID, "rs_tmp_") {
		codexLog.Debug("Skipping transient Codex reasoning item during normalization: " + itemID)
		return nil
	}
	if itemID != "" {
		raw["id"] = itemID
	}
	if summary, ok := item["summary"].([]any); ok {
		parts := []map[string]any{}
		for _, p := range summary {
			part, _ := p.(map[string]any)
			if part == nil {
				continue
			}
			if text, ok := part["text"].(string); ok {
				parts = append(parts, map[string]any{"type": "summary_text", "text": text})
			}
		}
		raw["summary"] = parts
	}
	return raw
}

// Accumulated view of one Responses output list
type codexOutputScan struct {
	contentParts, reasoningParts []string
	toolCalls                    []codexToolCall
	reasoningItemsRaw            []map[string]any
	messageItemsRaw              []map[string]any
	hasIncompleteItems           bool
	sawStreamingOrItemIncomplete bool
	sawCommentaryPhase           bool
	sawFinalAnswerPhase          bool
	sawReasoningItem             bool
}

func newCodexOutputScan(status string) *codexOutputScan {
	return &codexOutputScan{hasIncompleteItems: codexIncompleteStatuses[status], sawStreamingOrItemIncomplete: status == "queued" || status == "in_progress"}
}

func (s *codexOutputScan) scan(output []any, issuerKind, issuerModel string) {
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		itemType := stringOf(item["type"])
		itemStatus := lowerOrEmpty(item["status"])
		if codexIncompleteStatuses[itemStatus] && !serverSideToolCallTypes[itemType] {
			s.hasIncompleteItems = true
			s.sawStreamingOrItemIncomplete = true
		}
		switch {
		case itemType == "message":
			s.message(item, itemStatus)
		case itemType == "reasoning" || itemType == "compaction":
			if itemType == "reasoning" {
				s.sawReasoningItem = true
				if text := extractResponsesReasoningText(item); text != "" {
					s.reasoningParts = append(s.reasoningParts, text)
				}
			}
			if captured := captureEncryptedItem(item, itemType, issuerKind, issuerModel); captured != nil {
				s.reasoningItemsRaw = append(s.reasoningItemsRaw, captured)
				if itemType == "compaction" {
					codexLog.Info(fmt.Sprintf("Native Responses compaction item captured (%d chars encrypted).", len(captured["encrypted_content"].(string))))
				}
			}
		case itemType == "custom_tool_call" || (itemType == "function_call" && !codexIncompleteStatuses[itemStatus]):
			s.toolCalls = append(s.toolCalls, responseToolCall(item, itemType, len(s.toolCalls)))
		}
	}
}

func (s *codexOutputScan) message(item map[string]any, itemStatus string) {
	phase := lowerOrEmpty(item["phase"])
	isCommentary := phase == "commentary" || phase == "analysis"
	s.sawCommentaryPhase = s.sawCommentaryPhase || isCommentary
	s.sawFinalAnswerPhase = s.sawFinalAnswerPhase || phase == "final_answer" || phase == "final"
	text := extractResponsesMessageText(item)
	if text == "" {
		return
	}
	if isCommentary {
		s.reasoningParts = append(s.reasoningParts, text)
	} else {
		s.contentParts = append(s.contentParts, text)
	}
	itemID, _ := item["id"].(string)
	s.messageItemsRaw = append(s.messageItemsRaw, messageItem([]map[string]any{{"type": "output_text", "text": text}},
		normalizeResponsesMessageStatus(itemStatus, "completed"), itemID, phase))
}

// Issuer kinds whose reasoning only completed status is not final
func untrustedCompletedIssuers() map[string]bool {
	out := map[string]bool{"codex_backend": true, "xai_responses": true}
	for _, p := range Profiles() {
		if kind := p.ResponsesIssuerKind(); kind != "" && !p.ResponsesTrustsCompletedStatus() {
			out[kind] = true
		}
	}
	return out
}

// Normalized assistant turn of one Responses object
type codexAssistant struct {
	Content        string
	ToolCalls      []codexToolCall
	Reasoning      string
	ReasoningItems []map[string]any
	MessageItems   []map[string]any
}

// Assistant message and finish reason of a Responses API object with phase and leak gates applied
func normalizeCodexResponse(resp map[string]any, issuerKind, issuerModel string, recoverLeakedToolCall bool) (*codexAssistant, string, error) {
	status := lowerOrEmpty(resp["status"])
	incompleteReason := ""
	if details, ok := resp["incomplete_details"].(map[string]any); ok {
		incompleteReason = strings.ToLower(strings.TrimSpace(strOrEmpty(details["reason"])))
	}
	contentFilter := status == "incomplete" && incompleteReason == "content_filter"
	output, _ := resp["output"].([]any)
	if len(output) == 0 {
		outText := ""
		if s, ok := resp["output_text"].(string); ok {
			outText = strings.TrimSpace(s)
		}
		var content []any
		switch {
		case outText != "":
			codexLog.Debug(fmt.Sprintf("Codex response has empty output but output_text is present (%d chars); synthesizing output item.", len(outText)))
			content = []any{map[string]any{"type": "output_text", "text": outText}}
		case contentFilter:
			content = []any{}
		default:
			return nil, "", errors.New("Responses API returned no output items")
		}
		output = []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": content}}
		resp["output"] = output
	}
	if status == "failed" || status == "cancelled" {
		return nil, "", errors.New(formatResponsesError(resp["error"], status))
	}
	scan := newCodexOutputScan(status)
	scan.scan(output, issuerKind, issuerModel)
	toolCalls, reasoningParts := scan.toolCalls, scan.reasoningParts
	finalText := strings.TrimSpace(strings.Join(scan.contentParts, "\n"))
	if finalText == "" && (scan.sawFinalAnswerPhase || !scan.sawCommentaryPhase) {
		if s, ok := resp["output_text"].(string); ok {
			finalText = strings.TrimSpace(s)
		}
	}
	leaked := recoverLeakedToolCall && finalText != "" && len(toolCalls) == 0 && leakedToolCallText(finalText)
	if leaked {
		snippet := finalText
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		codexLog.Warn("Codex response contains leaked tool-call text in assistant content (no structured function_call items). Treating as incomplete so the continuation path can re-elicit a proper tool call. Leaked snippet: " + pyRepr(snippet))
		finalText = ""
	}
	if issuerKind == "xai_responses" && finalText == "" && len(toolCalls) == 0 && len(reasoningParts) > 0 {
		joined := strings.Join(reasoningParts, "\n\n")
		marker := strings.LastIndex(joined, "<response>")
		salvaged := ""
		if marker != -1 {
			tail := joined[marker+len("<response>"):]
			tail, _, _ = strings.Cut(tail, "</response>")
			salvaged = strings.TrimSpace(tail)
		}
		if salvaged != "" {
			codexLog.Warn(fmt.Sprintf("xAI response delivered its final answer inside the reasoning channel (<response> delimiter); promoting %d chars to assistant content.", len(salvaged)))
			finalText = salvaged
			prefix := strings.TrimSpace(joined[:marker])
			reasoningParts = nil
			if prefix != "" {
				reasoningParts = []string{prefix}
			}
		}
	}
	msg := &codexAssistant{Content: finalText, ToolCalls: toolCalls, ReasoningItems: scan.reasoningItemsRaw}
	if len(reasoningParts) > 0 {
		msg.Reasoning = strings.TrimSpace(strings.Join(reasoningParts, "\n\n"))
	}
	if !leaked {
		msg.MessageItems = scan.messageItemsRaw
	}
	reasoningOnly := (len(scan.reasoningItemsRaw) > 0 || len(reasoningParts) > 0 || scan.sawReasoningItem) && finalText == ""
	trustedFinal := status == "completed" && !untrustedCompletedIssuers()[issuerKind]
	var finish string
	switch {
	case len(toolCalls) > 0:
		finish = "tool_calls"
	case contentFilter:
		finish = "content_filter"
	case leaked || scan.sawStreamingOrItemIncomplete ||
		((scan.hasIncompleteItems || scan.sawCommentaryPhase) && !scan.sawFinalAnswerPhase) ||
		(reasoningOnly && !trustedFinal):
		finish = "incomplete"
	default:
		finish = "stop"
	}
	return msg, finish, nil
}

// Reports whether a Responses object carries output or a terminal content filter refusal
func validateCodexResponse(resp map[string]any) bool {
	if resp == nil {
		return false
	}
	if output, ok := resp["output"].([]any); ok && len(output) > 0 {
		return true
	}
	status := strings.ToLower(strings.TrimSpace(strOrEmpty(resp["status"])))
	reason := ""
	if details, ok := resp["incomplete_details"].(map[string]any); ok {
		reason = strings.ToLower(strings.TrimSpace(strOrEmpty(details["reason"])))
	}
	return status == "incomplete" && reason == "content_filter"
}

// ---- native compaction ----

// Native compaction fires this far below the local trigger so the server gets the first shot
const localTriggerSafetyMargin = 8192

// Fallback compact threshold when automatic mode has no local trigger to follow
const defaultCompactThreshold = 200000

const nativeCompactionEligibleMarker = "gpt-5.6"

// Retention budgets for user messages and local summaries carried across a compaction boundary
const (
	retainedUserMessageTokenBudget = 64000
	retainedSummaryTokenBudget     = 32000
)

// Reports whether the model is native compaction eligible, Astra only on official Codex OAuth
func isNativeCompactionModel(model, provider, baseURL string) bool {
	if strings.Contains(strings.ToLower(model), nativeCompactionEligibleMarker) {
		return true
	}
	return isAstraModel(model) && strings.ToLower(strings.TrimSpace(provider)) == "openai-codex" && isOfficialCodexBaseURL(baseURL)
}

// Resolved native compaction capability of a runtime destination
func resolveNativeCompactionCapability(model, baseURL, provider string, isCodexBackend bool) bool {
	directDefault := strings.ToLower(strings.TrimSpace(provider)) == "openai" && baseURL == ""
	return isNativeCompactionModel(model, provider, baseURL) && (directDefault || isDirectOpenAIRoute(baseURL, isCodexBackend))
}

// True for api.openai.com or the ChatGPT Codex backend only
func isDirectOpenAIRoute(baseURL string, isCodexBackend bool) bool {
	if isCodexBackend {
		return true
	}
	return strings.ToLower(baseURLHostname(baseURL)) == "api.openai.com"
}

// Positive integer of a configured value, zero for booleans and everything else
func positiveIntValue(v any, rejectFloat bool) int {
	switch t := v.(type) {
	case bool, nil:
		return 0
	case int:
		return maxInt(t, 0)
	case int64:
		return maxInt(int(t), 0)
	case float64:
		if rejectFloat {
			return 0
		}
		return maxInt(int(t), 0)
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil && strings.TrimSpace(t) == fmt.Sprintf("%d", n) {
			return maxInt(n, 0)
		}
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Automatic compact threshold under the local trigger or the clamped explicit one
func resolveCompactThreshold(configured any, localTrigger any) int {
	local := positiveIntValue(localTrigger, false)
	upper := 0
	hasUpper := false
	if local > 0 {
		hasUpper = true
		if local > localTriggerSafetyMargin {
			upper = maxInt(1024, local-localTriggerSafetyMargin)
		} else {
			upper = maxInt(1024, int(float64(local)*0.8))
		}
	}
	explicit := positiveIntValue(configured, true)
	if explicit == 0 {
		if hasUpper {
			return upper
		}
		return defaultCompactThreshold
	}
	if !hasUpper {
		return explicit
	}
	return maxInt(1024, minInt(explicit, upper))
}

var checkpointSuppressionOnce sync.Once

// Inputs of the per request native compaction gate
type nativeCompactionInputs struct {
	runtimeCapability  bool
	configured         bool
	compressionEnabled bool
	checkpointRequired bool
	model              string
	provider           string
	baseURL            string
	trustedProxy       bool
	localTriggerTokens any
	compactThreshold   any
	isCodexBackend     bool
	isXAI              bool
	endpoint           *responsesEndpointQuirks
}

// The context_management payload for this request or nil when it must not be sent
func nativeCompactionContextManagement(in nativeCompactionInputs) []map[string]any {
	if !in.runtimeCapability || !in.configured || !in.compressionEnabled {
		return nil
	}
	if in.checkpointRequired {
		checkpointSuppressionOnce.Do(func() {
			codexLog.Warn("compression.checkpoint_required is enabled: server-side native compaction (context_management) is disabled for this agent so the checkpoint-aware Icebath compressor stays authoritative.")
		})
		return nil
	}
	if in.isXAI || (in.endpoint != nil && !in.endpoint.nativeCompaction) || !isNativeCompactionModel(in.model, in.provider, in.baseURL) {
		return nil
	}
	if !in.trustedProxy && !isDirectOpenAIRoute(in.baseURL, in.isCodexBackend) {
		return nil
	}
	return []map[string]any{{"type": "compaction", "compact_threshold": resolveCompactThreshold(in.compactThreshold, in.localTriggerTokens)}}
}

// Retention cost of one carried over text, never zero
func approxTokens(text string) int {
	return maxInt(1, EstimateTokens(text))
}

// Longest prefix of text whose token estimate fits the budget
func headWithinBudget(text string, budget int) string {
	runes := []rune(text)
	limit := maxInt(0, budget) * CharsPerToken
	if limit > len(runes) {
		limit = len(runes)
	}
	head := string(runes[:limit])
	ascii := true
	for _, r := range head {
		if r > 0x7f {
			ascii = false
			break
		}
	}
	if ascii {
		return head
	}
	lo, hi := 0, len([]rune(head))
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if approxTokens(string(runes[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}

// Measurable text of a Responses item, false when it has none
func extractItemText(item map[string]any) (string, bool) {
	content, has := item["content"]
	if (!has || content == nil) && item["output_text"] != nil {
		content = item["output_text"]
	}
	switch c := content.(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return "", false
		}
		return c, true
	case []any:
		var parts []string
		for _, p := range c {
			var candidates []any
			if part, ok := p.(map[string]any); ok {
				var first any
				for _, k := range []string{"text", "input_text", "output_text"} {
					if pyTruthy(part[k]) {
						first = part[k]
						break
					}
				}
				var meta any
				if pm, ok := part["metadata"].(map[string]any); ok {
					meta = pm["text"]
				}
				candidates = []any{first, meta}
			} else {
				candidates = []any{p}
			}
			for _, cand := range candidates {
				if s, ok := cand.(string); ok && strings.TrimSpace(s) != "" {
					parts = append(parts, strings.TrimSpace(s))
				}
			}
		}
		text := strings.Join(parts, " ")
		if strings.TrimSpace(text) == "" {
			return "", false
		}
		return text, true
	case []map[string]any:
		list := make([]any, len(c))
		for i, m := range c {
			list[i] = m
		}
		return extractItemText(map[string]any{"content": list})
	}
	return "", false
}

// Typed input_text parts of adapter owned content as a list, nil for any other shape
func inputTextParts(content any) []map[string]any {
	var list []map[string]any
	switch c := content.(type) {
	case []map[string]any:
		list = c
	case []any:
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				return nil
			}
			list = append(list, m)
		}
	default:
		return nil
	}
	if len(list) == 0 {
		return nil
	}
	for _, part := range list {
		if part["type"] != "input_text" {
			return nil
		}
		if _, ok := part["text"].(string); !ok {
			return nil
		}
	}
	return list
}

// Token cost of adapter owned input_text parts, false for other shapes
func inputTextPartsCost(content any) (int, bool) {
	parts := inputTextParts(content)
	if parts == nil {
		return 0, false
	}
	total := 0
	for _, part := range parts {
		total += approxTokens(part["text"].(string))
	}
	return total, true
}

// Head of validated input_text parts within the budget with part metadata kept
func truncateInputTextParts(parts []map[string]any, budget int) []map[string]any {
	var head []map[string]any
	for _, part := range parts {
		text := part["text"].(string)
		cost := approxTokens(text)
		if cost > budget {
			if kept := headWithinBudget(text, budget); kept != "" {
				cp := make(map[string]any, len(part))
				for k, v := range part {
					cp[k] = v
				}
				cp["text"] = kept
				head = append(head, cp)
			}
			break
		}
		head = append(head, part)
		budget -= cost
	}
	return head
}

// Reports whether a converted message carries a valid input_image part
func hasRetainableImageContent(item map[string]any) bool {
	var list []any
	switch c := item["content"].(type) {
	case []any:
		list = c
	case []map[string]any:
		for _, m := range c {
			list = append(list, m)
		}
	default:
		return false
	}
	for _, p := range list {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if lowerOrEmpty(part["type"]) == "input_image" {
			if u, ok := part["image_url"].(string); ok && strings.TrimSpace(u) != "" {
				return true
			}
		}
	}
	return false
}

// Reports whether a chat message is a context compaction handoff summary by flag or content
func isCompactionSummaryMessage(m *state.Message) bool {
	if m == nil {
		return false
	}
	return m.CompressedSummary || isCompactionSummaryContent(m.Content)
}

// Reports whether a Responses item is a compaction summary by flag or content
func isCompactionSummaryItem(item map[string]any) bool {
	if pyTruthy(item["_compressed_summary"]) {
		return true
	}
	return isCompactionSummaryContent(item["content"])
}

func isCompactionItem(item map[string]any) bool {
	return item != nil && item["type"] == "compaction"
}

// Restructures Responses input around the newest compaction checkpoint with retained user and summary messages
func prunePreCheckpointItems(items []map[string]any, userBudget, summaryBudget int, summaryRetention bool, sources []*codexMsg) []map[string]any {
	if len(items) == 0 {
		return items
	}
	lastCP := -1
	for i, item := range items {
		if isCompactionItem(item) {
			lastCP = i
		}
	}
	if lastCP < 0 {
		return items
	}
	firstCP := lastCP
	for firstCP > 0 && isCompactionItem(items[firstCP-1]) {
		firstCP--
	}
	pre := items[:firstCP]
	hasSources := sources != nil && len(sources) == len(items)
	var retainedReversed []map[string]any
	userRemaining := maxInt(0, userBudget)
	summaryRemaining := maxInt(0, summaryBudget)
	seenSummaries := map[string]bool{}
	retainSummary := func(text string, ok bool, retained map[string]any) {
		if !ok || text == "" || summaryRemaining <= 0 || seenSummaries[text] {
			return
		}
		cost := approxTokens(text)
		if cost <= summaryRemaining {
			seenSummaries[text] = true
			retainedReversed = append(retainedReversed, retained)
			summaryRemaining -= cost
		}
	}
	for i := len(pre) - 1; i >= 0; i-- {
		item := pre[i]
		if item == nil {
			continue
		}
		var source *codexMsg
		if hasSources {
			source = sources[i]
		}
		if summaryRetention && source != nil && isCompactionSummaryMessage(source.m) {
			text := flattenMessageText(source.m.Content, "\n")
			role := source.m.Role
			if role != "user" && role != "assistant" {
				role = "assistant"
			}
			retainSummary(text, strings.TrimSpace(text) != "", map[string]any{"type": "message", "role": role, "content": text})
			continue
		}
		if t, has := item["type"]; has && t != "message" {
			continue
		}
		isSummary := summaryRetention && isCompactionSummaryItem(item)
		isUser := item["role"] == "user"
		if !isUser && !isSummary {
			continue
		}
		text, ok := extractItemText(item)
		if !ok {
			if !(isUser && hasRetainableImageContent(item)) {
				continue
			}
			text = ""
		}
		if isSummary {
			retainSummary(text, true, item)
		} else if userRemaining > 0 {
			content := item["content"]
			partsCost, isParts := inputTextPartsCost(content)
			cost := partsCost
			if !isParts {
				cost = approxTokens(text)
			}
			if cost <= userRemaining {
				retainedReversed = append(retainedReversed, item)
				userRemaining -= cost
			} else {
				var head any
				keep := false
				if s, isStr := content.(string); isStr {
					h := headWithinBudget(s, userRemaining)
					head, keep = h, strings.TrimSpace(h) != ""
				} else if isParts {
					h := truncateInputTextParts(inputTextParts(content), userRemaining)
					for _, part := range h {
						if strings.TrimSpace(part["text"].(string)) != "" {
							keep = true
						}
					}
					head = h
				}
				if keep {
					cp := make(map[string]any, len(item))
					for k, v := range item {
						cp[k] = v
					}
					cp["content"] = head
					retainedReversed = append(retainedReversed, cp)
				}
				userRemaining = 0
			}
		}
	}
	result := make([]map[string]any, 0, len(items))
	result = append(result, items[firstCP:lastCP+1]...)
	for i := len(retainedReversed) - 1; i >= 0; i-- {
		result = append(result, retainedReversed[i])
	}
	result = append(result, items[lastCP+1:]...)
	codexLog.Debug(fmt.Sprintf("Pruned pre-checkpoint items: %d input -> %d retained (user_rem=%d, summary_rem=%d)", len(items), len(result), userRemaining, summaryRemaining))
	return result
}

var nativeCompactionRejectionMarkers = []string{
	"unknown", "unsupported", "invalid", "unexpected", "not permitted", "not allowed", "unrecognized", "extra field",
	"no such", "bad request", "not supported",
}

// IsNativeCompactionRejection reports a structured 400 rejection of context_management
func IsNativeCompactionRejection(errText string, statusCode int) bool {
	text := strings.ToLower(errText)
	if !strings.Contains(text, "context_management") && !strings.Contains(text, "compact_threshold") {
		return false
	}
	if statusCode != 0 && statusCode != 400 {
		return false
	}
	for _, m := range nativeCompactionRejectionMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// HasCompactionCheckpoint reports whether a reasoning sidecar carries a compaction checkpoint
func HasCompactionCheckpoint(items []map[string]any) bool {
	for _, item := range items {
		if isCompactionItem(item) {
			if enc, ok := item["encrypted_content"].(string); ok && strings.TrimSpace(enc) != "" {
				return true
			}
		}
	}
	return false
}

// MergeInterimReasoningItems keeps prior checkpoints when a continuation carries none of its own
func MergeInterimReasoningItems(prior, newer []map[string]any) []map[string]any {
	var kept []map[string]any
	for _, item := range prior {
		if isCompactionItem(item) {
			kept = append(kept, item)
		}
	}
	out := append([]map[string]any{}, newer...)
	if HasCompactionCheckpoint(out) || len(kept) == 0 {
		return out
	}
	return append(kept, out...)
}

// Walks every nested object of a schema value
func codexSchemaNodes(v any, fn func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, child := range t {
			codexSchemaNodes(child, fn)
		}
	case []any:
		for _, child := range t {
			codexSchemaNodes(child, fn)
		}
	case []map[string]any:
		for _, child := range t {
			codexSchemaNodes(child, fn)
		}
	}
}

// Copies of the tools with enum keywords holding a slash value removed from their parameters
func codexStripSlashEnum(toolList []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(toolList))
	stripped := 0
	for _, tool := range toolList {
		cp := make(map[string]any, len(tool))
		for k, v := range tool {
			cp[k] = v
		}
		if params, ok := cp["parameters"].(map[string]any); ok {
			var clone any
			raw, _ := json.Marshal(params)
			_ = json.Unmarshal(raw, &clone)
			codexSchemaNodes(clone, func(node map[string]any) {
				enum, ok := node["enum"].([]any)
				if !ok {
					return
				}
				for _, v := range enum {
					if s, ok := v.(string); ok && strings.Contains(s, "/") {
						delete(node, "enum")
						stripped++
						return
					}
				}
			})
			cp["parameters"] = clone
		}
		out = append(out, cp)
	}
	if stripped > 0 {
		codexLog.Info("schema_sanitizer: stripped enum keyword(s) containing '/' from tool schemas (xAI Responses grammar-compile recovery)", "count", stripped)
	}
	return out
}
