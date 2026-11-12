package provider

import (
	"context"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"reflect"
	"slices"
	"strings"
	"sync"
)

const carrierGeminiKey = "extra_content"

// Opaque and text carrier keys the registered profiles declare, deduplicated in registration order
func declaredCarrierKeys(ctx context.Context) (opaque, text []string) {
	for _, p := range listProviders(ctx) {
		if k := p.MessageCarrierOpaque; k != "" && !slices.Contains(opaque, k) {
			opaque = append(opaque, k)
		}
		if k := p.MessageCarrierText; k != "" && !slices.Contains(text, k) {
			text = append(text, k)
		}
	}
	return opaque, text
}

// Gemini key first, then every declared opaque and text key once
func carrierMessageKeys(ctx context.Context) []string {
	opaque, text := declaredCarrierKeys(ctx)
	keys := []string{carrierGeminiKey}
	for _, k := range append(opaque, text...) {
		if !slices.Contains(keys[1:], k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// Named field of a response map, else the same key parked under model_extra or provider_data
func carrierField(obj map[string]any, name string) any {
	value := obj[name]
	for _, side := range []string{"model_extra", "provider_data"} {
		if value == nil {
			if extra, ok := obj[side].(map[string]any); ok {
				value = extra[name]
			}
		}
	}
	return value
}

// Gemini thought signature inside an extra_content map, nested under google or flat
func carrierThoughtSignature(extra any) string {
	m, ok := extra.(map[string]any)
	if !ok {
		return ""
	}
	sig := m["thought_signature"]
	if google, ok := m["google"].(map[string]any); ok {
		sig = google["thought_signature"]
	}
	s, _ := sig.(string)
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// Private reasoning_details record of one response message, nil when it carries nothing
func carrierRecord(ctx context.Context, assistantMessage map[string]any) map[string]any {
	record := map[string]any{}
	if sig := carrierThoughtSignature(carrierField(assistantMessage, carrierGeminiKey)); sig != "" {
		record[carrierGeminiKey] = map[string]any{"google": map[string]any{"thought_signature": sig}}
	}
	for _, key := range carrierMessageKeys(ctx)[1:] {
		if v, ok := carrierField(assistantMessage, key).(string); ok && v != "" {
			record[key] = v
		}
	}
	if len(record) == 0 {
		return nil
	}
	record["type"] = nativeCarrierType
	return record
}

// CarrierRecord is the private reasoning_details record of one response message, nil when it carries nothing
func CarrierRecord(ctx context.Context, assistantMessage map[string]any) map[string]any {
	return carrierRecord(ctx, assistantMessage)
}

type streamCarriers struct {
	opaqueKeys, textKeys []string
	signature            string
	opaque               map[string]string
	opaqueOrder          []string
	textParts            map[string][]string
	textOrder            []string
}

// Accumulator of streamed carriers with the declared keys resolved once for the stream
func newStreamCarriers(ctx context.Context) *streamCarriers {
	opaque, text := declaredCarrierKeys(ctx)
	return &streamCarriers{opaqueKeys: opaque, textKeys: text, opaque: map[string]string{}, textParts: map[string][]string{}}
}

// Absorbs one delta and returns the reasoning, else its first declared text carrier fragment
func (s *streamCarriers) feed(delta map[string]any, reasoning *string) *string {
	if sig := carrierThoughtSignature(carrierField(delta, carrierGeminiKey)); sig != "" {
		s.signature = sig
	}
	for _, key := range s.opaqueKeys {
		if v, ok := carrierField(delta, key).(string); ok && v != "" {
			if _, seen := s.opaque[key]; !seen {
				s.opaqueOrder = append(s.opaqueOrder, key)
			}
			s.opaque[key] = v
		}
	}
	for _, key := range s.textKeys {
		if v, ok := carrierField(delta, key).(string); ok && v != "" {
			if _, seen := s.textParts[key]; !seen {
				s.textOrder = append(s.textOrder, key)
			}
			s.textParts[key] = append(s.textParts[key], v)
			if reasoning == nil {
				text := v
				reasoning = &text
			}
		}
	}
	return reasoning
}

// Writes the latest signature, opaque blobs and joined carrier texts onto the first choice message
func (s *streamCarriers) apply(response map[string]any) {
	var message map[string]any
	switch choices := response["choices"].(type) {
	case []any:
		if len(choices) > 0 {
			if c, ok := choices[0].(map[string]any); ok {
				message, _ = c["message"].(map[string]any)
			}
		}
	case []map[string]any:
		if len(choices) > 0 {
			message, _ = choices[0]["message"].(map[string]any)
		}
	}
	if message == nil {
		return
	}
	if s.signature != "" {
		message[carrierGeminiKey] = map[string]any{"google": map[string]any{"thought_signature": s.signature}}
	}
	for _, key := range s.opaqueOrder {
		message[key] = s.opaque[key]
	}
	for _, key := range s.textOrder {
		message[key] = strings.Join(s.textParts[key], "")
	}
}

type carrierWireRoute struct {
	ctx        context.Context
	model      string
	baseURL    string
	gemini     bool
	profile    *Profile
	keepFnSig  bool
	keys       []string
	scope      any
	keySet     map[string]bool
	profileKey string
}

// Wire route facts for carrier shaping, the host profile and its function signature answer
func newCarrierWireRoute(ctx context.Context, model, baseURL string, scope any) carrierWireRoute {
	profile := profileForHostCtx(ctx, baseURL, false)
	keep := false
	if profile != nil {
		keep = hookBool(AskProfile(ctx, profile, "replays_function_thought_signature", false, scope, nil,
			map[string]any{"model": model, "base_url": baseURL}))
	}
	keys := carrierMessageKeys(ctx)
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	r := carrierWireRoute{ctx: ctx, model: model, baseURL: baseURL, gemini: modelConsumesThoughtSignature(model, "", ""),
		profile: profile, keepFnSig: keep, keys: keys, scope: scope, keySet: set}
	if profile != nil {
		r.profileKey = fmt.Sprintf("%p|%s", profile, profile.Name)
	}
	return r
}

type carrierReplayPlan struct {
	rewrites bool
	pairs    [][2]string
}

const carrierReplayPlansMax = 512

var (
	carrierReplayPlansMu sync.Mutex
	carrierReplayPlans   = map[string]*carrierReplayPlan{}
	carrierHookFailed    = &struct{ failed bool }{true}
)

// Output to source key pairs reproducing the answer from values, nil when a value has two sources
func carrierPlanFor(answer any, values map[string]any) *carrierReplayPlan {
	m, ok := answer.(map[string]any)
	if !ok {
		return &carrierReplayPlan{rewrites: true}
	}
	plan := &carrierReplayPlan{}
	for _, outKey := range sortedKeys(m) {
		var sources []string
		for _, k := range sortedKeys(values) {
			if reflect.DeepEqual(values[k], m[outKey]) {
				sources = append(sources, k)
			}
		}
		if len(sources) == 0 {
			return &carrierReplayPlan{rewrites: true}
		}
		if len(sources) > 1 {
			return nil
		}
		plan.pairs = append(plan.pairs, [2]string{outKey, sources[0]})
	}
	return plan
}

// Carriers the host profile replays for the values, one hook answer reused per key set
func carrierReplay(r carrierWireRoute, values map[string]any) map[string]any {
	if HookFailed(r.profile, "replay_message_carriers", r.scope) {
		return map[string]any{}
	}
	key := fmt.Sprintf("%d|%s|%s|%s|%s", RegistryGeneration(), r.profileKey, r.model, r.baseURL, strings.Join(sortedKeys(values), "\x00"))
	carrierReplayPlansMu.Lock()
	plan := carrierReplayPlans[key]
	carrierReplayPlansMu.Unlock()
	if plan != nil && !plan.rewrites {
		out := make(map[string]any, len(plan.pairs))
		for _, p := range plan.pairs {
			out[p[0]] = values[p[1]]
		}
		return out
	}
	answer := AskProfile(r.ctx, r.profile, "replay_message_carriers", carrierHookFailed, r.scope, []any{values},
		map[string]any{"model": r.model, "base_url": r.baseURL})
	if answer == any(carrierHookFailed) {
		return map[string]any{}
	}
	if plan == nil {
		if plan = carrierPlanFor(answer, values); plan != nil {
			carrierReplayPlansMu.Lock()
			if len(carrierReplayPlans) >= carrierReplayPlansMax {
				clear(carrierReplayPlans)
			}
			carrierReplayPlans[key] = plan
			carrierReplayPlansMu.Unlock()
		}
	}
	out := map[string]any{}
	if m, ok := answer.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// Tool calls with function thought_signature removed, untouched calls kept by identity
func carrierStripFunctionSignature(toolCalls []any) []any {
	out := make([]any, len(toolCalls))
	for i, tc := range toolCalls {
		out[i] = tc
		m, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := pyjson.Dict(m["function"])
		if !ok {
			continue
		}
		if _, has := fn["thought_signature"]; !has {
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		stripped := make(map[string]any, len(fn))
		for k, v := range fn {
			if k != "thought_signature" {
				stripped[k] = v
			}
		}
		cp["function"] = stripped
		out[i] = cp
	}
	return out
}

func carrierListOf(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out
	}
	return nil
}

func carrierIsRecord(d any) (map[string]any, bool) {
	m, ok := d.(map[string]any)
	if !ok {
		return nil, false
	}
	t, _ := m["type"].(string)
	return m, t == nativeCarrierType || t == "hermes.native_assistant"
}

// Wire copy of one assistant message with each carrier lifted onto the route that reads it, else the input
func carrierShape(msg map[string]any, r carrierWireRoute) map[string]any {
	if msg == nil || msg["role"] != "assistant" {
		return msg
	}
	details := carrierListOf(msg["reasoning_details"])
	var record map[string]any
	recordAt := -1
	for i, d := range details {
		if m, ok := carrierIsRecord(d); ok {
			record, recordAt = m, i
			break
		}
	}
	toolCalls := carrierListOf(msg["tool_calls"])
	fnSig := false
	if !r.keepFnSig {
		for _, tc := range toolCalls {
			if m, ok := pyjson.Dict(tc); ok {
				if fn, ok := pyjson.Dict(m["function"]); ok {
					if _, has := fn["thought_signature"]; has {
						fnSig = true
						break
					}
				}
			}
		}
	}
	keys := r.keys
	if record != nil {
		keys = slices.Clone(r.keys)
		for _, k := range sortedKeys(record) {
			if k != "type" && !r.keySet[k] {
				keys = append(keys, k)
			}
		}
	}
	present := false
	for _, k := range keys {
		if _, ok := msg[k]; ok {
			present = true
			break
		}
	}
	if record == nil && !fnSig && !present {
		return msg
	}
	strip := make(map[string]bool, len(keys))
	for _, k := range keys {
		strip[k] = true
	}
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		if !strip[k] {
			out[k] = v
		}
	}
	values := map[string]any{}
	for k, v := range record {
		values[k] = v
	}
	for _, k := range keys {
		if v, ok := msg[k]; ok {
			values[k] = v
		}
	}
	if record != nil {
		rest := make([]any, 0, len(details))
		for i, d := range details {
			if i != recordAt {
				rest = append(rest, d)
			}
		}
		if len(rest) > 0 {
			out["reasoning_details"] = rest
		} else {
			delete(out, "reasoning_details")
		}
	}
	if r.gemini && pyTruthy(values[carrierGeminiKey]) {
		out[carrierGeminiKey] = values[carrierGeminiKey]
	}
	if r.profile != nil {
		for k, v := range carrierReplay(r, values) {
			out[k] = v
		}
	}
	if fnSig {
		out["tool_calls"] = carrierStripFunctionSignature(toolCalls)
	}
	return out
}

// Wire copy of messages with each carrier on the route that reads it, the input itself when nothing changed
func shapeWireCarriers(ctx context.Context, messages []map[string]any, model, baseURL string, scope any) []map[string]any {
	r := newCarrierWireRoute(ctx, model, baseURL, scope)
	failedBefore := r.profile == nil || HookFailed(r.profile, "replay_message_carriers", scope)
	shape := func() []map[string]any {
		out := make([]map[string]any, len(messages))
		for i, m := range messages {
			out[i] = carrierShape(m, r)
		}
		return out
	}
	shaped := shape()
	if !failedBefore && HookFailed(r.profile, "replay_message_carriers", scope) {
		shaped = shape()
	}
	for i := range shaped {
		if reflect.ValueOf(shaped[i]).UnsafePointer() != reflect.ValueOf(messages[i]).UnsafePointer() {
			return shaped
		}
	}
	return messages
}

// ShapeWireCarriers is the wire copy of messages with each carrier only on the route that reads it
func ShapeWireCarriers(ctx context.Context, messages []map[string]any, model, baseURL string, scope any) []map[string]any {
	return shapeWireCarriers(ctx, messages, model, baseURL, scope)
}
