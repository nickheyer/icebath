package model

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	rejectedCarriersKey = "reasoning_rejected_carriers"
	imageRemovedText    = "[image content removed — server does not support images]"
	nativeAssistantType = ".native_assistant"
)

var (
	unknownFieldPhrases = []string{
		"extra inputs are not permitted", "is unsupported", "no such field", "unknown field",
		"unrecognized request argument", "unrecognized field", "unknown parameter", "unexpected field",
		"additional propert", "unknown key",
	}
	imageRejectionPhrases = []string{
		"only 'text' content type is supported", "only text content type is supported",
		"image_url is not supported", "image content is not supported",
		"multimodal is not supported", "multimodal content is not supported", "multimodal input is not supported",
		"vision is not supported", "vision input is not supported",
		"does not support images", "does not support image input", "does not support multimodal",
		"does not support vision", "model does not support image",
		"unexpected item type in content",
		"image_url'. expected",
		"unknown variant `image_url`, expected `text`", "unknown variant image_url, expected text",
		"no endpoints found that support image input",
	}
	imageCorruptPhrases = []string{"image data you provided does not represent a valid image", "failed to decode image"}
	imagePartWords      = map[string]bool{"image_url": true, "image": true, "input_image": true}
)

type ReplayRoute struct {
	Pad      bool
	Carriers wire.ReasoningKeys
	Native   bool
}

func anyHost(baseURL string, hosts []string) bool {
	for _, h := range hosts {
		if providers.HostMatches(baseURL, h) {
			return true
		}
	}
	return false
}

func echoRuleMatches(rule wire.EchoRule, provider, model, baseURL string) bool {
	if slices.Contains(rule.Providers, provider) || slices.Contains(rule.LowerProviders, strings.ToLower(provider)) {
		return true
	}
	lower := strings.ToLower(model)
	for _, part := range rule.ModelParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return anyHost(baseURL, rule.Hosts)
}

func EchoFamilyMatches(family, provider, model, baseURL string) bool {
	for _, rule := range wire.EchoRules() {
		if rule.Family == family {
			return echoRuleMatches(rule, provider, model, baseURL)
		}
	}
	return false
}

func ReasoningEchoFamily(provider, model, baseURL string) string {
	for _, rule := range wire.EchoRules() {
		if echoRuleMatches(rule, provider, model, baseURL) {
			return rule.Family
		}
	}
	return ""
}

func NeedsReasoningEcho(provider, model, baseURL string) bool {
	return ReasoningEchoFamily(provider, model, baseURL) != ""
}

func (r *Router) RouteReasoningCarriers(ctx context.Context, provider, baseURL string) wire.ReasoningKeys {
	lower := strings.ToLower(strings.TrimSpace(provider))
	keys, found := wire.ReasoningKeys(0), false
	for _, cr := range wire.CarrierRoutes() {
		if slices.Contains(cr.Providers, lower) || anyHost(baseURL, cr.Hosts) {
			keys, found = cr.Keys, true
			break
		}
	}
	if !found {
		keys = wire.DefaultReasoningKeys
		if baseURL != "" && catalog.IsLocalEndpoint(baseURL) {
			keys = wire.LocalReasoningKeys
		}
	}
	if keys != 0 && keys&wire.KeyReasoningDetails == 0 && r.declaresNativeDetails(ctx, lower) {
		keys |= wire.KeyReasoningDetails
	}
	return keys
}

func (r *Router) declaresNativeDetails(ctx context.Context, provider string) bool {
	if provider == "" {
		return false
	}
	p, ok := r.c.o.Providers.Get(r.bind(ctx), provider)
	return ok && p.Spec().NativeDetailsType != ""
}

func (r *Router) ReasoningReplayRoute(ctx context.Context, mode wire.Mode, provider, model, baseURL string, echoOptIn bool, rejected wire.ReasoningKeys) ReplayRoute {
	pad := echoOptIn || NeedsReasoningEcho(provider, model, baseURL)
	if mode != "" && mode != wire.ModeChat {
		return ReplayRoute{Pad: pad, Native: true}
	}
	carriers := r.RouteReasoningCarriers(ctx, provider, baseURL) &^ rejected
	return ReplayRoute{Pad: pad && carriers&wire.KeyReasoningContent != 0, Carriers: carriers}
}

