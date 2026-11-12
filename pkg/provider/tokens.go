package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"maps"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Rough chars per token ratio for ASCII text
const CharsPerToken = tools.ModelCharsPerToken

// Flat per image token price before a route calibrates it from usage
const DefaultImageTokenCost = 1500

var cjkDenseRe = regexp.MustCompile("[ᄀ-ᇿ⺀-鿿ꥠ-꥿가-힯豈-﫿＀-￯]")

// Rough token estimate, CJK codepoints one token each and the rest ceil of UTF8 bytes over four
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii := true
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return (len(text) + 3) / CharsPerToken
	}
	dense := 0
	stripped := cjkDenseRe.ReplaceAllStringFunc(text, func(string) string {
		dense++
		return ""
	})
	return dense + (len(stripped)+3)/CharsPerToken
}

var imagePartTypes = map[string]bool{"image": true, "image_url": true, "input_image": true}

// Persistence only message fields of hermes plus the token_count column a stored message row carries
var persistenceOnlyFields = func() map[string]bool {
	fields := maps.Clone(state.PersistenceOnlyMessageFields)
	fields["token_count"] = true
	return fields
}()

// Replay carriers priced at zero, opaque signatures, duplicate reasoning copies and ordered sidecars
var unpricedCoreFields = []string{"_anthropic_content_blocks", "reasoning_details", "extra_content", "bedrock_content_blocks", "data_use"}

// Core unpriced carriers plus every message carrier key a registered profile declares
func unpricedReplayFields() map[string]bool {
	out := make(map[string]bool, len(unpricedCoreFields))
	for _, k := range unpricedCoreFields {
		out[k] = true
	}
	for _, p := range ListProviders(context.Background()) {
		if p.MessageCarrierOpaque != "" {
			out[p.MessageCarrierOpaque] = true
		}
		if p.MessageCarrierText != "" {
			out[p.MessageCarrierText] = true
		}
	}
	return out
}

// Message as the Python dict row the estimator reads, numbers as ints or floats and the live ordered sidecar included
func messageRow(m *state.Message) map[string]any {
	raw, _ := json.Marshal(m)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	_ = dec.Decode(&decoded)
	row, _ := pyNumbers(decoded).(map[string]any)
	if row == nil {
		row = map[string]any{}
	}
	if blocks, ok := anthropicLiveBlocksOf(m); ok {
		row["anthropic_content_blocks"] = blocks
	}
	return row
}

// Wire shadow of a message with the count of image parts its row carries
func messageShadow(m *state.Message) (map[string]any, int) {
	row := messageRow(m)
	return mapShadow(row), countImageParts(row)
}

// Shadow of a message row holding only what the provider receives, image payloads swapped for a fixed marker
func mapShadow(row map[string]any) map[string]any {
	sidecar, _ := row["api_content"].(string)
	role, _ := row["role"].(string)
	sidecarWins := sidecar != "" && (role == "user" || role == "assistant")
	rc, _ := row["reasoning_content"].(string)
	dropReasoningDup := pystr.Strip(rc) != ""
	unpriced := unpricedReplayFields()
	shadow := map[string]any{}
	for k, v := range row {
		if unpriced[k] || persistenceOnlyFields[k] || (k == "reasoning" && dropReasoningDup) {
			continue
		}
		switch {
		case k == "api_content":
			if sidecarWins {
				shadow["content"] = v
			}
		case k == "content" && sidecarWins:
			continue
		case (k == "content" || k == "output") && isList(v):
			parts := v.([]any)
			out := make([]any, 0, len(parts))
			for _, p := range parts {
				pm, ok := p.(map[string]any)
				if ok && imagePartTypes[stringOf(pm["type"])] {
					out = append(out, map[string]any{"type": pm["type"], "image": "[stripped]"})
					continue
				}
				out = append(out, p)
			}
			shadow[k] = out
		case k == "content" && isUnconvertedMultimodal(v):
			summary, has := v.(map[string]any)["text_summary"]
			if !has {
				summary = ""
			}
			shadow[k] = summary
		case k == "codex_reasoning_items":
			shadow[k] = stripOpaqueReplayItems(v)
		case k == "encrypted_content":
			shadow[k] = ""
		default:
			shadow[k] = v
		}
	}
	return shadow
}

