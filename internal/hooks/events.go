package hooks

import (
	"cmp"
	"context"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Turn a firing happens in: the session and the task it runs as, the turn and the model request it is at, the surface it is shown on and the
// folder it works in. The agent puts it on the context of each turn, so whatever fires inside the turn names it
type Turn struct {
	Session  string
	Task     string
	ID       string
	Request  string
	Platform string
	Cwd      string
}

type turnKey struct{}

type callKey struct{}

// Context carrying the turn
func WithTurn(ctx context.Context, turn Turn) context.Context {
	return context.WithValue(ctx, turnKey{}, turn)
}

// Turn the context carries, empty outside one
func TurnOf(ctx context.Context) Turn {
	turn, _ := ctx.Value(turnKey{}).(Turn)
	return turn
}

// Context carrying the tool call that runs, so what fires inside it names the call
func WithCall(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callKey{}, id)
}

// Tool call the context carries, empty outside one
func CallOf(ctx context.Context) string {
	id, _ := ctx.Value(callKey{}).(string)
	return id
}

// Tool call a point fires for
type Call struct {
	Tool    string         `json:"tool_name"`
	Args    map[string]any `json:"args"`
	Task    string         `json:"task_id"`
	Session string         `json:"session_id"`
	ID      string         `json:"tool_call_id"`
	Turn    string         `json:"turn_id"`
	Request string         `json:"api_request_id"`
}

func (c Call) call() string { return cmp.Or(c.ID, c.Turn) }

// A tool call about to run, which a hook may block, rewrite or hold for the user's approval
type ToolCall struct {
	Call
	Trace []any `json:"middleware_trace"`
}

func (ToolCall) Point() string { return PreToolCall }

// What came of a tool call
type Outcome struct {
	Call
	Result string `json:"result"`
	// Milliseconds it took
	Duration int64 `json:"duration_ms"`
	// ok, error or blocked
	Status string `json:"status"`
	// tool_error or plugin_block, and what went wrong; nil for a call that went well
	ErrorType    *string `json:"error_type"`
	ErrorMessage *string `json:"error_message"`
}

// A tool call that finished
type ToolDone struct {
	Outcome
	Trace []any `json:"middleware_trace"`
}

func (ToolDone) Point() string { return PostToolCall }

// The result of a finished tool call, which a hook may replace before the model reads it
type ToolResult struct {
	Outcome
}

func (ToolResult) Point() string { return TransformToolResult }

// Output of a terminal command, which a hook may replace before it is cut to fit and the model reads it
type TerminalOutput struct {
	Command string `json:"command"`
	Output  string `json:"output"`
	// Exit code, nil while a background process still runs
	ReturnCode *int   `json:"returncode"`
	Task       string `json:"task_id"`
	// Backend the command ran on, empty for a background process
	Backend string `json:"env_type"`
	ID      string `json:"tool_call_id"`
}

func (TerminalOutput) Point() string { return TransformTerminalOutput }

func (t TerminalOutput) call() string { return t.ID }

// A user message about to start a turn, which a hook may refuse or give context to
type Prompt struct {
	Session string `json:"session_id"`
	Task    string `json:"task_id"`
	Turn    string `json:"turn_id"`
	// What the user sent
	Message string `json:"user_message"`
	// The conversation, the message last
	History []map[string]any `json:"conversation_history"`
	First   bool             `json:"is_first_turn"`
	Model   string           `json:"model"`
	// Surface the session is shown on
	Platform string `json:"platform"`
	// Session of the agent that delegated, nil for a session of its own
	Parent *string `json:"parent_session_id"`
	// Who sent it on a messaging platform, nil elsewhere
	Sender *string `json:"sender_id"`
}

func (Prompt) Point() string { return PreLLMCall }

func (p Prompt) call() string { return p.Turn }

