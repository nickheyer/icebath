package openairesponses

import "encoding/json"

const (
	itemMessage            = "message"
	itemFunctionCall       = "function_call"
	itemFunctionCallOutput = "function_call_output"
	itemReasoning          = "reasoning"
	partInputText          = "input_text"
	partInputImage         = "input_image"
	partInputFile          = "input_file"
	partOutputText         = "output_text"
	partRefusal            = "refusal"
	partReasoningText      = "reasoning_text"
	partSummaryText        = "summary_text"
	annotationURL          = "url_citation"
	annotationFile         = "file_citation"
)

// CreateResponse body limited to the fields the neutral model drives
type request struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions,omitempty"`
	Input           []any          `json:"input"`
	Tools           []functionTool `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	Store           bool           `json:"store"`
	Include         []string       `json:"include"`
}

type functionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type toolChoiceFunction struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Message input item for user turns and replayed assistant text
type messageItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []any  `json:"content"`
	Status  string `json:"status,omitempty"`
}

type inputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type inputImage struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

type inputFile struct {
	Type     string `json:"type"`
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

type outputText struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
	Logprobs    []json.RawMessage `json:"logprobs"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}

type reasoningItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Summary          []summaryPart `json:"summary"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Replay token carried in Thinking.Signature so a reasoning item can be sent back
type reasoningSignature struct {
	ID               string `json:"id"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

// Response object fields read by the decoder, output items kept raw for verbatim replay
type response struct {
	ID                string            `json:"id"`
	Model             string            `json:"model"`
	Status            string            `json:"status"`
	Error             *responseError    `json:"error"`
	IncompleteDetails *incompleteInfo   `json:"incomplete_details"`
	Output            []json.RawMessage `json:"output"`
	Usage             *usage            `json:"usage"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type incompleteInfo struct {
	Reason string `json:"reason"`
}

// Output item union flattened over message, function_call and reasoning fields
type outputItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Content          []outputPart    `json:"content"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	Summary          []summaryPart   `json:"summary"`
	EncryptedContent string          `json:"encrypted_content"`
	Raw              json.RawMessage `json:"-"`
}

type outputPart struct {
	Type        string       `json:"type"`
	Text        string       `json:"text"`
	Refusal     string       `json:"refusal"`
	Annotations []annotation `json:"annotations"`
}

// Annotation union flattened over the url_citation and file_citation shapes
type annotation struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	FileID     string `json:"file_id"`
	Filename   string `json:"filename"`
	Index      int    `json:"index"`
}

type usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// ResponseStreamEvent union flattened over every field the decoder reads
type streamEvent struct {
	Type         string          `json:"type"`
	Response     *response       `json:"response"`
	Item         json.RawMessage `json:"item"`
	ItemID       string          `json:"item_id"`
	ContentIndex int             `json:"content_index"`
	SummaryIndex int             `json:"summary_index"`
	Part         *outputPart     `json:"part"`
	Annotation   *annotation     `json:"annotation"`
	Delta        string          `json:"delta"`
	Text         string          `json:"text"`
	Refusal      string          `json:"refusal"`
	Arguments    string          `json:"arguments"`
	Code         string          `json:"code"`
	Message      string          `json:"message"`
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}
