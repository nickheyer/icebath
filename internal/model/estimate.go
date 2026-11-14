package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const CharsPerToken = 4

var persistenceFields = setOf(
	"timestamp", "display_kind", "display_metadata", "_row_id", "_submit_row_session_id",
	"_merged_turn_prefix", "message_uid", "_absorbed_message_uids", "_tool_call_uids", "_tool_call_uid",
	"_merged_durable_rows", "_unnamed_durable_rows", "_retired_durable_rows",
	"_db_row_snapshot", "_canonical_row", "_restored_from_store",
)

var unpricedCoreFields = []string{"_anthropic_content_blocks", "reasoning_details", "extra_content", "bedrock_content_blocks", "data_use"}

var imagePartTypes = setOf("image", "image_url", "input_image")

var staleThinkingKeys = []string{"reasoning", "reasoning_content"}

var strippedImage = wire.Quote("[stripped]")

type EstimatorOptions struct {
	Providers      *providers.Registry
	ResolveProfile func(arn string) string
}

type Estimator struct {
	providers *providers.Registry
	resolve   func(arn string) string
	memo      tokenMemo
	tools     toolsMemo
}

func NewEstimator(o EstimatorOptions) (*Estimator, error) {
	if o.Providers == nil {
		return nil, errors.New("model: estimator needs the provider registry")
	}
	if o.ResolveProfile == nil {
		return nil, errors.New("model: estimator needs the inference profile resolver")
	}
	return &Estimator{providers: o.Providers, resolve: o.ResolveProfile}, nil
}

func cjkDense(r rune) bool {
	return (r >= 0x1100 && r <= 0x11ff) || (r >= 0x2e80 && r <= 0x9fff) || (r >= 0xa960 && r <= 0xa97f) ||
		(r >= 0xac00 && r <= 0xd7af) || (r >= 0xf900 && r <= 0xfaff) || (r >= 0xff00 && r <= 0xffef)
}

func TextTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii := true
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return (len(text) + CharsPerToken - 1) / CharsPerToken
	}
	dense, rest := 0, 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if cjkDense(r) {
			dense++
		} else {
			rest += size
		}
		i += size
	}
	return dense + (rest+CharsPerToken-1)/CharsPerToken
}

func renderText(raw []byte) string {
	if text, ok := wire.FormatJSON(raw, wire.TextLayout{Spaced: true, UTF8: true}); ok {
		return text
	}
	return string(raw)
}

func JSONTokens(raw json.RawMessage) int {
	return TextTokens(renderText(raw))
}

func memberValue(members []wire.Member, key string) wire.Value {
	for _, m := range members {
		if m.Key == key {
			v, _ := wire.ParseValue(m.Value)
			return v
		}
	}
	return wire.Value{}
}

func (e *Estimator) unpriced(ctx context.Context, carriers map[string]string) map[string]bool {
	out := setOf(unpricedCoreFields...)
	for _, p := range e.providers.All(ctx) {
		spec := p.Spec()
		if spec.CarrierOpaque != "" {
			out[spec.CarrierOpaque] = true
		}
		if spec.CarrierText != "" {
			out[spec.CarrierText] = true
		}
	}
	for key := range carriers {
		out[key] = true
	}
	return out
}

func stripImageParts(v wire.Value) json.RawMessage {
	items := make([]json.RawMessage, 0, len(v.Elems()))
	for _, part := range v.Elems() {
		if t, ok := part.Get("type").Str(); ok && part.IsObject() && imagePartTypes[t] {
			items = append(items, wire.Object([]wire.Member{{Key: "type", Value: wire.Quote(t)}, {Key: "image", Value: strippedImage}}))
			continue
		}
		items = append(items, part.Raw())
	}
	return wire.Array(items...)
}

func StripOpaqueReplayItems(raw json.RawMessage) json.RawMessage {
	v, err := wire.ParseValue(raw)
	if err != nil {
		return raw
	}
	return stripOpaqueItems(v)
}

func stripOpaqueItems(v wire.Value) json.RawMessage {
	if !v.IsArray() {
		return v.Raw()
	}
	items := make([]json.RawMessage, 0, len(v.Elems()))
	for _, item := range v.Elems() {
		if !item.IsObject() {
			items = append(items, item.Raw())
			continue
		}
		var fields []wire.Member
		for _, f := range item.Fields() {
			value := f.Value.Raw()
			if f.Key == "encrypted_content" {
				value = wire.Quote("")
			}
			fields = append(fields, wire.Member{Key: f.Key, Value: value})
		}
		items = append(items, wire.Object(fields))
	}
	return wire.Array(items...)
}