// Whether content is a multimodal tool result not yet converted to wire parts
func isUnconvertedMultimodal(content any) bool {
	cm, ok := content.(map[string]any)
	return ok && pyTruthy(cm["_multimodal"])
}

// Image parts a message row carries in content, stashed Anthropic blocks, unconverted multimodal results and output
func countImageParts(msg any) int {
	row, ok := msg.(map[string]any)
	if !ok {
		return 0
	}
	content := row["content"]
	count := countPartsOfType(content, imagePartTypes)
	count += countPartsOfType(row["_anthropic_content_blocks"], map[string]bool{"image": true})
	if isUnconvertedMultimodal(content) {
		count += countPartsOfType(content.(map[string]any)["content"], map[string]bool{"image": true, "image_url": true})
	}
	return count + countPartsOfType(row["output"], imagePartTypes)
}

// Dict parts of a list whose type is one of types
func countPartsOfType(parts any, types map[string]bool) int {
	list, ok := parts.([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, p := range list {
		if pm, isDict := p.(map[string]any); isDict && types[stringOf(pm["type"])] {
			n++
		}
	}
	return n
}

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// Replay items with their ciphertext blanked so bytes never price what the provider counts itself
func stripOpaqueReplayItems(items any) any {
	list, ok := items.([]any)
	if !ok {
		return items
	}
	out := make([]any, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		c := make(map[string]any, len(m))
		for k, v := range m {
			if k == "encrypted_content" {
				c[k] = ""
			} else {
				c[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

// Opaque replay items with encrypted payloads removed, the shape the budget estimator charges
func StripOpaqueReplayItems(items any) any { return stripOpaqueReplayItems(items) }

// Text tokens of one message with image payloads stripped, the Python str of a dict row shadow or of the value
func estimateMessageTokensWithoutImages(msg any) int {
	if row, ok := msg.(map[string]any); ok {
		return EstimateTokens(pyrepr.Str(mapShadow(row)))
	}
	return EstimateTokens(pyrepr.Str(msg))
}

// Memoized tokens of message rows, each a dict row or any other value, images at the per image price
func estimateRowsTokens[R any](rows []R, imageCost int) int {
	total := 0
	for _, row := range rows {
		total += estimateMessageTokensCached(any(row), imageCost)
	}
	return total
}

// Memoized tokens of dict message rows with every priced key of each dict counted
func EstimateRowsTokens(rows []map[string]any, imageCost int) int {
	return estimateRowsTokens(rows, imageCost)
}

// Decoded JSON with numbers as Python ints or floats so their str matches the dict row
func pyNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		text := t.String()
		if !strings.ContainsAny(text, ".eE") {
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return n
			}
			if n, ok := new(big.Int).SetString(text, 10); ok {
				return n
			}
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, item := range t {
			t[k] = pyNumbers(item)
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = pyNumbers(item)
		}
		return t
	}
	return v
}

// Native Anthropic projection of the messages as dict rows with the replayed readable thinking texts
func nativeAnthropicProjectionRows(messages []*state.Message) ([]any, []string) {
	rows := make([]any, len(messages))
	for i, m := range messages {
		rows[i] = messageRow(m)
	}
	projected, replayed := NativeAnthropicAccountingProjection(rows)
	return projected.([]any), replayed
}

// EstimateNativeAnthropicMessagesTokens charges replayed readable thinking once over the native Anthropic projection
func EstimateNativeAnthropicMessagesTokens(ctx context.Context, messages []*state.Message) int {
	projected, replayed := nativeAnthropicProjectionRows(messages)
	total := estimateRowsTokens(projected, CurrentImageTokenCost(ctx))
	for _, text := range replayed {
		total += EstimateTokens(text)
	}
	return total
}

// EstimateNativeAnthropicRequestTokens is the request estimate over the native projection, opaque replay never counted as text
func EstimateNativeAnthropicRequestTokens(ctx context.Context, messages []*state.Message, system string, tools []map[string]any) int {
	projected, replayed := nativeAnthropicProjectionRows(messages)
	total := 0
	if system != "" {
		total = EstimateTokens(system)
	}
	total += estimateRowsTokens(projected, CurrentImageTokenCost(ctx))
	if len(tools) > 0 {
		total += EstimateToolsTokens(tools)
	}
	for _, text := range replayed {
		total += EstimateTokens(text)
	}
	return total
}

// Messages with stale reasoning text removed from every assistant row but the newest
func stripStaleThinking(msgs []*state.Message) []*state.Message {
	newest := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			newest = i
			break
		}
	}
	out := make([]*state.Message, len(msgs))
	for i, m := range msgs {
		if i != newest && m.Role == "assistant" && (m.Reasoning != "" || m.ReasoningContent != "") {
			c := *m
			c.Reasoning, c.ReasoningContent = "", ""
			out[i] = &c
			continue
		}
		out[i] = m
	}
	return out
}

// Memoized rough token estimate of a message list for preflight decisions
func EstimateMessagesTokens(msgs []*state.Message, imageCost int, chargeStaleThinking bool) int {
	if !chargeStaleThinking {
		msgs = stripStaleThinking(msgs)
	}
	total := 0
	for _, m := range msgs {
		total += estimateMessageTokensCached(messageRow(m), imageCost)
	}
	return total
}

// Rough token estimate of tool schemas from their names, descriptions and parameters
// Tool schema estimates keyed by the identity of the tools list, bounded with oldest first eviction
type toolsTokensEntry struct {
	count       int
	first, last string
	tokens      int
}

const toolsTokensCacheMax = 256

var (
	toolsTokensMu    sync.Mutex
	toolsTokensCache = map[uintptr]toolsTokensEntry{}
	toolsTokensOrder []uintptr
)

// Name of a tool schema for the cache signature, empty when it has none
func toolNameForCache(tool map[string]any) string {
	if tool == nil {
		return ""
	}
	if fn, ok := pyjson.Dict(tool["function"]); ok {
		if name, isStr := fn["name"].(string); isStr {
			return name
		}
	}
	name, _ := tool["name"].(string)
	return name
}

// Compact JSON of a schema the way json.dumps renders it with ensure_ascii off
func compactSchemaJSON(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// Rough token estimate of tool schemas, names, descriptions and parameters JSON, memoised per tools list
func EstimateToolsTokens(tools []map[string]any) int {
	if len(tools) == 0 {
		return 0
	}
	key := reflect.ValueOf(tools).Pointer()
	signature := toolsTokensEntry{count: len(tools), first: toolNameForCache(tools[0]), last: toolNameForCache(tools[len(tools)-1])}
	toolsTokensMu.Lock()
	cached, ok := toolsTokensCache[key]
	toolsTokensMu.Unlock()
	if ok && cached.count == signature.count && cached.first == signature.first && cached.last == signature.last {
		return cached.tokens
	}
	chars := 0
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		src := tool
		if fn, ok := pyjson.Dict(tool["function"]); ok {
			src = fn
		}
		for _, field := range []string{"name", "description"} {
			if s, isStr := src[field].(string); isStr {
				chars += utf8.RuneCountInString(s)
			}
		}
		params := src["parameters"]
		if !pyTruthy(params) {
			params = map[string]any{}
		}
		if text, err := compactSchemaJSON(params); err == nil {
			chars += utf8.RuneCountInString(text)
		} else {
			chars += utf8.RuneCountInString(stringOf(params))
		}
	}
	signature.tokens = (chars + 3) / 4
	toolsTokensMu.Lock()
	defer toolsTokensMu.Unlock()
	if _, present := toolsTokensCache[key]; !present {
		if len(toolsTokensCache) >= toolsTokensCacheMax {
			oldest := toolsTokensOrder[0]
			toolsTokensOrder = toolsTokensOrder[1:]
			delete(toolsTokensCache, oldest)
		}
		toolsTokensOrder = append(toolsTokensOrder, key)
	}
	toolsTokensCache[key] = signature
	return signature.tokens
}

// Rough token estimate of a whole request, system prompt plus messages plus tool schemas
func EstimateRequestTokens(ctx context.Context, msgs []*state.Message, system string, tools []map[string]any, chargeStaleThinking bool) int {
	total := 0
	if system != "" {
		total = EstimateTokens(system)
	}
	if len(msgs) > 0 {
		total += EstimateMessagesTokens(msgs, CurrentImageTokenCost(ctx), chargeStaleThinking)
	}
	if len(tools) > 0 {
		total += EstimateToolsTokens(tools)
	}
	return total
}

// Char equivalent of a wire value as len of its Python str with image parts priced at the learned per image cost
func payloadChars(v any, imageCost int) int {
	switch c := v.(type) {
	case nil:
		return 0
	case map[string]any:
		if t, ok := c["type"].(string); ok && imagePartTypes[t] {
			for _, k := range []string{"image_url", "image", "source", "file_id"} {
				if _, has := c[k]; has {
					text, _ := c["text"].(string)
					return imageCost*4 + pystr.Len(text)
				}
			}
		}
		sum := 0
		for k, val := range c {
			sum += pystr.Len(k) + 6 + payloadChars(val, imageCost)
		}
		return sum
	case []any:
		sum := 2 * len(c)
		for _, it := range c {
			sum += payloadChars(it, imageCost)
		}
		return sum
	case string:
		return pystr.Len(c)
	case json.Number:
		return pystr.Len(string(c))
	case json.RawMessage:
		var decoded any
		dec := json.NewDecoder(bytes.NewReader(c))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil {
			return pystr.Len(string(c))
		}
		return payloadChars(decoded, imageCost)
	}
	if generic, ok := payloadGeneric(v); ok {
		return payloadChars(generic, imageCost)
	}
	return pystr.Len(pyrepr.Str(v))
}

// Typed Go slices and string keyed maps viewed as the list and dict values the wire body holds
func payloadGeneric(v any) (any, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, true
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		if rv.IsNil() {
			return nil, true
		}
		out := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			out[it.Key().String()] = it.Value().Interface()
		}
		return out, true
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, true
		}
		return rv.Elem().Interface(), true
	}
	return nil, false
}

