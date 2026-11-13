package types

import "strings"

const AnnotatedLLMRequestSchema = "nemo.relay.AnnotatedLlmRequest@2"

// AnnotatedLLMRequest is the provider neutral view of an LLM request
type AnnotatedLLMRequest struct {
	Messages           []Message          `json:"messages" serde:"default"`
	Instructions       MessageContent     `json:"instructions" serde:"option,skip_none"`
	Model              *string            `json:"model" serde:"skip_none"`
	Params             *GenerationParams  `json:"params" serde:"skip_none"`
	Tools              []ToolDefinition   `json:"tools" serde:"option,skip_none"`
	ToolChoice         ToolChoice         `json:"tool_choice" serde:"option,skip_none"`
	Store              *bool              `json:"store" serde:"skip_none"`
	PreviousResponseID *string            `json:"previous_response_id" serde:"skip_none"`
	Truncation         any                `json:"truncation" serde:"option,skip_none"`
	TruncationPresent  bool               `json:"-"`
	Reasoning          any                `json:"reasoning" serde:"option,skip_none"`
	ReasoningPresent   bool               `json:"-"`
	Include            any                `json:"include" serde:"option,skip_none"`
	IncludePresent     bool               `json:"-"`
	User               *string            `json:"user" serde:"skip_none"`
	Metadata           any                `json:"metadata" serde:"option,skip_none"`
	MetadataPresent    bool               `json:"-"`
	ServiceTier        *string            `json:"service_tier" serde:"skip_none"`
	ParallelToolCalls  *bool              `json:"parallel_tool_calls" serde:"skip_none"`
	MaxOutputTokens    *uint64            `json:"max_output_tokens" serde:"skip_none"`
	MaxToolCalls       *uint64            `json:"max_tool_calls" serde:"skip_none"`
	TopLogprobs        *uint64            `json:"top_logprobs" serde:"skip_none"`
	Stream             *bool              `json:"stream" serde:"skip_none"`
	APISpecific        APISpecificRequest `json:"api_specific" serde:"option,skip_none"`
	Extra              map[string]any     `json:"extra" serde:"flatten"`
}

func (AnnotatedLLMRequest) rustName() string { return "AnnotatedLlmRequest" }

// MarshalJSON writes the serde form
func (r AnnotatedLLMRequest) MarshalJSON() ([]byte, error) { return EncodeJSON(r) }

// UnmarshalJSON reads the serde form
func (r *AnnotatedLLMRequest) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, r) }

// Clone deep copies the request through its serde form
func (r *AnnotatedLLMRequest) Clone() *AnnotatedLLMRequest {
	if r == nil {
		return nil
	}
	out := cloneDataModel(*r)
	return &out
}

// SystemPrompt returns the first instruction or system text
func (r *AnnotatedLLMRequest) SystemPrompt() (string, bool) {
	if r.Instructions != nil {
		if text, ok := firstContentText(r.Instructions); ok {
			return text, true
		}
	}
	for _, m := range r.Messages {
		switch x := m.(type) {
		case MessageSystem:
			if text, ok := firstContentText(x.Content); ok {
				return text, true
			}
		case MessageDeveloper:
			if text, ok := firstContentText(x.Content); ok {
				return text, true
			}
		}
	}
	return "", false
}

// LastUserMessage returns the text of the most recent user message
func (r *AnnotatedLLMRequest) LastUserMessage() (string, bool) {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		switch x := r.Messages[i].(type) {
		case MessageUser:
			if text, ok := firstContentText(x.Content); ok {
				return text, true
			}
		case MessageProviderNative:
			role, _ := JSONGet(x.Value, "role")
			if x.Provider == "openai_responses" && role == "user" {
				if text, ok := nativeMessageText(x.Value); ok {
					return text, true
				}
			}
		}
	}
	return "", false
}

// HasToolCalls reports whether any message carries a tool call
func (r *AnnotatedLLMRequest) HasToolCalls() bool {
	for _, m := range r.Messages {
		switch x := m.(type) {
		case MessageAssistant:
			if x.ToolCalls != nil && (len(x.ToolCalls) > 0 || (x.Content != nil && contentHasToolUse(x.Content))) {
				return true
			}
			if x.ToolCalls == nil && x.Content != nil && contentHasToolUse(x.Content) {
				return true
			}
		case MessageToolCallItem:
			return true
		case MessageProviderNative:
			switch t, _ := JSONGet(x.Value, "type"); t {
			case "function_call", "custom_tool_call", "tool_use":
				return true
			}
		}
	}
	return false
}

