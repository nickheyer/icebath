// Gemini generateContent wire protocol with paths relative to the generativelanguage.googleapis.com origin
package googlegenerativeai

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/sse"
	"github.com/nickheyer/icebath/pkg/protocol"
)

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.GoogleGenerativeAI }

type request struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Tools             []tool            `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

// One Part with its data union and the thought metadata that may ride on any kind
type part struct {
	Text                *string           `json:"text,omitempty"`
	InlineData          *blob             `json:"inlineData,omitempty"`
	FunctionCall        *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse    *functionResponse `json:"functionResponse,omitempty"`
	FileData            json.RawMessage   `json:"fileData,omitempty"`
	ExecutableCode      json.RawMessage   `json:"executableCode,omitempty"`
	CodeExecutionResult json.RawMessage   `json:"codeExecutionResult,omitempty"`
	ToolCall            json.RawMessage   `json:"toolCall,omitempty"`
	ToolResponse        json.RawMessage   `json:"toolResponse,omitempty"`
	Thought             bool              `json:"thought,omitempty"`
	ThoughtSignature    string            `json:"thoughtSignature,omitempty"`
}

type blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type functionResponse struct {
	ID       string                 `json:"id,omitempty"`
	Name     string                 `json:"name"`
	Response map[string]string      `json:"response"`
	Parts    []functionResponsePart `json:"parts,omitempty"`
}

type functionResponsePart struct {
	InlineData blob `json:"inlineData"`
}

type tool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type functionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
}

type toolConfig struct {
	FunctionCallingConfig functionCallingConfig `json:"functionCallingConfig"`
}

type functionCallingConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type generationConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// GenerateContentResponse, also the shape of every streamed chunk
type response struct {
	Candidates     []candidate     `json:"candidates"`
	PromptFeedback *promptFeedback `json:"promptFeedback"`
	UsageMetadata  *usageMetadata  `json:"usageMetadata"`
	ModelVersion   string          `json:"modelVersion"`
	ResponseID     string          `json:"responseId"`
	Error          *apiError       `json:"error"`
}

// Candidate with the rating and scoring fields the decoder accepts without acting on
type candidate struct {
	Content           content            `json:"content"`
	FinishReason      string             `json:"finishReason"`
	FinishMessage     string             `json:"finishMessage"`
	SafetyRatings     json.RawMessage    `json:"safetyRatings"`
	AvgLogprobs       float64            `json:"avgLogprobs"`
	CitationMetadata  *citationMetadata  `json:"citationMetadata"`
	GroundingMetadata *groundingMetadata `json:"groundingMetadata"`
}

// Citation list under the Gemini API name and the Vertex name
type citationMetadata struct {
	CitationSources []citationSource `json:"citationSources"`
	Citations       []citationSource `json:"citations"`
}

type citationSource struct {
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
	URI        string `json:"uri"`
	Title      string `json:"title"`
	License    string `json:"license"`
}

type groundingMetadata struct {
	GroundingChunks   []groundingChunk   `json:"groundingChunks"`
	GroundingSupports []groundingSupport `json:"groundingSupports"`
}

type groundingChunk struct {
	Web              *groundingSource `json:"web"`
	RetrievedContext *groundingSource `json:"retrievedContext"`
}

type groundingSource struct {
	URI   string `json:"uri"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type groundingSupport struct {
	Segment               *segment `json:"segment"`
	GroundingChunkIndices []int    `json:"groundingChunkIndices"`
}

type segment struct {
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
	Text       string `json:"text"`
}

type promptFeedback struct {
	BlockReason string `json:"blockReason"`
}

type usageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
}

// google.rpc.Status as mapped to HTTP JSON by AIP-193
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

type errorEnvelope struct {
	Error *apiError `json:"error"`
}

const (
	roleUser  = "user"
	roleModel = "model"
	// Neutral names for the executableCode and toolCall part kinds the model runs itself
	nameCodeExecution = "code_execution"
	nameToolCall      = "tool_call"
)

