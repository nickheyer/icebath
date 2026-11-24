package acputil

import (
	"encoding/json"
	"fmt"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Content a tool call shows: *ContentToolCallContent, *FileEditToolCallContent or *TerminalToolCallContent
type ToolCallContent interface{ isToolCallContent() }

func (*ContentToolCallContent) isToolCallContent()  {}
func (*FileEditToolCallContent) isToolCallContent() {}
func (*TerminalToolCallContent) isToolCallContent() {}

// Content block a tool call shows
type ContentToolCallContent struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Content ContentBlock     `json:"content"`
}

// JSON object of the content with type "content"
func (c ContentToolCallContent) MarshalJSON() ([]byte, error) {
	type plain ContentToolCallContent
	return withTag(plain(c), "type", "content")
}

// Fills the content from its JSON object, which must carry content
func (c *ContentToolCallContent) UnmarshalJSON(data []byte) error {
	type plain ContentToolCallContent
	var w struct {
		*plain
		Content json.RawMessage `json:"content"`
	}
	w.plain = (*plain)(c)
	if err := decodeObject(data, &w, "ContentToolCallContent", "content"); err != nil {
		return err
	}
	block, err := decodeContentBlock(w.Content)
	if err != nil {
		return fmt.Errorf("content: %w", err)
	}
	c.Content = block
	return nil
}

// File edit a tool call makes: the new text and, for a file that exists, the old text
type FileEditToolCallContent struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	NewText string           `json:"newText"`
	OldText *string          `json:"oldText,omitempty"`
	Path    string           `json:"path"`
}

// JSON object of the edit with type "diff"
func (c FileEditToolCallContent) MarshalJSON() ([]byte, error) {
	type plain FileEditToolCallContent
	return withTag(plain(c), "type", "diff")
}

// Fills the edit from its JSON object, which must carry newText and path
func (c *FileEditToolCallContent) UnmarshalJSON(data []byte) error {
	type plain FileEditToolCallContent
	return decodeObject(data, (*plain)(c), "FileEditToolCallContent", "newText", "path")
}

// Client terminal whose output a tool call shows
type TerminalToolCallContent struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	TerminalID string           `json:"terminalId"`
}

// JSON object of the terminal content with type "terminal"
func (c TerminalToolCallContent) MarshalJSON() ([]byte, error) {
	type plain TerminalToolCallContent
	return withTag(plain(c), "type", "terminal")
}

// Fills the terminal content from its JSON object, which must carry terminalId
func (c *TerminalToolCallContent) UnmarshalJSON(data []byte) error {
	type plain TerminalToolCallContent
	return decodeObject(data, (*plain)(c), "TerminalToolCallContent", "terminalId")
}

// File a tool call reads or changes, with a line in it
type ToolCallLocation struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
	Line *int             `json:"line,omitempty"`
	Path string           `json:"path"`
}

// Fills the location from its JSON object, which must carry path
func (l *ToolCallLocation) UnmarshalJSON(data []byte) error {
	type plain ToolCallLocation
	return decodeObject(data, (*plain)(l), "ToolCallLocation", "path")
}

// Tool call the agent started
type ToolCallStart struct {
	Meta    *jsonutil.Object  `json:"_meta,omitempty"`
	Content []ToolCallContent `json:"content,omitzero"`
	// read, edit, delete, move, search, execute, think, fetch, switch_mode or other
	Kind      *string            `json:"kind,omitempty"`
	Locations []ToolCallLocation `json:"locations,omitzero"`
	RawInput  any                `json:"rawInput,omitempty"`
	RawOutput any                `json:"rawOutput,omitempty"`
	// pending, in_progress, completed or failed
	Status     *string `json:"status,omitempty"`
	Title      string  `json:"title"`
	ToolCallID string  `json:"toolCallId"`
}

// JSON object of the start with sessionUpdate "tool_call"
func (t ToolCallStart) MarshalJSON() ([]byte, error) {
	type plain ToolCallStart
	return withTag(plain(t), "sessionUpdate", "tool_call")
}

// Fills the start from its JSON object, which must carry title and toolCallId
func (t *ToolCallStart) UnmarshalJSON(data []byte) error {
	type plain ToolCallStart
	var w struct {
		*plain
		Content   []json.RawMessage `json:"content"`
		RawInput  json.RawMessage   `json:"rawInput"`
		RawOutput json.RawMessage   `json:"rawOutput"`
	}
	w.plain = (*plain)(t)
	if err := decodeObject(data, &w, "ToolCallStart", "title", "toolCallId"); err != nil {
		return err
	}
	var err error
	t.Content, t.RawInput, t.RawOutput, err = toolCallParts(w.Content, w.RawInput, w.RawOutput)
	return err
}