// Returns the first text-bearing part of message content
func firstContentText(content MessageContent) (string, bool) {
	switch c := content.(type) {
	case MessageContentText:
		return string(c), true
	case MessageContentParts:
		for _, part := range c {
			switch p := part.(type) {
			case ContentPartText:
				return p.Text, true
			case ContentPartProviderNative:
				if text, ok := JSONGet(p.Value, "text"); ok {
					if s, ok := text.(string); ok {
						return s, true
					}
				}
				if text, ok := JSONGet(p.Value, "refusal"); ok {
					if s, ok := text.(string); ok {
						return s, true
					}
				}
			}
		}
	}
	return "", false
}

func contentHasToolUse(content MessageContent) bool {
	parts, ok := content.(MessageContentParts)
	if !ok {
		return false
	}
	for _, part := range parts {
		switch p := part.(type) {
		case ContentPartToolUse:
			return true
		case ContentPartProviderNative:
			switch t, _ := JSONGet(p.Value, "type"); t {
			case "tool_use", "mcp_tool_use", "server_tool_use":
				return true
			}
		}
	}
	return false
}

func nativeMessageText(value any) (string, bool) {
	content, ok := JSONGet(value, "content")
	if !ok {
		return "", false
	}
	switch c := content.(type) {
	case string:
		return c, true
	case []any:
		for _, part := range c {
			if text, ok := JSONGet(part, "text"); ok {
				if s, ok := text.(string); ok {
					return s, true
				}
			}
			if text, ok := JSONGet(part, "refusal"); ok {
				if s, ok := text.(string); ok {
					return s, true
				}
			}
		}
	}
	return "", false
}

// Message is one conversation item, a sealed sum of the Message variants
type Message interface {
	isMessage()
}

var messageRoles = []string{"system", "user", "developer", "assistant", "tool", "function", "tool_call", "tool_result", "provider_native"}

// MessageSystem is a system message
type MessageSystem struct {
	Content MessageContent `json:"content"`
	Name    *string        `json:"name" serde:"skip_none"`
}

// MessageUser is a user message
type MessageUser struct {
	Content MessageContent `json:"content"`
	Name    *string        `json:"name" serde:"skip_none"`
}

// MessageDeveloper is a developer message
type MessageDeveloper struct {
	Content MessageContent `json:"content"`
	Name    *string        `json:"name" serde:"skip_none"`
}

// MessageAssistant is an assistant message with optional content and tool calls
type MessageAssistant struct {
	Content   MessageContent `json:"content" serde:"option,skip_none"`
	ToolCalls []ToolCall     `json:"tool_calls" serde:"option,skip_none"`
	Name      *string        `json:"name" serde:"skip_none"`
}

// MessageTool is a tool result message keyed by tool call id
type MessageTool struct {
	Content    MessageContent `json:"content"`
	ToolCallID string         `json:"tool_call_id"`
}

// MessageFunction is a legacy function result message
type MessageFunction struct {
	Content *string `json:"content"`
	Name    string  `json:"name"`
}

// MessageToolCallItem is a standalone tool call item
type MessageToolCallItem struct {
	ID        *string        `json:"id" serde:"skip_none"`
	CallID    string         `json:"call_id"`
	Name      string         `json:"name"`
	Arguments any            `json:"arguments"`
	Extra     map[string]any `json:"extra" serde:"default,flatten"`
}

// MessageToolResultItem is a standalone tool result item
type MessageToolResultItem struct {
	ID     *string        `json:"id" serde:"skip_none"`
	CallID string         `json:"call_id"`
	Output any            `json:"output"`
	Extra  map[string]any `json:"extra" serde:"default,flatten"`
}

// MessageProviderNative is a provider specific item carried verbatim
type MessageProviderNative struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Value    any    `json:"value"`
}

func (MessageSystem) isMessage()         {}
func (MessageUser) isMessage()           {}
func (MessageDeveloper) isMessage()      {}
func (MessageAssistant) isMessage()      {}
func (MessageTool) isMessage()           {}
func (MessageFunction) isMessage()       {}
func (MessageToolCallItem) isMessage()   {}
func (MessageToolResultItem) isMessage() {}
func (MessageProviderNative) isMessage() {}

func (MessageSystem) rustName() string         { return "System" }
func (MessageUser) rustName() string           { return "User" }
func (MessageDeveloper) rustName() string      { return "Developer" }
func (MessageAssistant) rustName() string      { return "Assistant" }
func (MessageTool) rustName() string           { return "Tool" }
func (MessageFunction) rustName() string       { return "Function" }
func (MessageToolCallItem) rustName() string   { return "ToolCallItem" }
func (MessageToolResultItem) rustName() string { return "ToolResultItem" }
func (MessageProviderNative) rustName() string { return "ProviderNative" }

