package agent

import (
	"slices"
	"strings"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	carrierRC = "reasoning_content"
	carrierR  = "reasoning"
	carrierRD = "reasoning_details"
)

// Reasoning keys an assistant replay copy may carry, in wire order
var reasoningCarriers = []string{carrierRC, carrierR, carrierRD}

// Session model_config key holding the reasoning carriers each route rejected
const rejectionModelConfigKey = "reasoning_rejected_carriers"

var sanitizeImagePartTypes = map[string]bool{"image_url": true, "image": true, "input_image": true}

var imageRejectionPhrases = []string{
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

var imageCorruptPhrases = []string{
	"image data you provided does not represent a valid image",
	"failed to decode image",
}

// Session state the sanitization methods read and fill, embedded in AIAgent
type sanitizationState struct {
	imageRejectingModels         map[[2]string]bool
	reasoningRejectingRoutes     map[[3]string]map[string]bool
	reasoningRejectionsLoadedFor string
}

// Appends a closing assistant row when the transcript ends on a tool result, hidden unless a banner was given
func closeInterruptedToolSequence(messages *[]map[string]any, finalResponse any, stopped bool) bool {
	if len(*messages) == 0 || (*messages)[len(*messages)-1]["role"] != "tool" {
		return false
	}
	text, _ := finalResponse.(string)
	stripped := pystr.Strip(text)
	var row map[string]any
	if stripped == "" || stripped == InterruptedPlaceholder {
		row = HiddenInterruptPlaceholderRow()
	} else {
		row = map[string]any{"role": "assistant", "content": stripped}
	}
	if stopped {
		row["display_metadata"] = map[string]any{"interrupted": true}
	}
	state.AppendMessage(messages, row, nil)
	return true
}

// Exact UTF-8 size of the compact JSON of the messages, the summed str lengths when serialization fails
func serializedMessagesBytes(messages any) int {
	list, ok := messages.([]any)
	if !ok || len(list) == 0 {
		return 0
	}
	text, err := pyjson.Dumps(list, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"),
		pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err == nil {
		err = pystr.StrictUTF8EncodeError(text)
	}
	if err == nil {
		return len(text)
	}
	total := 0
	for _, m := range list {
		total += pystr.Len(pyrepr.Str(m))
	}
	return total
}

// Removes image parts in place, marking emptied tool rows and dropping other emptied rows
func stripImagesFromMessages(messages *[]any) bool {
	found := false
	var toDelete []int
	for i, raw := range *messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		var newParts []any
		for _, p := range content {
			if pd, isDict := p.(map[string]any); isDict {
				if kind, isStr := pd["type"].(string); isStr && sanitizeImagePartTypes[kind] {
					continue
				}
			}
			newParts = append(newParts, p)
		}
		if len(newParts) == len(content) {
			continue
		}
		found = true
		switch {
		case len(newParts) > 0:
			msg["content"] = newParts
			delete(msg, state.DBPersistedMarker)
		case msg["role"] == "tool" || pyrepr.Truthy(msg["tool_calls"]):
			msg["content"] = "[image content removed — server does not support images]"
			delete(msg, state.DBPersistedMarker)
		default:
			toDelete = append(toDelete, i)
		}
		dropStaleAPIContent(msg)
	}
	for i := len(toDelete) - 1; i >= 0; i-- {
		*messages = slices.Delete(*messages, toDelete[i], toDelete[i]+1)
	}
	return found
}

// Strips images from the per call messages when the active provider and model rejected image content
func (a *AIAgent) stripImagesForRejectingModel(apiMessages *[]any) bool {
	if !a.imageRejectingModels[providerModelKey(a)] {
		return false
	}
	return apiMessages != nil && stripImagesFromMessages(apiMessages)
}

// True when a provider error says image or multimodal input is unsupported
func looksLikeImageContentRejection(errorBody any) bool {
	return containsAnyPhrase(pystr.Lower(pyStrOr(errorBody, "")), imageRejectionPhrases)
}

// True when the rejection is about a bad image payload rather than the model capability
func looksLikeCorruptImageRejection(errorBody any) bool {
	return containsAnyPhrase(pystr.Lower(pyStrOr(errorBody, "")), imageCorruptPhrases)
}

// True when body contains any of the phrases
func containsAnyPhrase(body string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

// True when both values are the same list object, the Python is test for lists
func sameList(a, b any) bool {
	x, ok := a.([]any)
	y, ok2 := b.([]any)
	return ok && ok2 && x != nil && y != nil && len(x) == len(y) && cap(x) == cap(y) && unsafe.SliceData(x) == unsafe.SliceData(y)
}

// Makes every outbound kwargs string JSON safe, stripping non ASCII on a detached tools copy when forced
func (a *AIAgent) sanitizeOutboundKwargs(apiKwargs map[string]any) {
	state.SanitizeStructureSurrogates(apiKwargs)
	if !a.forceASCIIPayload {
		return
	}
	if tools, ok := apiKwargs["tools"]; ok && tools != nil && sameList(tools, a.tools) {
		apiKwargs["tools"] = cloneMessageForSend(tools)
	}
	state.SanitizeStructureNonASCII(apiKwargs)
}

// True when stale assistant reasoning text is replayed on the wire for the route
func staleThinkingReachesWire(apiMode, providerName, model, baseURL string) bool {
	if apiMode == "anthropic_messages" && provider.NativeAnthropicPreservesPriorThinking(baseURL, model) {
		return true
	}
	if apiMode == "" {
		return provider.NeedsReasoningEcho(providerName, model, baseURL)
	}
	route := provider.ReasoningReplayRouteFor(apiMode, providerName, model, baseURL, false, nil)
	if route.Carriers == nil {
		return apiMode != "codex_responses" && route.Pad
	}
	return route.Carriers[carrierRC] || route.Carriers[carrierR]
}

// Non blank reasoning_content, else non blank reasoning, of a turn
func replayableReasoningText(msg map[string]any) (string, bool) {
	for _, key := range []string{carrierRC, carrierR} {
		if value, ok := msg[key].(string); ok && pystr.Strip(value) != "" {
			return value, true
		}
	}
	return "", false
}

// Native adapter must echo pad keeping an explicit value and padding a tool call turn holding only reasoning
func applyLegacyMustEchoPad(sourceMsg, apiMsg map[string]any) {
	if existing, ok := sourceMsg[carrierRC].(string); ok {
		if existing == "" {
			existing = " "
		}
		apiMsg[carrierRC] = existing
		return
	}
	if reasoning, ok := sourceMsg[carrierR].(string); ok && reasoning != "" && !pyrepr.Truthy(sourceMsg["tool_calls"]) {
		apiMsg[carrierRC] = reasoning
		return
	}
	apiMsg[carrierRC] = " "
}

// Shapes an assistant replay message's reasoning keys for the active route, nil carriers meaning a native wire
func applyReasoningContentPolicy(sourceMsg, apiMsg map[string]any, needsThinkingPad bool, carriers map[string]bool) {
	if sourceMsg["role"] != "assistant" {
		return
	}
	text, hasText := replayableReasoningText(sourceMsg)
	effective := carriers
	if carriers == nil {
		effective = map[string]bool{}
		if needsThinkingPad {
			effective[carrierRC] = true
		}
	}
	switch {
	case carriers == nil && needsThinkingPad:
		applyLegacyMustEchoPad(sourceMsg, apiMsg)
	case effective[carrierRC] && (hasText || needsThinkingPad):
		if hasText {
			apiMsg[carrierRC] = text
		} else {
			apiMsg[carrierRC] = " "
		}
	default:
		delete(apiMsg, carrierRC)
	}
	if effective[carrierR] && hasText {
		apiMsg[carrierR] = text
	} else {
		delete(apiMsg, carrierR)
	}
	if carriers != nil && !carriers[carrierRD] {
		popped := apiMsg[carrierRD]
		delete(apiMsg, carrierRD)
		var private []any
		items, _ := popped.([]any)
		for _, d := range items {
			if dict, ok := d.(map[string]any); ok && strings.HasSuffix(pyStrOr(dict["type"], ""), ".native_assistant") {
				private = append(private, d)
			}
		}
		if len(private) > 0 {
			apiMsg[carrierRD] = private
		}
	}
}

// Reasoning carrier values of a message in carrier order
func reasoningShape(msg map[string]any) [3]any {
	return [3]any{msg[carrierRC], msg[carrierR], msg[carrierRD]}
}

// Reconciles built assistant turns with the active route and returns how many changed
func reapplyReasoningEcho(apiMessages []any, needsThinkingPad bool, carriers map[string]bool) int {
	changed := 0
	for _, raw := range apiMessages {
		apiMsg, ok := raw.(map[string]any)
		if !ok || apiMsg["role"] != "assistant" {
			continue
		}
		before := reasoningShape(apiMsg)
		if carriers == nil && needsThinkingPad && pyrepr.Truthy(apiMsg[carrierRC]) {
			continue
		}
		applyReasoningContentPolicy(apiMsg, apiMsg, needsThinkingPad, carriers)
		after := reasoningShape(apiMsg)
		for i := range before {
			if !pyrepr.Equal(before[i], after[i]) {
				changed++
				break
			}
		}
	}
	return changed
}

// Provider, base URL host and model a reasoning rejection is learned for
func (a *AIAgent) reasoningRouteKey() [3]string {
	return [3]string{pystr.Lower(pystr.Strip(a.provider)), provider.Hostname(a.baseURL), pystr.Strip(a.model)}
}

// Reasoning keys the active route rejected this session, loading the persisted record once per session
func (a *AIAgent) rejectedReasoningCarriers() (map[string]bool, error) {
	if a.reasoningRejectingRoutes == nil {
		a.reasoningRejectingRoutes = map[[3]string]map[string]bool{}
	}
	if a.sessionID != "" && a.reasoningRejectionsLoadedFor != a.sessionID {
		a.reasoningRejectionsLoadedFor = a.sessionID
		var stored any = []any{}
		if a.sessionDB != nil {
			value, found, err := a.sessionDB.GetModelConfigValue(a.sessionID, rejectionModelConfigKey)
			if err != nil {
				return nil, err
			}
			if found {
				stored = value
			}
		}
		entries, _ := stored.([]any)
		for _, raw := range entries {
			entry, ok := raw.([]any)
			if !ok || len(entry) != 4 {
				continue
			}
			keys, ok := entry[3].([]any)
			if !ok {
				continue
			}
			var route [3]string
			valid := true
			for i := range route {
				if route[i], ok = entry[i].(string); !ok {
					valid = false
				}
			}
			if !valid {
				continue
			}
			if a.reasoningRejectingRoutes[route] == nil {
				a.reasoningRejectingRoutes[route] = map[string]bool{}
			}
			for _, k := range keys {
				if s, ok := k.(string); ok && slices.Contains(reasoningCarriers, s) {
					a.reasoningRejectingRoutes[route][s] = true
				}
			}
		}
	}
	out := map[string]bool{}
	for k := range a.reasoningRejectingRoutes[a.reasoningRouteKey()] {
		out[k] = true
	}
	return out, nil
}

// Remembers and persists the reasoning keys this route newly rejected, empty when no retry is warranted
func (a *AIAgent) recordReasoningFieldRejection(errorBody, sentMessages any) (map[string]bool, error) {
	if a.apiMode != "chat_completions" {
		return map[string]bool{}, nil
	}
	fields := provider.RejectedReasoningFields(errorBody, sentMessages)
	if provider.NeedsReasoningEcho(a.provider, a.model, a.baseURL) {
		delete(fields, carrierRC)
	}
	if len(fields) == 0 {
		return map[string]bool{}, nil
	}
	rejected, err := a.rejectedReasoningCarriers()
	if err != nil {
		return nil, err
	}
	fresh := map[string]bool{}
	for k := range fields {
		if !rejected[k] {
			fresh[k] = true
		}
	}
	if len(fresh) == 0 {
		return fresh, nil
	}
	key := a.reasoningRouteKey()
	if a.reasoningRejectingRoutes[key] == nil {
		a.reasoningRejectingRoutes[key] = map[string]bool{}
	}
	for k := range fresh {
		a.reasoningRejectingRoutes[key][k] = true
	}
	if a.sessionDB != nil && a.sessionID != "" && !a.persistDisabled {
		routes := make([][3]string, 0, len(a.reasoningRejectingRoutes))
		for route := range a.reasoningRejectingRoutes {
			routes = append(routes, route)
		}
		slices.SortFunc(routes, func(x, y [3]string) int { return slices.Compare(x[:], y[:]) })
		payload := make([]any, 0, len(routes))
		for _, route := range routes {
			keys := make([]string, 0, len(a.reasoningRejectingRoutes[route]))
			for k := range a.reasoningRejectingRoutes[route] {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			sorted := make([]any, len(keys))
			for i, k := range keys {
				sorted[i] = k
			}
			payload = append(payload, []any{route[0], route[1], route[2], sorted})
		}
		if err := a.sessionDB.PatchModelConfig(a.sessionID, map[string]any{rejectionModelConfigKey: payload}); err != nil {
			return nil, err
		}
	}
	return fresh, nil
}