// The reply a turn ends with, which a hook may rewrite
type Reply struct {
	Text     string `json:"response_text"`
	Session  string `json:"session_id"`
	Model    string `json:"model"`
	Platform string `json:"platform"`
	Turn     string `json:"turn_id"`
}

func (Reply) Point() string { return TransformLLMOutput }

func (r Reply) call() string { return r.Turn }

// A turn that answered
type Answered struct {
	Session  string           `json:"session_id"`
	Task     string           `json:"task_id"`
	Turn     string           `json:"turn_id"`
	Message  string           `json:"user_message"`
	Response string           `json:"assistant_response"`
	History  []map[string]any `json:"conversation_history"`
	Model    string           `json:"model"`
	Platform string           `json:"platform"`
	// Lease a consented user plugin redeems to ask a side question of the turn, nil while no such plugin is loaded
	Token *string `json:"side_question_token"`
}

func (Answered) Point() string { return PostLLMCall }

func (a Answered) call() string { return a.Turn }

// Where a reply streams, which every stream event names
type Stream struct {
	Turn string `json:"turn_id"`
	// Model calls of the turn so far
	Iteration int    `json:"iteration"`
	Session   string `json:"session_id"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Surface   string `json:"surface"`
}

// A reply that began to stream
type Started struct {
	Stream
}

func (Started) Point() string { return StreamStart }

// A piece of a streaming reply: its text, or its reasoning when hooks.stream_reasoning lets hooks see it
type Delta struct {
	Stream
	Text string `json:"delta"`
	// text or reasoning
	Kind string `json:"kind"`
}

func (Delta) Point() string { return StreamDelta }

// A reply that ended streaming
type Ended struct {
	Stream
	Text     string `json:"final_text"`
	Finished bool   `json:"finished"`
	// Why it ended early, nil when it did not
	Error *string `json:"error"`
}

func (Ended) Point() string { return StreamEnd }

// Text the agent said while it went on working, such as before its tool calls
type Interim struct {
	Stream
	Text string `json:"text"`
	// The user saw it stream already
	Streamed bool `json:"already_streamed"`
}

func (Interim) Point() string { return InterimMessage }

// A turn that changed code about to end, which a hook may send back to check its work
type Verify struct {
	Session  string `json:"session_id"`
	Platform string `json:"platform"`
	Model    string `json:"model"`
	// The session works on code
	Coding bool `json:"coding"`
	// Times hooks sent this turn back so far
	Attempt int    `json:"attempt"`
	Final   string `json:"final_response"`
	// Files the turn's writes landed in
	Changed []string `json:"changed_paths"`
}

func (Verify) Point() string { return PreVerify }

// The model request a point fires for
type Request struct {
	Task     string `json:"task_id"`
	Turn     string `json:"turn_id"`
	ID       string `json:"api_request_id"`
	Session  string `json:"session_id"`
	Platform string `json:"platform"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Mode     string `json:"api_mode"`
	// Model calls of the turn, this one counted
	Count int `json:"api_call_count"`
	// When it was sent, in seconds since the epoch
	Started float64 `json:"started_at"`
}

func (r Request) call() string { return r.Turn }

// A model request about to be sent
type APIRequest struct {
	Request
	Message  string           `json:"user_message"`
	History  []map[string]any `json:"conversation_history"`
	Retries  int              `json:"retry_count"`
	Messages []map[string]any `json:"request_messages"`
	System   string           `json:"system_prompt"`
	// Messages, tools, rough tokens and characters of the request
	MessageCount int `json:"message_count"`
	ToolCount    int `json:"tool_count"`
	Tokens       int `json:"approx_input_tokens"`
	Chars        int `json:"request_char_count"`
	// Most tokens the reply may take, nil for the provider's limit
	MaxTokens *int  `json:"max_tokens"`
	Trace     []any `json:"middleware_trace"`
	// The request as it is sent, secrets masked and cut to hooks.payload_max_chars
	Body any `json:"request"`
}