func (Protocol) Encode(req protocol.Request) (protocol.Call, error) {
	if req.Model == "" {
		return protocol.Call{}, fmt.Errorf("google-generative-ai: model is required")
	}
	body := request{}
	if req.System != "" {
		body.SystemInstruction = &content{Parts: []part{{Text: ptr(req.System)}}}
	}
	var calls []protocol.ToolCall
	for i, msg := range req.Messages {
		c, keep, err := encodeMessage(msg, calls)
		if err != nil {
			return protocol.Call{}, fmt.Errorf("google-generative-ai: message %d: %w", i, err)
		}
		if msg.Role == protocol.RoleAssistant {
			calls = toolCalls(msg)
		}
		if keep {
			body.Contents = append(body.Contents, c)
		}
	}
	if len(req.Tools) > 0 {
		decls := make([]functionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			if len(t.Parameters) == 0 {
				return protocol.Call{}, fmt.Errorf("google-generative-ai: tool %q has no parameters schema", t.Name)
			}
			decls = append(decls, functionDeclaration{Name: t.Name, Description: t.Description, ParametersJSONSchema: t.Parameters})
		}
		body.Tools = []tool{{FunctionDeclarations: decls}}
	}
	cfg, err := encodeToolChoice(req.ToolChoice)
	if err != nil {
		return protocol.Call{}, err
	}
	body.ToolConfig = cfg
	if req.MaxTokens > 0 || req.Temperature != nil || len(req.StopSequences) > 0 {
		body.GenerationConfig = &generationConfig{MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature, StopSequences: req.StopSequences}
	}
	raw, err := protocol.MarshalBody(body, req.Extra)
	if err != nil {
		return protocol.Call{}, err
	}
	call := protocol.Call{Method: http.MethodPost, Path: path(req.Model, req.Stream), Header: http.Header{}, Body: raw}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", "application/json")
	if req.Stream {
		call.Query = url.Values{"alt": {"sse"}}
		call.Header.Set("Accept", "text/event-stream")
	}
	return call, nil
}

func toolCalls(msg protocol.Message) []protocol.ToolCall {
	var out []protocol.ToolCall
	for _, p := range msg.Content {
		if c, ok := p.(protocol.ToolCall); ok {
			out = append(out, c)
		}
	}
	return out
}

// Builds the method path for a bare model id or an already qualified resource name
func path(model string, stream bool) string {
	resource := model
	if !strings.Contains(model, "/") {
		resource = "models/" + url.PathEscape(model)
	}
	if stream {
		return "/v1beta/" + resource + ":streamGenerateContent"
	}
	return "/v1beta/" + resource + ":generateContent"
}

func encodeToolChoice(tc protocol.ToolChoice) (*toolConfig, error) {
	cfg := functionCallingConfig{}
	switch tc.Mode {
	case "":
		return nil, nil
	case protocol.ToolChoiceAuto:
		cfg.Mode = "AUTO"
	case protocol.ToolChoiceNone:
		cfg.Mode = "NONE"
	case protocol.ToolChoiceRequired:
		cfg.Mode = "ANY"
	case protocol.ToolChoiceTool:
		if tc.Name == "" {
			return nil, fmt.Errorf("google-generative-ai: tool choice names no tool")
		}
		cfg.Mode = "ANY"
		cfg.AllowedFunctionNames = []string{tc.Name}
	default:
		return nil, fmt.Errorf("google-generative-ai: unknown tool choice mode %q", tc.Mode)
	}
	return &toolConfig{FunctionCallingConfig: cfg}, nil
}

