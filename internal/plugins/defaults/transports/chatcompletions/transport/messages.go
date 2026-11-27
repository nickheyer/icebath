package transport

import (
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	chatconfig "github.com/nickheyer/icebath/internal/plugins/defaults/transports/chatcompletions/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Reasoning fields an assistant message can be replayed with
const (
	reasoningContent = "reasoning_content"
	reasoningField   = "reasoning"
	reasoningDetails = "reasoning_details"
)

// What a reply carried beside its text and calls that its provider wants back when it is replayed
type carried struct {
	// Text of the reply as it came, with the reasoning blocks shown apart, when it differs from what the message shows
	Content string `json:"content,omitempty"`
	// Reasoning the provider sent in its own field, when it differs from what the message shows
	Reasoning string `json:"reasoning_content,omitempty"`
	// Structured reasoning entries, replayed as they came
	Details []any `json:"reasoning_details,omitempty"`
	// Gemini thought signature of the reply, and those of its tool calls by id
	Signature string                 `json:"thought_signature,omitempty"`
	Calls     map[string]callCarried `json:"tool_calls,omitempty"`
	// Fields kinds declare their replies carry, such as Copilot's reasoning_opaque and reasoning_text, by name
	Fields map[string]string `json:"fields,omitempty"`
}

// Signature a tool call came with: Gemini's thought_signature of its function, and its extra_content
type callCarried struct {
	Signature string `json:"thought_signature,omitempty"`
	Extra     any    `json:"extra_content,omitempty"`
}

// Model id parts of Mistral's models, which take only nine character tool call ids
var mistralModels = []string{"mistral", "magistral", "mixtral", "devstral", "codestral", "ministral", "pixtral", "voxtral"}

// Whether the route takes only Mistral's nine character tool call ids
func (c *client) shortIDs(p *prepared) bool {
	return p.spec.ShortToolIDs || p.hosted.ShortToolIDs || strings.EqualFold(c.route.Provider, "mistral") ||
		provider.BaseHostMatches(c.route.BaseURL, "mistral.ai") || provider.Contains(strings.ToLower(c.route.Model), mistralModels...)
}

var mistralID = regexp.MustCompile(`^[A-Za-z0-9]{9}$`)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Nine character base62 digest of a tool call id, the id itself when it already is one
func shortID(id string) string {
	if id == "" || mistralID.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	n := new(big.Int).SetBytes(sum[:8])
	out := make([]byte, 9)
	for i := range out {
		var rem big.Int
		n.DivMod(n, big.NewInt(62), &rem)
		out[i] = base62[rem.Int64()]
	}
	return string(out)
}

// Reasoning fields the route reads back: the kind's, else those of the kind claiming its address, else reasoning_content, with reasoning too
// for a server on this network; reasoning_details as well for a kind whose replies carry its own detail entries
func (c *client) carriers(p *prepared) []string {
	set := p.spec.Carriers
	if set == nil {
		set = p.hosted.Carriers
	}
	if set == nil {
		set = []string{reasoningContent}
		if models.Local(c.route.BaseURL) {
			set = append(set, reasoningField)
		}
	}
	if len(set) > 0 && !slices.Contains(set, reasoningDetails) && (p.spec.NativeReasoningDetailsType != "" || p.hosted.NativeReasoningDetailsType != "") {
		set = append(slices.Clone(set), reasoningDetails)
	}
	return set
}

// Model id parts of the families that refuse a tool call turn whose reasoning isn't echoed back
var echoModels = []string{"deepseek", "mimo", "stealth/missingno"}

// Whether the route refuses a tool call turn without the reasoning_content of the replayed messages
func (c *client) echoes(p *prepared, cfg *config.Config) bool {
	return cfg.GetBool(chatconfig.ReasoningEcho) || p.spec.EchoesReasoning || p.hosted.EchoesReasoning ||
		provider.Contains(strings.ToLower(c.route.Model), echoModels...)
}

// Whether the route reads Gemini thought signatures back
func (c *client) signs(p *prepared) bool {
	model := strings.ToLower(c.route.Model)
	return strings.Contains(model, "gemini") || strings.Contains(model, "gemma") || p.spec.ReplaysToolCallSignatures ||
		p.hosted.ReplaysToolCallSignatures || provider.BaseHostMatches(c.route.BaseURL, "generativelanguage.googleapis.com") ||
		provider.BaseHostMatches(c.route.BaseURL, "aiplatform.googleapis.com")
}

// Whether tool calls are replayed with the thought signatures of their functions, as the kind claiming the address asks
func (c *client) functionSignatures(p *prepared) bool {
	for _, spec := range []provider.Spec{p.spec, p.hosted} {
		if spec.FunctionSignatures != nil && spec.FunctionSignatures(c.route.Model, c.route.BaseURL) {
			return true
		}
	}
	return false
}

// Messages of the request as the wire sends them
func (c *client) messages(p *prepared, cfg *config.Config) []map[string]any {
	set := c.carriers(p)
	pad := c.echoes(p, cfg) && slices.Contains(set, reasoningContent)
	signs, functions, short := c.signs(p), c.functionSignatures(p), c.shortIDs(p)
	var replay func(map[string]string) map[string]string
	for _, spec := range []provider.Spec{p.spec, p.hosted} {
		if spec.Replay != nil {
			replay = func(kept map[string]string) map[string]string { return spec.Replay(p.call, kept) }
			break
		}
	}
	id := func(id string) string {
		if short {
			return shortID(id)
		}
		return id
	}
	out := make([]map[string]any, 0, len(p.request.Messages))
	for _, m := range p.request.Messages {
		switch m.GetRole() {
		case sessionsv1.Role_ROLE_SYSTEM:
			out = append(out, map[string]any{"role": "system", "content": m.GetText()})
		case sessionsv1.Role_ROLE_USER:
			out = append(out, map[string]any{"role": "user", "content": content(m.GetText(), m.GetMedia())})
		case sessionsv1.Role_ROLE_TOOL:
			out = append(out, map[string]any{"role": "tool", "tool_call_id": id(m.GetToolCallId()), "content": content(m.GetText(), m.GetMedia())})
		case sessionsv1.Role_ROLE_ASSISTANT:
			out = append(out, assistant(m, set, pad, signs, functions, id, replay))
		}
	}
	return out
}

// Replayed assistant message with the reasoning fields the route reads, the signatures it takes and the carriers its kind wants back
func assistant(m *sessionsv1.Message, set []string, pad, signs, functions bool, id func(string) string,
	replay func(map[string]string) map[string]string) map[string]any {
	kept := transport.Carried[carried](m, carrier)
	text := m.GetText()
	if kept.Content != "" {
		text = kept.Content
	}
	out := map[string]any{"role": "assistant", "content": text}
	if text == "" && len(m.GetToolCalls()) > 0 {
		out["content"] = nil
	}
	reasoning := kept.Reasoning
	if strings.TrimSpace(reasoning) == "" {
		reasoning = m.GetReasoning()
	}
	if slices.Contains(set, reasoningContent) && (strings.TrimSpace(reasoning) != "" || pad) {
		out[reasoningContent] = reasoning
		if strings.TrimSpace(reasoning) == "" {
			out[reasoningContent] = " "
		}
	}
	if slices.Contains(set, reasoningField) && strings.TrimSpace(reasoning) != "" {
		out[reasoningField] = reasoning
	}
	if slices.Contains(set, reasoningDetails) && len(kept.Details) > 0 {
		out[reasoningDetails] = kept.Details
	}
	if signs && kept.Signature != "" {
		out["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": kept.Signature}}
	}
	if replay != nil && len(kept.Fields) > 0 {
		for name, value := range replay(kept.Fields) {
			out[name] = value
		}
	}
	var calls []any
	for _, call := range m.GetToolCalls() {
		function := map[string]any{"name": call.GetName(), "arguments": call.GetArguments()}
		entry := map[string]any{"id": id(call.GetId()), "type": "function", "function": function}
		if signature := kept.Calls[call.GetId()]; signs {
			if functions && signature.Signature != "" {
				function["thought_signature"] = signature.Signature
			}
			if signature.Extra != nil && thoughtSignature(signature.Extra) != "" {
				entry["extra_content"] = signature.Extra
			}
		}
		calls = append(calls, entry)
	}
	if len(calls) > 0 {
		out["tool_calls"] = calls
	}
	return out
}

// Text of a message, or its text and its pictures and videos as parts
func content(text string, media []*sessionsv1.Media) any {
	if len(media) == 0 {
		return text
	}
	parts := []any{map[string]any{"type": "text", "text": text}}
	for _, item := range media {
		address := "data:" + item.GetMimeType() + ";base64," + base64.StdEncoding.EncodeToString(item.GetData())
		kind := "image_url"
		if strings.HasPrefix(item.GetMimeType(), "video/") {
			kind = "video_url"
		}
		parts = append(parts, map[string]any{"type": kind, kind: map[string]any{"url": address}})
	}
	return parts
}

// Gemini thought signature of an extra_content record, under google or at its top
func thoughtSignature(extra any) string {
	object, ok := extra.(map[string]any)
	if !ok {
		return ""
	}
	if google, ok := object["google"].(map[string]any); ok {
		signature, _ := google["thought_signature"].(string)
		if strings.TrimSpace(signature) == "" {
			return ""
		}
		return signature
	}
	signature, _ := object["thought_signature"].(string)
	if strings.TrimSpace(signature) == "" {
		return ""
	}
	return signature
}