func (APIRequest) Point() string { return PreAPIRequest }

// Tokens a model call took
type Usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
	Requests   int64 `json:"request_count"`
	Prompt     int64 `json:"prompt_tokens"`
	Total      int64 `json:"total_tokens"`
}

// A model request that was answered
type APIResponse struct {
	Request
	// Seconds it took, and when it ended and its first piece came, in seconds since the epoch
	Duration  float64  `json:"api_duration"`
	Finished  float64  `json:"ended_at"`
	FirstAt   *float64 `json:"first_chunk_at"`
	Finish    *string  `json:"finish_reason"`
	Messages  int      `json:"message_count"`
	Answering string   `json:"response_model"`
	// The reply as it came, secrets masked and cut to hooks.payload_max_chars
	Body  any    `json:"response"`
	Usage *Usage `json:"usage"`
	// Tokens the model takes in at once, nil when not known
	Window    *int           `json:"context_length"`
	Assistant map[string]any `json:"assistant_message"`
	Chars     int            `json:"assistant_content_chars"`
	ToolCalls int            `json:"assistant_tool_call_count"`
	// What the reference models of a mixture of agents answered, nil for a request of one model
	References any `json:"moa_references"`
}

func (APIResponse) Point() string { return PostAPIRequest }

// What went wrong with a request
type Failure struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// A model request that failed
type APIError struct {
	Request
	Duration float64 `json:"api_duration"`
	Finished float64 `json:"ended_at"`
	Status   *int    `json:"status_code"`
	Retries  int     `json:"retry_count"`
	// Retries the request may take, whether this failure is one to retry, and why it failed as the agent reads it
	MaxRetries int     `json:"max_retries"`
	Retryable  bool    `json:"retryable"`
	Reason     *string `json:"reason"`
	Error      Failure `json:"error"`
	Body       any     `json:"request"`
}

func (APIError) Point() string { return APIRequestError }

// A request of a helper model's task, one per attempt of a route
type Auxiliary struct {
	Session  string `json:"session_id"`
	Task     string `json:"task_id"`
	Turn     string `json:"turn_id"`
	Platform string `json:"platform"`
	// Task of the helper model, such as title_generation
	Job      string  `json:"aux_task"`
	ID       string  `json:"api_request_id"`
	Retries  int     `json:"retry_count"`
	Count    int     `json:"api_call_count"`
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	BaseURL  string  `json:"base_url"`
	Mode     string  `json:"api_mode"`
	Streamed bool    `json:"streaming"`
	Started  float64 `json:"started_at"`
	Messages int     `json:"message_count"`
}

func (a Auxiliary) call() string { return a.Turn }

// A helper model's request about to be sent
type AuxRequest struct {
	Auxiliary
	Sent      []map[string]any `json:"request_messages"`
	System    string           `json:"system_prompt"`
	Tools     int              `json:"tool_count"`
	Tokens    int              `json:"approx_input_tokens"`
	Chars     int              `json:"request_char_count"`
	MaxTokens *int             `json:"max_tokens"`
	Body      any              `json:"request"`
}

func (AuxRequest) Point() string { return PreAuxiliaryCall }

// A helper model's request that was answered or failed
type AuxResponse struct {
	Auxiliary
	Finished float64 `json:"ended_at"`
	Duration float64 `json:"api_duration"`
	// What failed, kind and message, and its kind; nil for an answer
	Error     *string `json:"error"`
	ErrorType *string `json:"error_type"`
	Finish    *string `json:"finish_reason"`
	Answering *string `json:"response_model"`
	Usage     *Usage  `json:"usage"`
	Body      any     `json:"response"`
	Chars     int     `json:"assistant_content_chars"`
	ToolCalls int     `json:"assistant_tool_call_count"`
}

func (AuxResponse) Point() string { return PostAuxiliaryCall }