// Change to the fields of a tool call already started, nil fields left as they were
type ToolCallProgress struct {
	Meta    *jsonutil.Object  `json:"_meta,omitempty"`
	Content []ToolCallContent `json:"content,omitzero"`
	// read, edit, delete, move, search, execute, think, fetch, switch_mode or other
	Kind      *string            `json:"kind,omitempty"`
	Locations []ToolCallLocation `json:"locations,omitzero"`
	RawInput  any                `json:"rawInput,omitempty"`
	RawOutput any                `json:"rawOutput,omitempty"`
	// pending, in_progress, completed or failed
	Status     *string `json:"status,omitempty"`
	Title      *string `json:"title,omitempty"`
	ToolCallID string  `json:"toolCallId"`
}

// JSON object of the progress with sessionUpdate "tool_call_update"
func (t ToolCallProgress) MarshalJSON() ([]byte, error) {
	type plain ToolCallProgress
	return withTag(plain(t), "sessionUpdate", "tool_call_update")
}

// Fills the progress from its JSON object, which must carry toolCallId
func (t *ToolCallProgress) UnmarshalJSON(data []byte) error {
	type plain ToolCallProgress
	var w struct {
		*plain
		Content   []json.RawMessage `json:"content"`
		RawInput  json.RawMessage   `json:"rawInput"`
		RawOutput json.RawMessage   `json:"rawOutput"`
	}
	w.plain = (*plain)(t)
	if err := decodeObject(data, &w, "ToolCallProgress", "toolCallId"); err != nil {
		return err
	}
	var err error
	t.Content, t.RawInput, t.RawOutput, err = toolCallParts(w.Content, w.RawInput, w.RawOutput)
	return err
}

// Tool call as a permission request shows it
type ToolCallUpdate struct {
	Meta    *jsonutil.Object  `json:"_meta,omitempty"`
	Content []ToolCallContent `json:"content,omitzero"`
	// read, edit, delete, move, search, execute, think, fetch, switch_mode or other
	Kind      *string            `json:"kind,omitempty"`
	Locations []ToolCallLocation `json:"locations,omitzero"`
	RawInput  any                `json:"rawInput,omitempty"`
	RawOutput any                `json:"rawOutput,omitempty"`
	// pending, in_progress, completed or failed
	Status     *string `json:"status,omitempty"`
	Title      *string `json:"title,omitempty"`
	ToolCallID string  `json:"toolCallId"`
}

// Fills the tool call from its JSON object, which must carry toolCallId
func (t *ToolCallUpdate) UnmarshalJSON(data []byte) error {
	type plain ToolCallUpdate
	var w struct {
		*plain
		Content   []json.RawMessage `json:"content"`
		RawInput  json.RawMessage   `json:"rawInput"`
		RawOutput json.RawMessage   `json:"rawOutput"`
	}
	w.plain = (*plain)(t)
	if err := decodeObject(data, &w, "ToolCallUpdate", "toolCallId"); err != nil {
		return err
	}
	var err error
	t.Content, t.RawInput, t.RawOutput, err = toolCallParts(w.Content, w.RawInput, w.RawOutput)
	return err
}

// Content, raw input and raw output of a tool call decoded from their JSON, the raw values with objects as *jsonutil.Object
func toolCallParts(content []json.RawMessage, rawInput, rawOutput json.RawMessage) ([]ToolCallContent, any, any, error) {
	items, err := decodeEach(content, "content", decodeToolCallContent)
	if err != nil {
		return nil, nil, nil, err
	}
	in, err := dynamic(rawInput)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("rawInput: %w", err)
	}
	out, err := dynamic(rawOutput)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("rawOutput: %w", err)
	}
	return items, in, out, nil
}

// Tool call content of a JSON object, the variant chosen by its type
func decodeToolCallContent(data []byte) (ToolCallContent, error) {
	kind, ok := tagOf(data, "type")
	if !ok {
		return nil, &MissingFieldError{Type: "ToolCallContent", Field: "type"}
	}
	var content ToolCallContent
	switch kind {
	case "content":
		content = new(ContentToolCallContent)
	case "diff":
		content = new(FileEditToolCallContent)
	case "terminal":
		content = new(TerminalToolCallContent)
	default:
		return nil, &UnknownVariantError{Union: "ToolCallContent", Key: "type", Value: kind}
	}
	if err := json.Unmarshal(data, content); err != nil {
		return nil, err
	}
	return content, nil
}