func (r *Router) StaleThinkingReachesWire(ctx context.Context, mode wire.Mode, provider, model, baseURL string) bool {
	if mode == wire.ModeAnthropic && anthropicPreservesPriorThinking(baseURL, model) {
		return true
	}
	if mode == "" {
		return NeedsReasoningEcho(provider, model, baseURL)
	}
	rt := r.ReasoningReplayRoute(ctx, mode, provider, model, baseURL, false, 0)
	if rt.Native {
		return mode != wire.ModeResponses && rt.Pad
	}
	return rt.Carriers&(wire.KeyReasoningContent|wire.KeyReasoning) != 0
}

func replayableReasoningText(m *wire.Message) (string, bool) {
	if m.ReasoningContent != nil && strings.TrimSpace(*m.ReasoningContent) != "" {
		return *m.ReasoningContent, true
	}
	if strings.TrimSpace(m.Reasoning) != "" {
		return m.Reasoning, true
	}
	return "", false
}

func reasoningShape(m *wire.Message) string {
	var b strings.Builder
	if m.ReasoningContent != nil {
		b.WriteString("1" + *m.ReasoningContent)
	}
	b.WriteString("\x00" + m.Reasoning + "\x00")
	for _, d := range m.ReasoningDetails {
		b.Write(d)
	}
	return b.String()
}

func ApplyReasoningPolicy(m *wire.Message, rt ReplayRoute) bool {
	if m.Role != wire.RoleAssistant {
		return false
	}
	if rt.Native && rt.Pad && m.ReasoningContent != nil && *m.ReasoningContent != "" {
		return false
	}
	before := reasoningShape(m)
	text, has := replayableReasoningText(m)
	effective := rt.Carriers
	if rt.Native {
		effective = 0
		if rt.Pad {
			effective = wire.KeyReasoningContent
		}
	}
	switch {
	case rt.Native && rt.Pad:
		switch {
		case m.ReasoningContent != nil:
			v := *m.ReasoningContent
			if v == "" {
				v = " "
			}
			m.ReasoningContent = &v
		case m.Reasoning != "" && len(m.ToolCalls) == 0:
			v := m.Reasoning
			m.ReasoningContent = &v
		default:
			v := " "
			m.ReasoningContent = &v
		}
	case effective&wire.KeyReasoningContent != 0 && (has || rt.Pad):
		v := " "
		if has {
			v = text
		}
		m.ReasoningContent = &v
	default:
		m.ReasoningContent = nil
	}
	if effective&wire.KeyReasoning != 0 && has {
		m.Reasoning = text
	} else {
		m.Reasoning = ""
	}
	if !rt.Native && rt.Carriers&wire.KeyReasoningDetails == 0 {
		var private []json.RawMessage
		for _, d := range m.ReasoningDetails {
			if strings.HasSuffix(wire.DetailType(d), nativeAssistantType) {
				private = append(private, d)
			}
		}
		m.ReasoningDetails = private
	}
	return reasoningShape(m) != before
}