func (m MessageSystem) serdeSerialize() (any, error)    { return taggedStruct{"role", "system", m}, nil }
func (m MessageUser) serdeSerialize() (any, error)      { return taggedStruct{"role", "user", m}, nil }
func (m MessageDeveloper) serdeSerialize() (any, error) { return taggedStruct{"role", "developer", m}, nil }
func (m MessageAssistant) serdeSerialize() (any, error) { return taggedStruct{"role", "assistant", m}, nil }
func (m MessageTool) serdeSerialize() (any, error)      { return taggedStruct{"role", "tool", m}, nil }
func (m MessageFunction) serdeSerialize() (any, error)  { return taggedStruct{"role", "function", m}, nil }
func (m MessageToolCallItem) serdeSerialize() (any, error) {
	return taggedStruct{"role", "tool_call", m}, nil
}
func (m MessageToolResultItem) serdeSerialize() (any, error) {
	return taggedStruct{"role", "tool_result", m}, nil
}
func (m MessageProviderNative) serdeSerialize() (any, error) {
	return taggedStruct{"role", "provider_native", m}, nil
}

func (m MessageSystem) MarshalJSON() ([]byte, error)         { return EncodeJSON(m) }
func (m MessageUser) MarshalJSON() ([]byte, error)           { return EncodeJSON(m) }
func (m MessageDeveloper) MarshalJSON() ([]byte, error)      { return EncodeJSON(m) }
func (m MessageAssistant) MarshalJSON() ([]byte, error)      { return EncodeJSON(m) }
func (m MessageTool) MarshalJSON() ([]byte, error)           { return EncodeJSON(m) }
func (m MessageFunction) MarshalJSON() ([]byte, error)       { return EncodeJSON(m) }
func (m MessageToolCallItem) MarshalJSON() ([]byte, error)   { return EncodeJSON(m) }
func (m MessageToolResultItem) MarshalJSON() ([]byte, error) { return EncodeJSON(m) }
func (m MessageProviderNative) MarshalJSON() ([]byte, error) { return EncodeJSON(m) }

// DecodeMessage decodes one internally tagged message
func DecodeMessage(v any) (Message, error) {
	tag, rest, err := splitInternalTag(v, "role", "internally tagged enum Message")
	if err != nil {
		return nil, err
	}
	idx, err := variantIndex(tag, messageRoles)
	if err != nil {
		return nil, err
	}
	switch idx {
	case 0:
		var m MessageSystem
		err := decodeVariantStruct(rest, &m, "struct variant Message::System")
		return m, err
	case 1:
		var m MessageUser
		err := decodeVariantStruct(rest, &m, "struct variant Message::User")
		return m, err
	case 2:
		var m MessageDeveloper
		err := decodeVariantStruct(rest, &m, "struct variant Message::Developer")
		return m, err
	case 3:
		var m MessageAssistant
		err := decodeVariantStruct(rest, &m, "struct variant Message::Assistant")
		return m, err
	case 4:
		var m MessageTool
		err := decodeVariantStruct(rest, &m, "struct variant Message::Tool")
		return m, err
	case 5:
		var m MessageFunction
		err := decodeVariantStruct(rest, &m, "struct variant Message::Function")
		return m, err
	case 6:
		var m MessageToolCallItem
		err := decodeVariantStruct(rest, &m, "struct variant Message::ToolCallItem")
		return m, err
	case 7:
		var m MessageToolResultItem
		err := decodeVariantStruct(rest, &m, "struct variant Message::ToolResultItem")
		return m, err
	}
	var m MessageProviderNative
	err = decodeVariantStruct(rest, &m, "struct variant Message::ProviderNative")
	return m, err
}

// MessageContent is plain text or a list of parts, a sealed untagged sum
type MessageContent interface {
	isMessageContent()
}

// MessageContentText is text message content
type MessageContentText string

// MessageContentParts is multi-part message content
type MessageContentParts []ContentPart

func (MessageContentText) isMessageContent()  {}
func (MessageContentParts) isMessageContent() {}

func (c MessageContentText) serdeSerialize() (any, error) { return string(c), nil }
func (c MessageContentParts) serdeSerialize() (any, error) {
	return []ContentPart(c), nil
}

func (c MessageContentText) rustDebug(b *strings.Builder) {
	b.WriteString("Text(" + RustStrDebug(string(c)) + ")")
}

func (c MessageContentParts) rustDebug(b *strings.Builder) {
	b.WriteString("Parts(")
	b.WriteString(RustDebug([]ContentPart(c)))
	b.WriteByte(')')
}

func (c MessageContentText) MarshalJSON() ([]byte, error)  { return EncodeJSON(c) }
func (c MessageContentParts) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// DecodeMessageContent decodes text or parts as the untagged serde enum does
func DecodeMessageContent(v any) (MessageContent, error) {
	if s, ok := v.(string); ok {
		return MessageContentText(s), nil
	}
	if arr, ok := normalizeJSON(v).([]any); ok {
		parts := make(MessageContentParts, 0, len(arr))
		matched := true
		for _, item := range arr {
			part, err := DecodeContentPart(item)
			if err != nil {
				matched = false
				break
			}
			parts = append(parts, part)
		}
		if matched {
			return parts, nil
		}
	}
	return nil, UntaggedNoMatchError("MessageContent")
}