// Maps one turn to a content, reporting false when the replay policy left no parts to send
func encodeMessage(msg protocol.Message, calls []protocol.ToolCall) (content, bool, error) {
	if len(msg.Content) == 0 {
		return content{}, false, fmt.Errorf("empty message")
	}
	switch msg.Role {
	case protocol.RoleUser:
		parts, err := encodeUser(msg.Content)
		return content{Role: roleUser, Parts: parts}, true, err
	case protocol.RoleAssistant:
		parts, err := encodeAssistant(msg.Content)
		return content{Role: roleModel, Parts: parts}, len(parts) > 0, err
	case protocol.RoleTool:
		parts, err := encodeToolResults(msg.Content, calls)
		return content{Role: roleUser, Parts: parts}, true, err
	}
	return content{}, false, fmt.Errorf("unknown role %q", msg.Role)
}

func encodeUser(parts []protocol.Part) ([]part, error) {
	out := make([]part, 0, len(parts))
	for _, p := range parts {
		switch p := p.(type) {
		case protocol.Text:
			out = append(out, part{Text: ptr(p.Text)})
		case protocol.Image:
			ip, err := encodeImage(p)
			if err != nil {
				return nil, err
			}
			out = append(out, ip)
		case protocol.Document:
			dp, err := encodeDocument(p)
			if err != nil {
				return nil, err
			}
			out = append(out, dp)
		default:
			return nil, fmt.Errorf("user message cannot carry %T", p)
		}
	}
	return out, nil
}

// Emits model parts, folding a replayable signature only Thinking back onto the Text before it
func encodeAssistant(parts []protocol.Part) ([]part, error) {
	out := make([]part, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		switch p := parts[i].(type) {
		case protocol.Text:
			wp := part{Text: ptr(p.Text)}
			if i+1 < len(parts) {
				if th, ok := parts[i+1].(protocol.Thinking); ok && signatureOnly(th) && replayable(th.Origin) {
					wp.ThoughtSignature = th.Signature
					i++
				}
			}
			out = append(out, wp)
		case protocol.Thinking:
			if p.Redacted || !replayable(p.Origin) {
				continue
			}
			if signatureOnly(p) {
				out = append(out, part{Text: ptr(""), ThoughtSignature: p.Signature})
				continue
			}
			out = append(out, part{Thought: true, Text: ptr(p.Text), ThoughtSignature: p.Signature})
		case protocol.ToolCall:
			fc := &functionCall{ID: p.ID, Name: p.Name}
			args, err := argsJSON(p.Args)
			if err != nil {
				return nil, fmt.Errorf("assistant part %d: %w", i, err)
			}
			if len(p.Args) > 0 {
				fc.Args = args
			}
			wp := part{FunctionCall: fc}
			if replayable(p.Origin) {
				wp.ThoughtSignature = p.Signature
			}
			out = append(out, wp)
		case protocol.ServerToolCall:
			if !replayable(p.Origin) {
				continue
			}
			wp, err := encodeServerToolCall(p)
			if err != nil {
				return nil, fmt.Errorf("assistant part %d: %w", i, err)
			}
			out = append(out, wp)
		case protocol.ServerToolResult:
			if !replayable(p.Origin) {
				continue
			}
			wp, err := encodeServerToolResult(p)
			if err != nil {
				return nil, fmt.Errorf("assistant part %d: %w", i, err)
			}
			out = append(out, wp)
		case protocol.Image:
			ip, err := encodeImage(p)
			if err != nil {
				return nil, err
			}
			out = append(out, ip)
		default:
			return nil, fmt.Errorf("assistant message cannot carry %T", p)
		}
	}
	return out, nil
}

func replayable(origin protocol.ID) bool {
	return protocol.Replayable(origin, protocol.GoogleGenerativeAI)
}

func signatureOnly(th protocol.Thinking) bool {
	return th.Text == "" && th.Signature != "" && !th.Redacted
}

// Restores the executableCode or toolCall part a vendor side call was decoded from
func encodeServerToolCall(c protocol.ServerToolCall) (part, error) {
	args, err := protocol.ParseArgs(string(c.Args))
	if err != nil {
		return part{}, fmt.Errorf("server tool call %s: %w", c.Name, err)
	}
	switch c.Name {
	case nameCodeExecution:
		return part{ExecutableCode: args}, nil
	case nameToolCall:
		return part{ToolCall: args}, nil
	}
	return part{}, fmt.Errorf("server tool call %q has no part kind", c.Name)
}