func wordByte(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func standaloneAt(text []rune, at, n int) bool {
	if at > 0 && wordByte(text[at-1]) {
		return false
	}
	return at+n >= len(text) || !wordByte(text[at+n])
}

func containsStandalone(text []rune, word string) bool {
	w := []rune(word)
	for i := 0; i+len(w) <= len(text); i++ {
		if string(text[i:i+len(w)]) == word && standaloneAt(text, i, len(w)) {
			return true
		}
	}
	return false
}

func messagePathNamesReasoning(text []rune) bool {
	head, word := []rune("messages"), []rune("reasoning")
	for i := 0; i+len(head) <= len(text); i++ {
		if string(text[i:i+len(head)]) != string(head) {
			continue
		}
		start := i + len(head)
		for gap := 0; gap <= 80 && start+gap+len(word) <= len(text); gap++ {
			if gap > 0 && text[start+gap-1] == '\n' {
				break
			}
			at := start + gap
			if string(text[at:at+len(word)]) == string(word) && standaloneAt(text, at, len(word)) {
				return true
			}
		}
	}
	return false
}

func sentReasoningKeys(sent []wire.Message) wire.ReasoningKeys {
	var keys wire.ReasoningKeys
	for i := range sent {
		m := &sent[i]
		if m.Role != wire.RoleAssistant {
			continue
		}
		if m.ReasoningContent != nil {
			keys |= wire.KeyReasoningContent
		}
		if m.Reasoning != "" {
			keys |= wire.KeyReasoning
		}
		if len(m.ReasoningDetails) > 0 {
			keys |= wire.KeyReasoningDetails
		}
	}
	return keys
}

func RejectedReasoningFields(errorBody string, sent []wire.Message) wire.ReasoningKeys {
	body := strings.ToLower(errorBody)
	matched := false
	for _, phrase := range unknownFieldPhrases {
		if strings.Contains(body, phrase) {
			matched = true
			break
		}
	}
	if !matched {
		return 0
	}
	present := sentReasoningKeys(sent)
	text := []rune(body)
	var named wire.ReasoningKeys
	if present&wire.KeyReasoningContent != 0 && containsStandalone(text, "reasoning_content") {
		named |= wire.KeyReasoningContent
	}
	if present&wire.KeyReasoningDetails != 0 && containsStandalone(text, "reasoning_details") {
		named |= wire.KeyReasoningDetails
	}
	if present&wire.KeyReasoning != 0 && messagePathNamesReasoning(text) {
		named |= wire.KeyReasoning
	}
	if named == 0 {
		return 0
	}
	if named&wire.KeyReasoningContent != 0 {
		return present & wire.AllReasoningKeys
	}
	return present & (wire.KeyReasoning | wire.KeyReasoningDetails)
}

type reasoningRouteKey struct {
	provider, host, model string
}

func reasoningKeyOf(provider, baseURL, model string) reasoningRouteKey {
	return reasoningRouteKey{
		provider: strings.ToLower(strings.TrimSpace(provider)),
		host:     providers.URLHostname(baseURL),
		model:    strings.TrimSpace(model),
	}
}

func (s *Session) rejectedCarriersLocked(ctx context.Context) wire.ReasoningKeys {
	sid := s.sessionID
	if sid != "" && s.carriersLoadedFor != sid && s.o.Store != nil {
		s.carriersLoadedFor = sid
		raw, ok, err := s.o.Store.ModelConfigValue(ctx, sid, rejectedCarriersKey)
		if err == nil && ok {
			if v, perr := wire.ParseValue(raw); perr == nil && v.IsArray() {
				for _, entry := range v.Elems() {
					parts := entry.Elems()
					if !entry.IsArray() || len(parts) != 4 || !parts[3].IsArray() {
						continue
					}
					key := reasoningRouteKey{provider: parts[0].Text(), host: parts[1].Text(), model: parts[2].Text()}
					for _, k := range parts[3].Elems() {
						s.rejectedCarriers[key] |= wire.ReasoningKeysOf(k.Text())
					}
				}
			}
		}
	}
	rt := s.route
	return s.rejectedCarriers[reasoningKeyOf(rt.Provider, rt.BaseURL, rt.Model)]
}

func (s *Session) RejectedCarriers(ctx context.Context) wire.ReasoningKeys {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejectedCarriersLocked(s.r.bind(ctx))
}

func (s *Session) RecordReasoningRejection(ctx context.Context, errorBody string, sent []wire.Message) (wire.ReasoningKeys, error) {
	ctx = s.r.bind(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.route
	if rt.Mode != wire.ModeChat {
		return 0, nil
	}
	fields := RejectedReasoningFields(errorBody, sent)
	if NeedsReasoningEcho(rt.Provider, rt.Model, rt.BaseURL) {
		fields &^= wire.KeyReasoningContent
	}
	if fields == 0 {
		return 0, nil
	}
	fresh := fields &^ s.rejectedCarriersLocked(ctx)
	if fresh == 0 {
		return 0, nil
	}
	s.rejectedCarriers[reasoningKeyOf(rt.Provider, rt.BaseURL, rt.Model)] |= fresh
	if s.o.Store == nil || s.sessionID == "" || s.o.PersistDisabled {
		return fresh, nil
	}
	keys := make([]reasoningRouteKey, 0, len(s.rejectedCarriers))
	for k := range s.rejectedCarriers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		if a.host != b.host {
			return a.host < b.host
		}
		return a.model < b.model
	})
	rows := make([]json.RawMessage, 0, len(keys))
	for _, k := range keys {
		names := s.rejectedCarriers[k].Names()
		sort.Strings(names)
		rows = append(rows, wire.Array(wire.Quote(k.provider), wire.Quote(k.host), wire.Quote(k.model), wire.Strings(names)))
	}
	patch := map[string]json.RawMessage{rejectedCarriersKey: wire.Array(rows...)}
	if err := s.o.Store.PatchModelConfig(ctx, s.sessionID, patch); err != nil {
		return fresh, err
	}
	return fresh, nil
}