// A failed model request, which a plugin may classify in place of the agent
type Classify struct {
	Provider string         `json:"provider"`
	Model    string         `json:"model"`
	Status   *int           `json:"status_code"`
	Type     string         `json:"error_type"`
	Code     string         `json:"error_code"`
	Message  string         `json:"error_message"`
	Body     map[string]any `json:"error_body"`
	Error    string         `json:"error"`
	Tokens   int            `json:"approx_tokens"`
	Window   int            `json:"context_length"`
	Count    int            `json:"num_messages"`
}

func (Classify) Point() string { return TransformAPIErrorClassification }

// How a plugin classified a failed request
type Classification struct {
	// Why it failed, one of the reasons the agent knows
	Reason string
	// What the agent does about it, each nil to leave it to the agent
	Retryable, Compress, Rotate, Fallback *bool
	// What the user is told, at most 500 characters
	Message string
	Context map[string]any
}

// A session that starts, its prompt built for the first time
type SessionStarted struct {
	Session  string `json:"session_id"`
	Model    string `json:"model"`
	Platform string `json:"platform"`
}

func (SessionStarted) Point() string { return SessionStart }

// A turn of a session that ended, however it ended
type TurnEnded struct {
	Session     string `json:"session_id"`
	Task        string `json:"task_id"`
	Turn        string `json:"turn_id"`
	Completed   bool   `json:"completed"`
	Failed      bool   `json:"failed"`
	Interrupted bool   `json:"interrupted"`
	// How it ended, such as text_response(stop)
	Reason   string `json:"turn_exit_reason"`
	Model    string `json:"model"`
	Platform string `json:"platform"`
}

func (TurnEnded) Point() string { return SessionEnd }

func (t TurnEnded) call() string { return t.Turn }

// A session closed for good, whose hooks may have something to tell the user
type Finalized struct {
	Session  string `json:"session_id"`
	Platform string `json:"platform"`
	// Why it closed: shutdown, new_session, or as the session store ends it
	Reason string `json:"reason"`
	// For a session a window left for a new one, the two of them
	Old *string `json:"old_session_id,omitempty"`
	New *string `json:"new_session_id,omitempty"`
}

func (Finalized) Point() string { return SessionFinalize }

// A window that left its session for a new one
type Reset struct {
	Session  *string `json:"session_id"`
	Reason   string  `json:"reason"`
	Platform string  `json:"platform"`
	Old      *string `json:"old_session_id"`
	New      *string `json:"new_session_id"`
}

func (Reset) Point() string { return SessionReset }

// Sessions taken out of the store
type Deleted struct {
	Session string `json:"session_id"`
	// delete, prune, archive or empty_sweep
	Reason  string  `json:"reason"`
	Profile *string `json:"profile"`
	// Every session it took out, when more than one
	IDs []string `json:"deleted_ids,omitempty"`
}

func (Deleted) Point() string { return SessionDelete }

// A change of a skill's state its usage records
type Skill struct {
	// loaded, viewed, patched, created, installed, archived and the like
	Action string `json:"action"`
	Name   string `json:"skill_name"`
	// Where it comes from: bundled, installed, agent_created, external, local or unknown
	Provenance string `json:"provenance"`
	Task       string `json:"task_id"`
	Session    string `json:"session_id"`
	Uses       any    `json:"use_count"`
	Reused     any    `json:"reused"`
	Patched    any    `json:"reuse_after_patch"`
}

func (Skill) Point() string { return SkillLifecycle }

// A subagent the agent delegated a task to, about to start
type SubagentStarted struct {
	Parent       *string `json:"parent_session_id"`
	ParentTurn   string  `json:"parent_turn_id"`
	ParentAgent  *string `json:"parent_subagent_id"`
	ChildSession *string `json:"child_session_id"`
	ChildAgent   string  `json:"child_subagent_id"`
	Role         string  `json:"child_role"`
	Goal         string  `json:"child_goal"`
}

func (SubagentStarted) Point() string { return SubagentStart }