// Restores the codeExecutionResult or toolResponse part a vendor side result was decoded from
func encodeServerToolResult(r protocol.ServerToolResult) (part, error) {
	if len(r.Content) == 0 {
		return part{}, fmt.Errorf("server tool result %s has no content", r.Name)
	}
	switch r.Name {
	case nameCodeExecution:
		return part{CodeExecutionResult: r.Content}, nil
	case nameToolCall:
		return part{ToolResponse: r.Content}, nil
	}
	return part{}, fmt.Errorf("server tool result %q has no part kind", r.Name)
}

// Names each result from the matching call of the previous model turn when the result carries none
func encodeToolResults(parts []protocol.Part, calls []protocol.ToolCall) ([]part, error) {
	out := make([]part, 0, len(parts))
	for i, p := range parts {
		tr, ok := p.(protocol.ToolResult)
		if !ok {
			return nil, fmt.Errorf("tool message cannot carry %T", p)
		}
		name, err := resultName(tr, i, calls)
		if err != nil {
			return nil, fmt.Errorf("tool result %d: %w", i, err)
		}
		fr := &functionResponse{ID: tr.CallID, Name: name}
		var texts []string
		for _, c := range tr.Content {
			switch c := c.(type) {
			case protocol.Text:
				texts = append(texts, c.Text)
			case protocol.Image:
				ip, err := encodeImage(c)
				if err != nil {
					return nil, err
				}
				fr.Parts = append(fr.Parts, functionResponsePart{InlineData: *ip.InlineData})
			case protocol.Document:
				dp, err := encodeDocument(c)
				if err != nil {
					return nil, err
				}
				fr.Parts = append(fr.Parts, functionResponsePart{InlineData: *dp.InlineData})
			default:
				return nil, fmt.Errorf("tool result %d cannot carry %T", i, c)
			}
		}
		key := "result"
		if tr.IsError {
			key = "error"
		}
		fr.Response = map[string]string{key: strings.Join(texts, "\n")}
		out = append(out, part{FunctionResponse: fr})
	}
	return out, nil
}

// Resolves the function name by id, then by position among the previous calls
func resultName(tr protocol.ToolResult, index int, calls []protocol.ToolCall) (string, error) {
	if tr.Name != "" {
		return tr.Name, nil
	}
	if tr.CallID != "" {
		for _, c := range calls {
			if c.ID == tr.CallID {
				return c.Name, nil
			}
		}
		return "", fmt.Errorf("no function call %s to name the result", tr.CallID)
	}
	if index < len(calls) {
		return calls[index].Name, nil
	}
	return "", fmt.Errorf("name is required")
}

func encodeImage(img protocol.Image) (part, error) {
	if img.MediaType == "" {
		return part{}, fmt.Errorf("image has no media type")
	}
	if len(img.Data) == 0 {
		return part{}, fmt.Errorf("image has no data")
	}
	return part{InlineData: &blob{MimeType: img.MediaType, Data: base64.StdEncoding.EncodeToString(img.Data)}}, nil
}

func encodeDocument(d protocol.Document) (part, error) {
	if d.MediaType == "" {
		return part{}, fmt.Errorf("document %q has no media type", d.Name)
	}
	if len(d.Data) == 0 {
		return part{}, fmt.Errorf("document %q has no data", d.Name)
	}
	return part{InlineData: &blob{MimeType: d.MediaType, Data: base64.StdEncoding.EncodeToString(d.Data)}}, nil
}

func ptr(s string) *string { return &s }

// Validates function call arguments as an object and compacts them for byte stable replay
func argsJSON(raw json.RawMessage) (json.RawMessage, error) {
	args, err := protocol.ParseArgs(string(raw))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, args); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var transientStatuses = map[string]bool{
	"RESOURCE_EXHAUSTED": true,
	"UNAVAILABLE":        true,
	"DEADLINE_EXCEEDED":  true,
	"INTERNAL":           true,
}