type carrierRoute struct {
	model, baseURL string
	gemini         bool
	profile        providers.Profile
	keepFnSig      bool
	keys           []string
}

func consumesThoughtSignature(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "gemini") || strings.Contains(m, "gemma")
}

func (s *Session) declaredCarrierKeys(ctx context.Context) []string {
	keys := []string{"extra_content"}
	seen := map[string]bool{"extra_content": true}
	var opaque, text []string
	for _, p := range s.r.c.o.Providers.All(ctx) {
		sp := p.Spec()
		if sp.CarrierOpaque != "" {
			opaque = append(opaque, sp.CarrierOpaque)
		}
		if sp.CarrierText != "" {
			text = append(text, sp.CarrierText)
		}
	}
	for _, k := range slices.Concat(opaque, text) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

func signatureJSON(sig string) string {
	google := (&wire.Dict{}).SetString("thought_signature", sig)
	return string((&wire.Dict{}).Set("google", google.JSON()).JSON())
}

func (s *Session) shapeCarrier(ctx context.Context, m *wire.Message, route *carrierRoute) bool {
	if m.Role != wire.RoleAssistant {
		return false
	}
	var record wire.Value
	recordAt := -1
	for i, d := range m.ReasoningDetails {
		if wire.DetailType(d) == wire.CarrierType {
			if v, err := wire.ParseValue(d); err == nil && v.IsObject() {
				record, recordAt = v, i
				break
			}
		}
	}
	fnSig := false
	if !route.keepFnSig {
		for _, c := range m.ToolCalls {
			if c.FunctionSignature != "" {
				fnSig = true
				break
			}
		}
	}
	keys := slices.Clone(route.keys)
	if recordAt >= 0 {
		for _, f := range record.Fields() {
			if f.Key != "type" && !slices.Contains(keys, f.Key) {
				keys = append(keys, f.Key)
			}
		}
	}
	top := map[string]json.RawMessage{}
	for _, mb := range m.Members() {
		if slices.Contains(keys, mb.Key) {
			top[mb.Key] = mb.Value
		}
	}
	if recordAt < 0 && !fnSig && len(top) == 0 {
		return false
	}
	values := map[string]string{}
	if recordAt >= 0 {
		for _, f := range record.Fields() {
			if f.Key == "type" {
				values["type"] = f.Value.Text()
				continue
			}
			if f.Key == "extra_content" {
				if sig := wire.ThoughtSignature(f.Value.Raw()); sig != "" {
					values["extra_content"] = signatureJSON(sig)
				}
				continue
			}
			if str, ok := f.Value.Str(); ok {
				values[f.Key] = str
			}
		}
	}
	keepExtra := false
	for _, k := range keys {
		raw, ok := top[k]
		if !ok {
			continue
		}
		if k == "extra_content" {
			if m.ThoughtSignature != "" {
				values[k] = signatureJSON(m.ThoughtSignature)
				continue
			}
			v, err := wire.ParseValue(raw)
			if err != nil || !v.Truthy() {
				delete(values, k)
				continue
			}
			values[k] = string(raw)
			keepExtra = route.gemini && wire.ThoughtSignature(raw) == ""
			continue
		}
		if v, err := wire.ParseValue(raw); err == nil {
			if str, isText := v.Str(); isText {
				values[k] = str
				continue
			}
		}
		values[k] = string(raw)
	}
	m.ThoughtSignature = ""
	for _, k := range keys {
		delete(m.Carriers, k)
		if k == "extra_content" && keepExtra {
			continue
		}
		m.DropMember(k)
	}
	if len(m.Carriers) == 0 {
		m.Carriers = nil
	}
	if recordAt >= 0 {
		rest := slices.Concat(m.ReasoningDetails[:recordAt:recordAt], m.ReasoningDetails[recordAt+1:])
		if len(rest) == 0 {
			rest = nil
		}
		m.ReasoningDetails = rest
	}
	if route.gemini && values["extra_content"] != "" {
		m.ThoughtSignature = wire.ThoughtSignature([]byte(values["extra_content"]))
	}
	if route.profile != nil && !providers.HookFailed(s.scope, route.profile, providers.HookReplayCarriers) {
		answer, ok := s.r.c.o.Providers.ReplayCarriers(ctx, s.scope, route.profile, values, route.model, route.baseURL)
		if ok {
			for k, v := range answer {
				if k == "extra_content" {
					m.ThoughtSignature = wire.ThoughtSignature([]byte(v))
					continue
				}
				if m.Carriers == nil {
					m.Carriers = map[string]string{}
				}
				m.Carriers[k] = v
			}
		}
	}
	if fnSig {
		for i := range m.ToolCalls {
			m.ToolCalls[i].FunctionSignature = ""
		}
	}
	return true
}

func (s *Session) shapeWireCarriers(ctx context.Context, msgs []wire.Message, model, baseURL string) []wire.Message {
	route := &carrierRoute{model: model, baseURL: baseURL, gemini: consumesThoughtSignature(model), keys: s.declaredCarrierKeys(ctx)}
	if p, ok := s.r.c.o.Providers.ForHost(ctx, baseURL, false); ok {
		route.profile = p
		route.keepFnSig = s.r.c.o.Providers.ReplaysFunctionSignature(ctx, s.scope, p, model, baseURL)
	}
	failedBefore := route.profile == nil || providers.HookFailed(s.scope, route.profile, providers.HookReplayCarriers)
	shaped := cloneMessages(msgs)
	for i := range shaped {
		s.shapeCarrier(ctx, &shaped[i], route)
	}
	if !failedBefore && providers.HookFailed(s.scope, route.profile, providers.HookReplayCarriers) {
		shaped = cloneMessages(msgs)
		for i := range shaped {
			s.shapeCarrier(ctx, &shaped[i], route)
		}
	}
	return shaped
}

func cloneMessages(msgs []wire.Message) []wire.Message {
	out := make([]wire.Message, len(msgs))
	for i := range msgs {
		out[i] = msgs[i].Clone()
	}
	return out
}

func ImageRejection(errorBody string) bool {
	body := strings.ToLower(errorBody)
	for _, phrase := range imageRejectionPhrases {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

func CorruptImageRejection(errorBody string) bool {
	body := strings.ToLower(errorBody)
	for _, phrase := range imageCorruptPhrases {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

func StripImages(msgs []wire.Message) ([]wire.Message, bool) {
	found := false
	out := msgs[:0:0]
	for i := range msgs {
		m := msgs[i]
		if m.Parts == nil {
			out = append(out, m)
			continue
		}
		kept := make([]wire.Part, 0, len(m.Parts))
		for _, p := range m.Parts {
			if !imagePartWords[p.TypeName] && !(p.TypeName == "" && p.Kind == wire.PartImage) {
				kept = append(kept, p)
			}
		}
		if len(kept) == len(m.Parts) {
			out = append(out, m)
			continue
		}
		found = true
		switch {
		case len(kept) > 0:
			m.Parts = kept
		case m.Role == wire.RoleTool || len(m.ToolCalls) > 0:
			m.Parts, m.Text = nil, imageRemovedText
		default:
			continue
		}
		out = append(out, m)
	}
	return out, found
}

func providerModelKey(provider, model string) [2]string {
	return [2]string{strings.ToLower(strings.TrimSpace(provider)), strings.TrimSpace(model)}
}

func (s *Session) RememberImageRejection() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imageRejecting[providerModelKey(s.route.Provider, s.route.Model)] = true
}

func (s *Session) stripImagesForRejectingModel(msgs []wire.Message) []wire.Message {
	if !s.imageRejecting[providerModelKey(s.route.Provider, s.route.Model)] {
		return msgs
	}
	out, _ := StripImages(msgs)
	return out
}