// A tool call of a subagent, as its stop tells it
type Step struct {
	Tool string `json:"tool_name"`
	// Names of its arguments and the paths and addresses it acted on
	Input  map[string]any `json:"tool_input"`
	In     int            `json:"input_bytes"`
	Out    *int           `json:"output_bytes"`
	Status string         `json:"status"`
}

// A subagent that finished its task
type SubagentStopped struct {
	Parent       *string `json:"parent_session_id"`
	ParentTurn   string  `json:"parent_turn_id"`
	ChildSession *string `json:"child_session_id"`
	Role         *string `json:"child_role"`
	Summary      *string `json:"child_summary"`
	Status       string  `json:"child_status"`
	Steps        []Step  `json:"tool_call_history"`
	Duration     int64   `json:"duration_ms"`
}

func (SubagentStopped) Point() string { return SubagentStop }

// An approval about to be asked
type Approval struct {
	Command     string   `json:"command"`
	Description string   `json:"description"`
	Key         string   `json:"pattern_key"`
	Keys        []string `json:"pattern_keys"`
	SessionKey  string   `json:"session_key"`
	// Where it is asked: the chat, smart for the helper model's review, transport:<name> for an approval transport
	Surface string `json:"surface"`
	// Request a transport is handed and its digest, for an approval through one
	RequestID string `json:"request_id,omitempty"`
	Digest    string `json:"request_digest,omitempty"`
	// It waits on an identical prompt already asked
	Coalesced bool   `json:"coalesced,omitempty"`
	Session   string `json:"session_id"`
	Turn      string `json:"turn_id"`
	Call      string `json:"tool_call_id"`
}

func (Approval) Point() string { return PreApprovalRequest }

// An approval that was answered, refused or not answered in time
type Approved struct {
	Approval
	// once, session, always, deny, timeout, cancelled, smart_approve, smart_deny or transport_<failure>
	Choice    string `json:"choice"`
	Cancelled string `json:"cancelled,omitempty"`
	// Who decided it when the user did not, aux_llm for the smart reviewer
	DecidedBy string `json:"decided_by,omitempty"`
}

func (Approved) Point() string { return PostApprovalResponse }

// The user asked for something, an approval, an answer or a password
type HumanInput struct {
	// approval, clarify or sudo
	Kind       string `json:"kind"`
	ID         string `json:"request_id"`
	Session    string `json:"session_id"`
	SessionKey string `json:"session_key"`
	Platform   string `json:"platform"`
	// What the user is asked, secrets masked
	Prompt string `json:"prompt"`
	Turn   string `json:"turn_id"`
	Call   string `json:"tool_call_id"`
}

func (HumanInput) Point() string { return HumanInputRequest }

// What became of asking the user
type HumanAnswered struct {
	HumanInput
	Outcome string `json:"outcome"`
}

func (HumanAnswered) Point() string { return HumanInputResolved }

// A slash command about to run
type Command struct {
	// Where it was typed: cli for the app and the command line
	Surface string `json:"surface"`
	Name    string `json:"command"`
	// Name it was typed as
	Alias      string  `json:"alias_used"`
	Args       string  `json:"args_raw"`
	SessionKey *string `json:"session_key"`
	Platform   *string `json:"platform"`
}

func (Command) Point() string { return PreCommand }

func (c Command) call() string {
	if c.SessionKey == nil {
		return ""
	}
	return *c.SessionKey
}

// A compaction of a session's context that was stored
type Compacted struct {
	Session string `json:"session_id"`
	// Session it came from when the conversation went on in a new one, empty when it did not
	Old string `json:"old_session_id"`
	// summarize, micro_summarize, prune_tool_results or provider_native
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Before *int   `json:"before_tokens"`
	After  *int   `json:"after_tokens"`
	// Messages it folded away
	Removed *int `json:"messages_removed"`
}

func (Compacted) Point() string { return PostCompaction }

