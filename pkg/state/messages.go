package state

import (
	"encoding/json"
	"strings"
)

// Prefix marking structured content stored as JSON text
const contentJSONPrefix = "\x00json:"

// Display metadata key marking a row the model sees but the person never does
const ModelOnlyKey = "model_only"

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
	// Dict keys the struct has no field for, kept for lossless dict round trips
	Extra map[string]any `json:"-"`
}

// Function part of a tool call
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// Dict keys the struct has no field for, kept for lossless dict round trips
	Extra map[string]any `json:"-"`
}

// One transcript row in provider message shape
type Message struct {
	// Row id, zero before the message is stored
	RowID int64  `json:"-"`
	Role  string `json:"role"`
	// String text or a list of content parts
	Content          any             `json:"content"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolName         string          `json:"tool_name,omitempty"`
	Timestamp        float64         `json:"timestamp,omitempty"`
	TokenCount       int             `json:"token_count,omitempty"`
	FinishReason     string          `json:"finish_reason,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	// Opaque replay items of a Responses API turn
	CodexReasoningItems json.RawMessage `json:"codex_reasoning_items,omitempty"`
	CodexMessageItems   json.RawMessage `json:"codex_message_items,omitempty"`
	PlatformMessageID   string          `json:"platform_message_id,omitempty"`
	Observed            bool            `json:"observed,omitempty"`
	CompressedSummary   bool            `json:"compressed_summary,omitempty"`
	// Exact string sent to the API when it differed from Content
	APIContent      string         `json:"api_content,omitempty"`
	DisplayKind     string         `json:"display_kind,omitempty"`
	DisplayMetadata map[string]any `json:"display_metadata,omitempty"`
	// Durable id kept across clones and compactions
	MessageUID   string            `json:"message_uid,omitempty"`
	AbsorbedUIDs []string          `json:"absorbed_message_uids,omitempty"`
	ToolCallUIDs map[string]string `json:"tool_call_uids,omitempty"`
	ToolCallUID  string            `json:"tool_call_uid,omitempty"`
	Active       bool              `json:"-"`
	Compacted    bool              `json:"-"`
	// Set once the row exists so a flush never appends it again
	Persisted bool `json:"-"`
	// Dict keys the struct has no field for, such as underscore bookkeeping keys and data_use
	Extra map[string]any `json:"-"`
}

// Text of a message, content parts joined when the content is a list
func (m *Message) Text() string {
	return ContentText(m.Content)
}

// Plain text of provider content, text parts joined with newlines
func ContentText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					parts = append(parts, t)
				}
			} else if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case []map[string]any:
		var parts []string
		for _, pm := range c {
			if t, ok := pm["text"].(string); ok {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	b, _ := json.Marshal(content)
	return string(b)
}

// Visibility predicate of a transcript read
func activeClause(includeInactive, includeCompacted bool) string {
	switch {
	case includeInactive:
		return ""
	case includeCompacted:
		return " AND (active = 1 OR compacted = 1)"
	}
	return " AND active = 1"
}