// ContentPart is one part of multi-part content, a sealed sum of the ContentPart variants
type ContentPart interface {
	isContentPart()
}

var contentPartTypes = []string{"text", "image_url", "image", "audio", "file", "refusal", "tool_use", "tool_result", "provider_native"}

// ContentPartText is a text part
type ContentPartText struct {
	Text  string         `json:"text"`
	Extra map[string]any `json:"extra" serde:"default,flatten"`
}

// ContentPartImageURL is an image referenced by URL
type ContentPartImageURL struct {
	ImageURL OpenAIImageURL `json:"image_url"`
	Extra    map[string]any `json:"extra" serde:"default,flatten"`
}

// ContentPartImage is an inline image
type ContentPartImage struct {
	Image any            `json:"image"`
	Extra map[string]any `json:"extra" serde:"default,flatten"`
}

// ContentPartAudio is an audio part
type ContentPartAudio struct {
	Audio any            `json:"audio"`
	Extra map[string]any `json:"extra" serde:"default,flatten"`
}

// ContentPartFile is a file part
type ContentPartFile struct {
	File  any            `json:"file"`
	Extra map[string]any `json:"extra" serde:"default,flatten"`
}

// ContentPartRefusal is a model refusal part
type ContentPartRefusal struct {
	Refusal string         `json:"refusal"`
	Extra   map[string]any `json:"extra" serde:"default,flatten"`
}

// ContentPartToolUse is an inline tool invocation part
type ContentPartToolUse struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input any            `json:"input"`
	Extra map[string]any `json:"extra" serde:"default,flatten"`
}

// ContentPartToolResult is an inline tool result part
type ContentPartToolResult struct {
	ToolUseID string         `json:"tool_use_id"`
	Content   any            `json:"content"`
	IsError   *bool          `json:"is_error" serde:"skip_none"`
	Extra     map[string]any `json:"extra" serde:"default,flatten"`
}

// ContentPartProviderNative is a provider specific part carried verbatim
type ContentPartProviderNative struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Value    any    `json:"value"`
}

func (ContentPartText) isContentPart()           {}
func (ContentPartImageURL) isContentPart()       {}
func (ContentPartImage) isContentPart()          {}
func (ContentPartAudio) isContentPart()          {}
func (ContentPartFile) isContentPart()           {}
func (ContentPartRefusal) isContentPart()        {}
func (ContentPartToolUse) isContentPart()        {}
func (ContentPartToolResult) isContentPart()     {}
func (ContentPartProviderNative) isContentPart() {}

func (ContentPartText) rustName() string           { return "Text" }
func (ContentPartImageURL) rustName() string       { return "ImageUrl" }
func (ContentPartImage) rustName() string          { return "Image" }
func (ContentPartAudio) rustName() string          { return "Audio" }
func (ContentPartFile) rustName() string           { return "File" }
func (ContentPartRefusal) rustName() string        { return "Refusal" }
func (ContentPartToolUse) rustName() string        { return "ToolUse" }
func (ContentPartToolResult) rustName() string     { return "ToolResult" }
func (ContentPartProviderNative) rustName() string { return "ProviderNative" }

func (p ContentPartText) serdeSerialize() (any, error)     { return taggedStruct{"type", "text", p}, nil }
func (p ContentPartImageURL) serdeSerialize() (any, error) { return taggedStruct{"type", "image_url", p}, nil }
func (p ContentPartImage) serdeSerialize() (any, error)    { return taggedStruct{"type", "image", p}, nil }
func (p ContentPartAudio) serdeSerialize() (any, error)    { return taggedStruct{"type", "audio", p}, nil }
func (p ContentPartFile) serdeSerialize() (any, error)     { return taggedStruct{"type", "file", p}, nil }
func (p ContentPartRefusal) serdeSerialize() (any, error)  { return taggedStruct{"type", "refusal", p}, nil }
func (p ContentPartToolUse) serdeSerialize() (any, error)  { return taggedStruct{"type", "tool_use", p}, nil }
func (p ContentPartToolResult) serdeSerialize() (any, error) {
	return taggedStruct{"type", "tool_result", p}, nil
}
func (p ContentPartProviderNative) serdeSerialize() (any, error) {
	return taggedStruct{"type", "provider_native", p}, nil
}

func (p ContentPartText) MarshalJSON() ([]byte, error)           { return EncodeJSON(p) }
func (p ContentPartImageURL) MarshalJSON() ([]byte, error)       { return EncodeJSON(p) }
func (p ContentPartImage) MarshalJSON() ([]byte, error)          { return EncodeJSON(p) }
func (p ContentPartAudio) MarshalJSON() ([]byte, error)          { return EncodeJSON(p) }
func (p ContentPartFile) MarshalJSON() ([]byte, error)           { return EncodeJSON(p) }
func (p ContentPartRefusal) MarshalJSON() ([]byte, error)        { return EncodeJSON(p) }
func (p ContentPartToolUse) MarshalJSON() ([]byte, error)        { return EncodeJSON(p) }
func (p ContentPartToolResult) MarshalJSON() ([]byte, error)     { return EncodeJSON(p) }
func (p ContentPartProviderNative) MarshalJSON() ([]byte, error) { return EncodeJSON(p) }