func (Protocol) Decode(r io.Reader) (protocol.Response, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return protocol.Response{}, err
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return protocol.Response{}, fmt.Errorf("google-generative-ai: response: %w", err)
	}
	if resp.Error != nil {
		return protocol.Response{}, wireError(resp.Error)
	}
	st := newState()
	st.b.Start(resp.ResponseID, resp.ModelVersion)
	if err := st.apply(resp); err != nil {
		return protocol.Response{}, err
	}
	if !st.blocked && len(resp.Candidates) == 0 {
		return protocol.Response{}, fmt.Errorf("google-generative-ai: response has no candidates")
	}
	if err := st.finish(); err != nil {
		return protocol.Response{}, err
	}
	return st.b.Response, nil
}

// Yields neutral events from alt=sse chunks until the chunk carrying finishReason or a block
func (Protocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		br := bufio.NewReader(r)
		if startsObject(br) {
			body, err := io.ReadAll(br)
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			yield(protocol.Event{}, rawError(body))
			return
		}
		st := newState()
		for ev, err := range sse.Read(br) {
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			var chunk response
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				yield(protocol.Event{}, fmt.Errorf("google-generative-ai: stream chunk: %w", err))
				return
			}
			if chunk.Error != nil {
				yield(protocol.Event{}, wireError(chunk.Error))
				return
			}
			st.b.Start(chunk.ResponseID, chunk.ModelVersion)
			if err := st.apply(chunk); err != nil {
				yield(protocol.Event{}, err)
				return
			}
			if st.blocked {
				break
			}
			for _, ev := range st.b.Events() {
				if !yield(ev, nil) {
					return
				}
			}
		}
		if st.finishReason == "" {
			yield(protocol.Event{}, fmt.Errorf("google-generative-ai: stream ended before finishReason"))
			return
		}
		if err := st.finish(); err != nil {
			yield(protocol.Event{}, err)
			return
		}
		for _, ev := range st.b.Events() {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Reports whether the body opens with a JSON object rather than event stream fields
func startsObject(br *bufio.Reader) bool {
	head, _ := br.Peek(64)
	trimmed := bytes.TrimLeft(head, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func (Protocol) DecodeError(status int, body []byte) *protocol.Error {
	e := &protocol.Error{Status: status, Transient: transientStatus(status)}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error != nil {
		e.Type, e.Message = env.Error.Status, env.Error.Message
		e.Transient = e.Transient || transientStatuses[e.Type]
		return e
	}
	e.Message = strings.TrimSpace(string(body))
	return e
}

// Reports statuses that denote a timeout, conflict, throttle or server failure
func transientStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}

func wireError(e *apiError) *protocol.Error {
	transient := e.Code == 429 || e.Code == 500 || e.Code == 503 || e.Code == 504 || transientStatuses[e.Status]
	return &protocol.Error{Status: e.Code, Type: e.Status, Message: e.Message, Transient: transient}
}

// Parses a bare JSON body in place of a stream the way the official SDKs do
func rawError(body []byte) error {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return fmt.Errorf("google-generative-ai: invalid stream body: %s", bytes.TrimSpace(body))
	}
	return wireError(env.Error)
}

// Folds chunk parts into one Builder, keeping one block open across chunk boundaries
type state struct {
	b            protocol.Builder
	open         string
	kind         string
	n            int
	finishReason string
	blocked      bool
	hasCalls     bool
	usage        protocol.Usage
	usageSeen    bool
}

func newState() *state {
	return &state{b: protocol.Builder{Origin: protocol.GoogleGenerativeAI}}
}

func (st *state) apply(resp response) error {
	if resp.UsageMetadata != nil {
		if u := mapUsage(*resp.UsageMetadata); !st.usageSeen || u != st.usage {
			st.usage, st.usageSeen = u, true
			st.b.Usage(u)
		}
	}
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		st.blocked = true
		st.finishReason = resp.PromptFeedback.BlockReason
		return nil
	}
	if len(resp.Candidates) == 0 {
		return nil
	}
	cand := resp.Candidates[0]
	for i, p := range cand.Content.Parts {
		if err := st.applyPart(p); err != nil {
			return fmt.Errorf("google-generative-ai: part %d: %w", i, err)
		}
	}
	if err := st.applyCitations(cand); err != nil {
		return fmt.Errorf("google-generative-ai: %w", err)
	}
	if cand.FinishReason != "" {
		st.finishReason = cand.FinishReason
	}
	return nil
}