func shadowTokens(members []wire.Member, unpriced map[string]bool) int {
	role := memberValue(members, "role").Text()
	sidecar, sidecarString := memberValue(members, "api_content").Str()
	sidecarWins := sidecarString && sidecar != "" && (role == "user" || role == "assistant")
	rc, rcString := memberValue(members, "reasoning_content").Str()
	dropReasoning := rcString && strings.TrimSpace(rc) != ""
	var shadow []wire.Member
	for _, m := range members {
		key := m.Key
		if unpriced[key] || persistenceFields[key] || (key == "reasoning" && dropReasoning) {
			continue
		}
		v, _ := wire.ParseValue(m.Value)
		switch {
		case key == "api_content":
			if sidecarWins {
				shadow = append(shadow, wire.Member{Key: "content", Value: m.Value})
			}
		case key == "content" && sidecarWins:
		case key == "content" && v.IsArray():
			shadow = append(shadow, wire.Member{Key: key, Value: stripImageParts(v)})
		case key == "content" && v.IsObject() && v.Get("_multimodal").Truthy():
			summary := v.Get("text_summary").Raw()
			if summary == nil {
				summary = wire.Quote("")
			}
			shadow = append(shadow, wire.Member{Key: key, Value: summary})
		case key == "output" && v.IsArray():
			shadow = append(shadow, wire.Member{Key: key, Value: stripImageParts(v)})
		case key == "codex_reasoning_items":
			shadow = append(shadow, wire.Member{Key: key, Value: stripOpaqueItems(v)})
		case key == "encrypted_content":
			shadow = append(shadow, wire.Member{Key: key, Value: wire.Quote("")})
		default:
			shadow = append(shadow, m)
		}
	}
	return TextTokens(renderText(wire.Object(shadow)))
}

func countParts(v wire.Value, types map[string]bool) int {
	n := 0
	for _, part := range v.Elems() {
		if t, ok := part.Get("type").Str(); ok && types[t] {
			n++
		}
	}
	return n
}

var multimodalImageTypes = setOf("image", "image_url")

var anthropicImageTypes = setOf("image")

func countImages(members []wire.Member) int {
	content := memberValue(members, "content")
	n := countParts(content, imagePartTypes)
	n += countParts(memberValue(members, "_anthropic_content_blocks"), anthropicImageTypes)
	if content.IsObject() && content.Get("_multimodal").Truthy() {
		n += countParts(content.Get("content"), multimodalImageTypes)
	}
	return n + countParts(memberValue(members, "output"), imagePartTypes)
}

func CountImages(msgs []wire.Message) int {
	n := 0
	for i := range msgs {
		n += countImages(msgs[i].Members())
	}
	return n
}

func (e *Estimator) memberTokens(members []wire.Member, unpriced map[string]bool, imageCost int) int {
	key := keyOf(members)
	if text, images, ok := e.memo.lookup(key); ok {
		return text + images*imageCost
	}
	text, images := shadowTokens(members, unpriced), countImages(members)
	e.memo.remember(key, text, images)
	return text + images*imageCost
}

func withoutKeys(members []wire.Member, keys []string) []wire.Member {
	out := make([]wire.Member, 0, len(members))
	for _, m := range members {
		if !containsString(keys, m.Key) {
			out = append(out, m)
		}
	}
	return out
}

func staleThinking(members []wire.Member) bool {
	for _, key := range staleThinkingKeys {
		if memberValue(members, key).Truthy() {
			return true
		}
	}
	return false
}

func (e *Estimator) memberLists(msgs []wire.Message, chargeStale bool) ([][]wire.Member, map[string]string) {
	lists := make([][]wire.Member, len(msgs))
	carriers := map[string]string{}
	newest := -1
	for i := range msgs {
		lists[i] = msgs[i].Members()
		for k, v := range msgs[i].Carriers {
			carriers[k] = v
		}
		if msgs[i].Role == wire.RoleAssistant {
			newest = i
		}
	}
	if !chargeStale {
		for i, members := range lists {
			if i != newest && msgs[i].Role == wire.RoleAssistant && staleThinking(members) {
				lists[i] = withoutKeys(members, staleThinkingKeys)
			}
		}
	}
	return lists, carriers
}

func (e *Estimator) listTokens(ctx context.Context, lists [][]wire.Member, carriers map[string]string) int {
	unpriced := e.unpriced(ctx, carriers)
	cost := ImageCost(ctx)
	total := 0
	for _, members := range lists {
		total += e.memberTokens(members, unpriced, cost)
	}
	return total
}

func (e *Estimator) Messages(ctx context.Context, msgs []wire.Message, chargeStale bool) int {
	lists, carriers := e.memberLists(msgs, chargeStale)
	return e.listTokens(ctx, lists, carriers)
}