// DecodeContentPart decodes one internally tagged content part
func DecodeContentPart(v any) (ContentPart, error) {
	tag, rest, err := splitInternalTag(v, "type", "internally tagged enum ContentPart")
	if err != nil {
		return nil, err
	}
	idx, err := variantIndex(tag, contentPartTypes)
	if err != nil {
		return nil, err
	}
	switch idx {
	case 0:
		var p ContentPartText
		err := decodeVariantStruct(rest, &p, "struct variant ContentPart::Text")
		return p, err
	case 1:
		var p ContentPartImageURL
		err := decodeVariantStruct(rest, &p, "struct variant ContentPart::ImageUrl")
		return p, err
	case 2:
		var p ContentPartImage
		err := decodeVariantStruct(rest, &p, "struct variant ContentPart::Image")
		return p, err
	case 3:
		var p ContentPartAudio
		err := decodeVariantStruct(rest, &p, "struct variant ContentPart::Audio")
		return p, err
	case 4:
		var p ContentPartFile
		err := decodeVariantStruct(rest, &p, "struct variant ContentPart::File")
		return p, err
	case 5:
		var p ContentPartRefusal
		err := decodeVariantStruct(rest, &p, "struct variant ContentPart::Refusal")
		return p, err
	case 6:
		var p ContentPartToolUse
		err := decodeVariantStruct(rest, &p, "struct variant ContentPart::ToolUse")
		return p, err
	case 7:
		var p ContentPartToolResult
		err := decodeVariantStruct(rest, &p, "struct variant ContentPart::ToolResult")
		return p, err
	}
	var p ContentPartProviderNative
	err = decodeVariantStruct(rest, &p, "struct variant ContentPart::ProviderNative")
	return p, err
}

// OpenAIImageURL is an image URL with optional detail level
type OpenAIImageURL struct {
	URL    string  `json:"url"`
	Detail *string `json:"detail" serde:"skip_none"`
}

func (OpenAIImageURL) rustName() string { return "OpenAiImageUrl" }

// MarshalJSON writes the serde form
func (u OpenAIImageURL) MarshalJSON() ([]byte, error) { return EncodeJSON(u) }

// UnmarshalJSON reads the serde form
func (u *OpenAIImageURL) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, u) }

// ToolCall is an assistant tool call in chat form
type ToolCall struct {
	ID       string       `json:"id"`
	CallType string       `json:"type" rust:"call_type"`
	Function FunctionCall `json:"function"`
}

// MarshalJSON writes the serde form
func (c ToolCall) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *ToolCall) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// FunctionCall is a function name with JSON encoded arguments
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// MarshalJSON writes the serde form
func (c FunctionCall) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *FunctionCall) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// ToolDefinition is a tool offered to the model, a sealed sum of the ToolDefinition variants
type ToolDefinition interface {
	isToolDefinition()
}

// ToolDefinitionFunction is a function tool
type ToolDefinitionFunction struct {
	Function FunctionDefinition `json:"function"`
	Extra    map[string]any     `json:"extra" serde:"default,flatten"`
}

// ToolDefinitionProviderNative is a provider specific tool carried verbatim
type ToolDefinitionProviderNative struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Value    any    `json:"value"`
}

func (ToolDefinitionFunction) isToolDefinition()       {}
func (ToolDefinitionProviderNative) isToolDefinition() {}

func (ToolDefinitionFunction) rustName() string       { return "Function" }
func (ToolDefinitionProviderNative) rustName() string { return "ProviderNative" }

func (d ToolDefinitionFunction) serdeSerialize() (any, error) {
	return taggedStruct{"type", "function", d}, nil
}
func (d ToolDefinitionProviderNative) serdeSerialize() (any, error) {
	return taggedStruct{"type", "provider_native", d}, nil
}

func (d ToolDefinitionFunction) MarshalJSON() ([]byte, error)       { return EncodeJSON(d) }
func (d ToolDefinitionProviderNative) MarshalJSON() ([]byte, error) { return EncodeJSON(d) }

// DecodeToolDefinition decodes one internally tagged tool definition
func DecodeToolDefinition(v any) (ToolDefinition, error) {
	tag, rest, err := splitInternalTag(v, "type", "internally tagged enum ToolDefinition")
	if err != nil {
		return nil, err
	}
	idx, err := variantIndex(tag, []string{"function", "provider_native"})
	if err != nil {
		return nil, err
	}
	if idx == 0 {
		var d ToolDefinitionFunction
		err := decodeVariantStruct(rest, &d, "struct variant ToolDefinition::Function")
		return d, err
	}
	var d ToolDefinitionProviderNative
	err = decodeVariantStruct(rest, &d, "struct variant ToolDefinition::ProviderNative")
	return d, err
}