// Takes cached tokens out of the prompt count and folds thought tokens into the output count
func mapUsage(u usageMetadata) protocol.Usage {
	return protocol.Usage{
		Input:     u.PromptTokenCount - u.CachedContentTokenCount,
		Output:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CacheRead: u.CachedContentTokenCount,
		Reasoning: u.ThoughtsTokenCount,
	}
}

func (st *state) applyPart(p part) error {
	switch {
	case len(p.FileData) > 0:
		return fmt.Errorf("unsupported part kind fileData")
	case len(p.ExecutableCode) > 0:
		return st.serverToolCall("", nameCodeExecution, p.ExecutableCode)
	case len(p.CodeExecutionResult) > 0:
		return st.serverToolResult("", nameCodeExecution, p.CodeExecutionResult)
	case len(p.ToolCall) > 0:
		return st.serverToolCall(rawID(p.ToolCall), nameToolCall, p.ToolCall)
	case len(p.ToolResponse) > 0:
		return st.serverToolResult(rawID(p.ToolResponse), nameToolCall, p.ToolResponse)
	case p.FunctionCall != nil:
		st.hasCalls = true
		args, err := argsJSON(p.FunctionCall.Args)
		if err != nil {
			return err
		}
		if err := st.closeOpen(); err != nil {
			return err
		}
		key := st.next("call")
		st.b.OpenToolCall(key, p.FunctionCall.ID, p.FunctionCall.Name)
		if err := st.b.Delta(key, string(args)); err != nil {
			return err
		}
		if p.ThoughtSignature != "" {
			if err := st.b.Signature(key, p.ThoughtSignature); err != nil {
				return err
			}
		}
		return st.b.Close(key)
	case p.InlineData != nil:
		return st.image(*p.InlineData)
	case p.Thought:
		if err := st.ensure("thinking"); err != nil {
			return err
		}
		if err := st.delta(text(p)); err != nil {
			return err
		}
		if p.ThoughtSignature != "" {
			return st.b.Signature(st.open, p.ThoughtSignature)
		}
		return nil
	}
	if err := st.ensure("text"); err != nil {
		return err
	}
	if err := st.delta(text(p)); err != nil {
		return err
	}
	if p.ThoughtSignature == "" {
		return nil
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	key := st.next("signature")
	st.b.OpenThinking(key, false)
	if err := st.b.Signature(key, p.ThoughtSignature); err != nil {
		return err
	}
	return st.b.Close(key)
}

// Delivers a part the model executed itself as one complete server tool call
func (st *state) serverToolCall(id, name string, raw json.RawMessage) error {
	args, err := argsJSON(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	key := st.next("server")
	st.b.OpenServerToolCall(key, id, name)
	if err := st.b.Delta(key, string(args)); err != nil {
		return err
	}
	return st.b.Close(key)
}

func (st *state) serverToolResult(id, name string, raw json.RawMessage) error {
	content, err := argsJSON(raw)
	if err != nil {
		return fmt.Errorf("%s result: %w", name, err)
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	st.b.AddServerToolResult(protocol.ServerToolResult{CallID: id, Name: name, Content: content})
	return nil
}

func rawID(raw json.RawMessage) string {
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.ID
}

// Appends a generated image as a complete part after closing the block before it
func (st *state) image(b blob) error {
	data, err := base64.StdEncoding.DecodeString(b.Data)
	if err != nil {
		return fmt.Errorf("inlineData: %w", err)
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	st.b.Response.Content = append(st.b.Response.Content, protocol.Image{MediaType: b.MimeType, Data: data})
	return nil
}

// Attaches citation sources and grounding supports to the candidate text
func (st *state) applyCitations(cand candidate) error {
	var cites []protocol.Citation
	if cm := cand.CitationMetadata; cm != nil {
		for _, s := range append(cm.CitationSources, cm.Citations...) {
			cites = append(cites, protocol.Citation{Title: s.Title, URL: s.URI, Start: s.StartIndex, End: s.EndIndex})
		}
	}
	if gm := cand.GroundingMetadata; gm != nil {
		for _, sup := range gm.GroundingSupports {
			for _, idx := range sup.GroundingChunkIndices {
				if idx < 0 || idx >= len(gm.GroundingChunks) {
					return fmt.Errorf("grounding chunk index %d out of range", idx)
				}
				src := gm.GroundingChunks[idx].source()
				c := protocol.Citation{Title: src.Title, URL: src.URI}
				if sup.Segment != nil {
					c.Start, c.End, c.Cited = sup.Segment.StartIndex, sup.Segment.EndIndex, sup.Segment.Text
				}
				cites = append(cites, c)
			}
		}
	}
	if len(cites) == 0 {
		return nil
	}
	if st.open != "" && st.kind == "text" {
		for _, c := range cites {
			if err := st.b.Cite(st.open, c); err != nil {
				return err
			}
		}
		return nil
	}
	for i := len(st.b.Response.Content) - 1; i >= 0; i-- {
		if t, ok := st.b.Response.Content[i].(protocol.Text); ok {
			t.Citations = append(t.Citations, cites...)
			st.b.Response.Content[i] = t
			return nil
		}
	}
	if err := st.ensure("text"); err != nil {
		return err
	}
	for _, c := range cites {
		if err := st.b.Cite(st.open, c); err != nil {
			return err
		}
	}
	return st.closeOpen()
}

func (g groundingChunk) source() groundingSource {
	if g.Web != nil {
		return *g.Web
	}
	if g.RetrievedContext != nil {
		return *g.RetrievedContext
	}
	return groundingSource{}
}

func text(p part) string {
	if p.Text == nil {
		return ""
	}
	return *p.Text
}

// Opens a block of kind unless one is already receiving fragments
func (st *state) ensure(kind string) error {
	if st.open != "" && st.kind == kind {
		return nil
	}
	if err := st.closeOpen(); err != nil {
		return err
	}
	key := st.next(kind)
	if kind == "thinking" {
		st.b.OpenThinking(key, false)
	} else {
		st.b.OpenText(key)
	}
	st.open, st.kind = key, kind
	return nil
}

func (st *state) delta(s string) error {
	if s == "" {
		return nil
	}
	return st.b.Delta(st.open, s)
}

func (st *state) closeOpen() error {
	if st.open == "" {
		return nil
	}
	key := st.open
	st.open, st.kind = "", ""
	return st.b.Close(key)
}

func (st *state) next(kind string) string {
	st.n++
	return kind + "-" + strconv.Itoa(st.n)
}

func (st *state) finish() error {
	if err := st.closeOpen(); err != nil {
		return err
	}
	if st.blocked {
		return st.b.Finish(protocol.StopContentFilter, st.finishReason, st.usage)
	}
	return st.b.Finish(stopReason(st.finishReason, st.hasCalls), st.finishReason, st.usage)
}

// Maps a FinishReason value onto the neutral set, with tool use detected from parts
func stopReason(reason string, hasCalls bool) protocol.StopReason {
	switch reason {
	case "STOP":
		if hasCalls {
			return protocol.StopToolUse
		}
		return protocol.StopEnd
	case "MAX_TOKENS", "CONTINUATION":
		return protocol.StopLength
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "ESCALATION", "PUP_LIMITED_DISABLED":
		return protocol.StopContentFilter
	}
	return protocol.StopOther
}