// Cheap char over four context estimate of a Chat, Responses or bare payload for the stale call detectors
func EstimateRequestContextTokens(ctx context.Context, payload any) int {
	imageCost := CurrentImageTokenCost(ctx)
	if generic, ok := payloadGeneric(payload); ok {
		if _, isMap := payload.(map[string]any); !isMap {
			if _, isList := payload.([]any); !isList {
				payload = generic
			}
		}
	}
	if list, ok := payload.([]any); ok {
		total := 0
		for _, item := range list {
			total += payloadChars(item, imageCost)
		}
		return total / 4
	}
	body, ok := payload.(map[string]any)
	if !ok {
		return payloadChars(payload, imageCost) / 4
	}
	messages := body["messages"]
	if generic, ok := payloadGeneric(messages); ok && messages != nil {
		if _, isList := messages.([]any); !isList {
			messages = generic
		}
	}
	if msgs, ok := messages.([]any); ok {
		total := 0
		for _, m := range msgs {
			total += payloadChars(m, imageCost)
		}
		if tools, has := body["tools"]; has {
			total += payloadChars(tools, imageCost)
		}
		return total / 4
	}
	if _, has := body["input"]; has {
		return (payloadChars(body["input"], imageCost) + payloadChars(body["instructions"], imageCost) + payloadChars(body["tools"], imageCost)) / 4
	}
	total := 0
	for _, v := range body {
		total += payloadChars(v, imageCost)
	}
	return total / 4
}