// FunctionDefinition describes a callable function tool
type FunctionDefinition struct {
	Name              string         `json:"name"`
	Description       *string        `json:"description" serde:"skip_none"`
	Parameters        any            `json:"parameters" serde:"option,skip_none"`
	ParametersPresent bool           `json:"-"`
	Strict            *bool          `json:"strict" serde:"skip_none"`
	Extra             map[string]any `json:"extra" serde:"default,flatten"`
}

// MarshalJSON writes the serde form
func (d FunctionDefinition) MarshalJSON() ([]byte, error) { return EncodeJSON(d) }

// UnmarshalJSON reads the serde form
func (d *FunctionDefinition) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, d) }

// ToolChoice selects how the model may call tools, a sealed sum of the ToolChoice variants
type ToolChoice interface {
	isToolChoice()
}

// ToolChoiceMode is one of the keyword tool choices
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
)

// ToolChoiceSpecific forces one named function
type ToolChoiceSpecific ToolChoiceFunction

// ToolChoiceProviderNative is a provider specific tool choice carried verbatim
type ToolChoiceProviderNative ProviderNativeComponent

func (ToolChoiceMode) isToolChoice()           {}
func (ToolChoiceSpecific) isToolChoice()       {}
func (ToolChoiceProviderNative) isToolChoice() {}

func (m ToolChoiceMode) serdeSerialize() (any, error) { return string(m), nil }
func (s ToolChoiceSpecific) serdeSerialize() (any, error) {
	return ToolChoiceFunction(s), nil
}
func (p ToolChoiceProviderNative) serdeSerialize() (any, error) {
	return ProviderNativeComponent(p), nil
}

func (m ToolChoiceMode) rustDebug(b *strings.Builder) {
	switch m {
	case ToolChoiceAuto:
		b.WriteString("Auto")
	case ToolChoiceNone:
		b.WriteString("None")
	case ToolChoiceRequired:
		b.WriteString("Required")
	default:
		b.WriteString(string(m))
	}
}

func (s ToolChoiceSpecific) rustDebug(b *strings.Builder) {
	b.WriteString("Specific(" + RustDebug(ToolChoiceFunction(s)) + ")")
}

func (p ToolChoiceProviderNative) rustDebug(b *strings.Builder) {
	b.WriteString("ProviderNative(" + RustDebug(ProviderNativeComponent(p)) + ")")
}

func (m ToolChoiceMode) MarshalJSON() ([]byte, error)           { return EncodeJSON(m) }
func (s ToolChoiceSpecific) MarshalJSON() ([]byte, error)       { return EncodeJSON(s) }
func (p ToolChoiceProviderNative) MarshalJSON() ([]byte, error) { return EncodeJSON(p) }

// DecodeToolChoice tries the keyword variants then the untagged struct variants as serde does
func DecodeToolChoice(v any) (ToolChoice, error) {
	modes := []string{"auto", "none", "required"}
	if idx, err := decodeUnitEnum(v, modes); err == nil {
		return ToolChoiceMode(modes[idx]), nil
	}
	var f ToolChoiceFunction
	if err := decodeInto(v, reflectValueOf(&f)); err == nil {
		return ToolChoiceSpecific(f), nil
	}
	var p ProviderNativeComponent
	if err := decodeInto(v, reflectValueOf(&p)); err == nil {
		return ToolChoiceProviderNative(p), nil
	}
	return nil, UntaggedNoMatchError("ToolChoice")
}

// ProviderNativeComponent is a provider specific component carried verbatim
type ProviderNativeComponent struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Value    any    `json:"value"`
}

// MarshalJSON writes the serde form
func (c ProviderNativeComponent) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *ProviderNativeComponent) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// ToolChoiceFunction names the function a specific tool choice forces
type ToolChoiceFunction struct {
	ChoiceType string                 `json:"type" rust:"choice_type"`
	Function   ToolChoiceFunctionName `json:"function"`
}

// MarshalJSON writes the serde form
func (f ToolChoiceFunction) MarshalJSON() ([]byte, error) { return EncodeJSON(f) }

// UnmarshalJSON reads the serde form
func (f *ToolChoiceFunction) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, f) }

// ToolChoiceFunctionName is the name of a forced function
type ToolChoiceFunctionName struct {
	Name string `json:"name"`
}

// MarshalJSON writes the serde form
func (n ToolChoiceFunctionName) MarshalJSON() ([]byte, error) { return EncodeJSON(n) }

// UnmarshalJSON reads the serde form
func (n *ToolChoiceFunctionName) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, n) }