func (e *Estimator) NativeAnthropicMessages(ctx context.Context, msgs []wire.Message) int {
	lists, thinking, carriers := e.anthropicProjection(msgs)
	total := e.listTokens(ctx, lists, carriers)
	for _, text := range thinking {
		total += TextTokens(text)
	}
	return total
}

func (e *Estimator) Request(ctx context.Context, system string, msgs []wire.Message, tools []wire.Tool, chargeStale bool) int {
	total := TextTokens(system)
	if len(msgs) > 0 {
		total += e.Messages(ctx, msgs, chargeStale)
	}
	return total + e.Tools(tools)
}

func (e *Estimator) NativeAnthropicRequest(ctx context.Context, system string, msgs []wire.Message, tools []wire.Tool) int {
	lists, thinking, carriers := e.anthropicProjection(msgs)
	total := TextTokens(system) + e.Tools(tools)
	if len(lists) > 0 {
		total += e.listTokens(ctx, lists, carriers)
	}
	for _, text := range thinking {
		total += TextTokens(text)
	}
	return total
}

func (e *Estimator) Tools(tools []wire.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	if tokens, ok := e.tools.lookup(tools); ok {
		return tokens
	}
	chars := 0
	for _, t := range tools {
		chars += utf8.RuneCountInString(t.Name) + utf8.RuneCountInString(t.Description)
		params := []byte("{}")
		if v, err := wire.ParseValue(t.Parameters); err == nil && v.Truthy() {
			params = t.Parameters
		}
		text, ok := wire.FormatJSON(params, wire.TextLayout{UTF8: true})
		if !ok {
			text = string(params)
		}
		chars += utf8.RuneCountInString(text)
	}
	tokens := (chars + 3) / 4
	e.tools.remember(tools, tokens)
	return tokens
}

func (e *Estimator) Forget(msgs, keep []wire.Message) int {
	keys := e.variantKeys(msgs)
	for key := range e.variantKeys(keep) {
		delete(keys, key)
	}
	return e.memo.forget(keys)
}

func (e *Estimator) variantKeys(msgs []wire.Message) map[memoKey]bool {
	keys := map[memoKey]bool{}
	for i := range msgs {
		members := msgs[i].Members()
		keys[keyOf(members)] = true
		keys[keyOf(withoutKeys(members, staleThinkingKeys))] = true
	}
	lists, _, _ := e.anthropicProjection(msgs)
	for _, members := range lists {
		keys[keyOf(members)] = true
	}
	return keys
}

func payloadChars(v wire.Value, imageCost int) int {
	switch v.Kind() {
	case wire.KindNone, wire.KindNull:
		return 0
	case wire.KindObject:
		if t, ok := v.Get("type").Str(); ok && imagePartTypes[t] &&
			(v.Get("image_url").Exists() || v.Get("image").Exists() || v.Get("source").Exists() || v.Get("file_id").Exists()) {
			chars := imageCost * 4
			if text, ok := v.Get("text").Str(); ok {
				chars += utf8.RuneCountInString(text)
			}
			return chars
		}
		total := 0
		for _, f := range v.Fields() {
			total += utf8.RuneCountInString(f.Key) + 6 + payloadChars(f.Value, imageCost)
		}
		return total
	case wire.KindArray:
		items := v.Elems()
		total := 2 * len(items)
		for _, item := range items {
			total += payloadChars(item, imageCost)
		}
		return total
	case wire.KindString:
		return utf8.RuneCountInString(v.Text())
	}
	return utf8.RuneCount(v.Raw())
}

func RequestContextTokens(ctx context.Context, body []byte) int {
	payload, err := wire.ParseValue(body)
	if err != nil {
		return 0
	}
	cost := ImageCost(ctx)
	chars := func(v wire.Value) int { return payloadChars(v, cost) }
	switch {
	case payload.IsArray():
		total := 0
		for _, item := range payload.Elems() {
			total += chars(item)
		}
		return total / 4
	case !payload.IsObject():
		return chars(payload) / 4
	}
	if messages := payload.Get("messages"); messages.IsArray() {
		total := 0
		for _, item := range messages.Elems() {
			total += chars(item)
		}
		if payload.Get("tools").Exists() {
			total += chars(payload.Get("tools"))
		}
		return total / 4
	}
	if payload.Get("input").Exists() {
		return (chars(payload.Get("input")) + chars(payload.Get("instructions")) + chars(payload.Get("tools"))) / 4
	}
	total := 0
	for _, f := range payload.Fields() {
		total += chars(f.Value)
	}
	return total / 4
}

func SerializedBytes(msgs []wire.Message) int {
	if len(msgs) == 0 {
		return 0
	}
	raw := wire.AppendMessages(nil, msgs)
	if text, ok := wire.FormatJSON(raw, wire.TextLayout{UTF8: true}); ok {
		return len(text)
	}
	return len(raw)
}