func (c Compacted) call() string { return c.Session }

// Audio about to be transcribed, whose model, language and prompt a hook may set
type Transcription struct {
	File     string  `json:"file_path"`
	Provider string  `json:"provider"`
	Model    *string `json:"model"`
	Language *string `json:"language"`
	Prompt   *string `json:"prompt"`
	Source   *string `json:"source"`
}

func (Transcription) Point() string { return PreTranscription }

// Messages of a conversation as hooks read them: role, content, the tool calls an answer made and the call a result is of
func Messages(list []*sessionsv1.Message) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		row := map[string]any{"role": strings.TrimPrefix(strings.ToLower(m.GetRole().String()), "role_"), "content": cmp.Or(m.GetSent(), m.GetText())}
		if m.GetReasoning() != "" {
			row["reasoning"] = m.GetReasoning()
		}
		if calls := m.GetToolCalls(); len(calls) > 0 {
			listed := make([]any, len(calls))
			for i, call := range calls {
				listed[i] = map[string]any{"id": call.GetId(), "type": "function",
					"function": map[string]any{"name": call.GetName(), "arguments": call.GetArguments()}}
			}
			row["tool_calls"] = listed
		}
		if m.GetToolCallId() != "" {
			row["tool_call_id"] = m.GetToolCallId()
		}
		if m.GetToolName() != "" {
			row["name"] = m.GetToolName()
		}
		out = append(out, row)
	}
	return out
}

// Text as a nullable value, nil for empty
func Optional(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

// Tokens a model call took as hooks read them, nil when the provider said nothing of them
func UsageOf(u *provider.Usage) *Usage {
	if u == nil {
		return nil
	}
	return &Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Reasoning: u.Reasoning, Requests: 1,
		Prompt: u.Prompt(), Total: u.Prompt() + u.Output}
}

// Characters of the text of the messages, which a rough count of their tokens divides by four
func Chars(list []*sessionsv1.Message) int {
	total := 0
	for _, m := range list {
		total += utf8.RuneCountInString(cmp.Or(m.GetSent(), m.GetText()))
	}
	return total
}

// Text of the first system message, empty when there is none
func System(list []*sessionsv1.Message) string {
	for _, m := range list {
		if m.GetRole() == sessionsv1.Role_ROLE_SYSTEM {
			return m.GetText()
		}
	}
	return ""
}

// The request as hooks are handed it: the body sent, secrets masked and cut to hooks.payload_max_chars
func RequestView(cfg *config.Config, request *provider.Request) any {
	body := map[string]any{"model": request.Model, "messages": Messages(request.Messages)}
	if len(request.Tools) > 0 {
		body["tools"] = request.Tools
	}
	if request.MaxTokens > 0 {
		body["max_tokens"] = request.MaxTokens
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if request.Reasoning.Effort != "" {
		body["reasoning_effort"] = request.Reasoning.Effort
	}
	if request.Format != nil {
		body["response_format"] = request.Format
	}
	for key, value := range request.Extra {
		body[key] = value
	}
	return Sanitize(cfg, map[string]any{"method": "POST", "body": body})
}

// The reply as hooks are handed it: the model, why it stopped, the assistant's message and the tokens, secrets masked and cut to
// hooks.payload_max_chars
func ResponseView(cfg *config.Config, model string, response *provider.Response) any {
	message := response.Message
	calls := []any{}
	for _, call := range message.GetToolCalls() {
		calls = append(calls, map[string]any{"id": call.GetId(), "type": "function", "function": map[string]any{"name": call.GetName(),
			"arguments": call.GetArguments()}})
	}
	return Sanitize(cfg, map[string]any{"model": model, "upstream_provider": nil, "finish_reason": Optional(response.FinishReason),
		"assistant_message": map[string]any{"role": "assistant", "content": message.GetText(), "tool_calls": calls}, "usage": UsageOf(response.Usage)})
}