// GenerationParams are the common sampling parameters
type GenerationParams struct {
	Temperature *float64 `json:"temperature" serde:"skip_none"`
	MaxTokens   *uint64  `json:"max_tokens" serde:"skip_none"`
	TopP        *float64 `json:"top_p" serde:"skip_none"`
	Stop        []string `json:"stop" serde:"option,skip_none"`
}

// MarshalJSON writes the serde form
func (p GenerationParams) MarshalJSON() ([]byte, error) { return EncodeJSON(p) }

// UnmarshalJSON reads the serde form
func (p *GenerationParams) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, p) }

// APISpecificRequest carries request fields only one provider API knows, tagged by api
type APISpecificRequest interface {
	isAPISpecificRequest()
}

// APISpecificRequestAnthropicMessages holds Anthropic Messages only request fields
type APISpecificRequestAnthropicMessages struct {
	CacheControl        any     `json:"cache_control" serde:"option,skip_none"`
	CacheControlPresent bool    `json:"-"`
	Container           *string `json:"container" serde:"skip_none"`
	InferenceGeo        *string `json:"inference_geo" serde:"skip_none"`
	OutputConfig        any     `json:"output_config" serde:"option,skip_none"`
	OutputConfigPresent bool    `json:"-"`
	Thinking            any     `json:"thinking" serde:"option,skip_none"`
	ThinkingPresent     bool    `json:"-"`
	TopK                *uint64 `json:"top_k" serde:"skip_none"`
	UserProfileID       *string `json:"user_profile_id" serde:"skip_none"`
}

// APISpecificRequestOpenAIChat holds OpenAI Chat only request fields
type APISpecificRequestOpenAIChat struct {
	Audio                      any      `json:"audio" serde:"option,skip_none"`
	AudioPresent               bool     `json:"-"`
	FrequencyPenalty           *float64 `json:"frequency_penalty" serde:"skip_none"`
	FunctionCall               any      `json:"function_call" serde:"option,skip_none"`
	FunctionCallPresent        bool     `json:"-"`
	Functions                  []any    `json:"functions" serde:"option,skip_none"`
	LogitBias                  any      `json:"logit_bias" serde:"option,skip_none"`
	LogitBiasPresent           bool     `json:"-"`
	Logprobs                   *bool    `json:"logprobs" serde:"skip_none"`
	Modalities                 []string `json:"modalities" serde:"option,skip_none"`
	Moderation                 any      `json:"moderation" serde:"option,skip_none"`
	ModerationPresent          bool     `json:"-"`
	N                          *uint64  `json:"n" serde:"skip_none"`
	Prediction                 any      `json:"prediction" serde:"option,skip_none"`
	PredictionPresent          bool     `json:"-"`
	PresencePenalty            *float64 `json:"presence_penalty" serde:"skip_none"`
	PromptCacheKey             *string  `json:"prompt_cache_key" serde:"skip_none"`
	PromptCacheOptions         any      `json:"prompt_cache_options" serde:"option,skip_none"`
	PromptCacheOptionsPresent  bool     `json:"-"`
	PromptCacheRetention       *string  `json:"prompt_cache_retention" serde:"skip_none"`
	ReasoningEffort            *string  `json:"reasoning_effort" serde:"skip_none"`
	ResponseFormat             any      `json:"response_format" serde:"option,skip_none"`
	ResponseFormatPresent      bool     `json:"-"`
	SafetyIdentifier           *string  `json:"safety_identifier" serde:"skip_none"`
	Seed                       *int64   `json:"seed" serde:"skip_none"`
	StreamOptions              any      `json:"stream_options" serde:"option,skip_none"`
	StreamOptionsPresent       bool     `json:"-"`
	Verbosity                  *string  `json:"verbosity" serde:"skip_none"`
	WebSearchOptions           any      `json:"web_search_options" serde:"option,skip_none"`
	WebSearchOptionsPresent    bool     `json:"-"`
}

// APISpecificRequestOpenAIResponses holds OpenAI Responses only request fields
type APISpecificRequestOpenAIResponses struct {
	Background                *bool   `json:"background" serde:"skip_none"`
	ContextManagement         any     `json:"context_management" serde:"option,skip_none"`
	ContextManagementPresent  bool    `json:"-"`
	Conversation              any     `json:"conversation" serde:"option,skip_none"`
	ConversationPresent       bool    `json:"-"`
	Moderation                any     `json:"moderation" serde:"option,skip_none"`
	ModerationPresent         bool    `json:"-"`
	Prompt                    any     `json:"prompt" serde:"option,skip_none"`
	PromptPresent             bool    `json:"-"`
	PromptCacheKey            *string `json:"prompt_cache_key" serde:"skip_none"`
	PromptCacheOptions        any     `json:"prompt_cache_options" serde:"option,skip_none"`
	PromptCacheOptionsPresent bool    `json:"-"`
	PromptCacheRetention      *string `json:"prompt_cache_retention" serde:"skip_none"`
	SafetyIdentifier          *string `json:"safety_identifier" serde:"skip_none"`
	StreamOptions             any     `json:"stream_options" serde:"option,skip_none"`
	StreamOptionsPresent      bool    `json:"-"`
	Text                      any     `json:"text" serde:"option,skip_none"`
	TextPresent               bool    `json:"-"`
}

// APISpecificRequestOCIGenAI holds OCI Generative AI only request fields
type APISpecificRequestOCIGenAI struct {
	CompartmentID      *string `json:"compartment_id" serde:"skip_none"`
	ServingMode        any     `json:"serving_mode" serde:"option,skip_none"`
	ServingModePresent bool    `json:"-"`
	APIFormat          *string `json:"api_format" serde:"skip_none"`
}

// APISpecificRequestCustom holds request fields of a custom API
type APISpecificRequestCustom struct {
	APIName string `json:"api_name"`
	Data    any    `json:"data"`
}

func (APISpecificRequestAnthropicMessages) isAPISpecificRequest() {}
func (APISpecificRequestOpenAIChat) isAPISpecificRequest()        {}
func (APISpecificRequestOpenAIResponses) isAPISpecificRequest()   {}
func (APISpecificRequestOCIGenAI) isAPISpecificRequest()          {}
func (APISpecificRequestCustom) isAPISpecificRequest()            {}

func (APISpecificRequestAnthropicMessages) rustName() string { return "AnthropicMessages" }
func (APISpecificRequestOpenAIChat) rustName() string        { return "OpenAIChat" }
func (APISpecificRequestOpenAIResponses) rustName() string   { return "OpenAIResponses" }
func (APISpecificRequestOCIGenAI) rustName() string          { return "OCIGenAI" }
func (APISpecificRequestCustom) rustName() string            { return "Custom" }

func (a APISpecificRequestAnthropicMessages) serdeSerialize() (any, error) {
	return taggedStruct{"api", "anthropic_messages", a}, nil
}
func (a APISpecificRequestOpenAIChat) serdeSerialize() (any, error) {
	return taggedStruct{"api", "openai_chat", a}, nil
}
func (a APISpecificRequestOpenAIResponses) serdeSerialize() (any, error) {
	return taggedStruct{"api", "openai_responses", a}, nil
}
func (a APISpecificRequestOCIGenAI) serdeSerialize() (any, error) {
	return taggedStruct{"api", "oci_genai", a}, nil
}
func (a APISpecificRequestCustom) serdeSerialize() (any, error) {
	return taggedStruct{"api", "custom", a}, nil
}

func (a APISpecificRequestAnthropicMessages) MarshalJSON() ([]byte, error) { return EncodeJSON(a) }
func (a APISpecificRequestOpenAIChat) MarshalJSON() ([]byte, error)        { return EncodeJSON(a) }
func (a APISpecificRequestOpenAIResponses) MarshalJSON() ([]byte, error)   { return EncodeJSON(a) }
func (a APISpecificRequestOCIGenAI) MarshalJSON() ([]byte, error)          { return EncodeJSON(a) }
func (a APISpecificRequestCustom) MarshalJSON() ([]byte, error)            { return EncodeJSON(a) }

var apiSpecificRequestNames = []string{"anthropic_messages", "openai_chat", "openai_responses", "oci_genai", "custom"}

// DecodeAPISpecificRequest decodes the api tagged request extension
func DecodeAPISpecificRequest(v any) (APISpecificRequest, error) {
	tag, rest, err := splitInternalTag(v, "api", "internally tagged enum ApiSpecificRequest")
	if err != nil {
		return nil, err
	}
	idx, err := variantIndex(tag, apiSpecificRequestNames)
	if err != nil {
		return nil, err
	}
	switch idx {
	case 0:
		var a APISpecificRequestAnthropicMessages
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificRequest::AnthropicMessages")
		return a, err
	case 1:
		var a APISpecificRequestOpenAIChat
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificRequest::OpenAIChat")
		return a, err
	case 2:
		var a APISpecificRequestOpenAIResponses
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificRequest::OpenAIResponses")
		return a, err
	case 3:
		var a APISpecificRequestOCIGenAI
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificRequest::OCIGenAI")
		return a, err
	}
	var a APISpecificRequestCustom
	err = decodeVariantStruct(rest, &a, "struct variant ApiSpecificRequest::Custom")
	return a, err
}

func init() {
	registerInterfaceDecoder(DecodeMessage)
	registerInterfaceDecoder(DecodeMessageContent)
	registerInterfaceDecoder(DecodeContentPart)
	registerInterfaceDecoder(DecodeToolDefinition)
	registerInterfaceDecoder(DecodeToolChoice)
	registerInterfaceDecoder(DecodeAPISpecificRequest)
}
