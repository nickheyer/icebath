package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/shell"
)

// Moment in a session or run a hook can attach to
type HookEvent string

const (
	SessionStart        HookEvent = "SessionStart"
	SessionEnd          HookEvent = "SessionEnd"
	Setup               HookEvent = "Setup"
	InstructionsLoaded  HookEvent = "InstructionsLoaded"
	UserPromptSubmit    HookEvent = "UserPromptSubmit"
	UserPromptExpansion HookEvent = "UserPromptExpansion"
	MessageDisplay      HookEvent = "MessageDisplay"
	PreToolUse          HookEvent = "PreToolUse"
	PermissionRequest   HookEvent = "PermissionRequest"
	PermissionDenied    HookEvent = "PermissionDenied"
	PostToolUse         HookEvent = "PostToolUse"
	PostToolUseFailure  HookEvent = "PostToolUseFailure"
	PostToolBatch       HookEvent = "PostToolBatch"
	Notification        HookEvent = "Notification"
	SubagentStart       HookEvent = "SubagentStart"
	SubagentStop        HookEvent = "SubagentStop"
	TaskCreated         HookEvent = "TaskCreated"
	TaskCompleted       HookEvent = "TaskCompleted"
	TeammateIdle        HookEvent = "TeammateIdle"
	Stop                HookEvent = "Stop"
	StopFailure         HookEvent = "StopFailure"
	ConfigChange        HookEvent = "ConfigChange"
	CwdChanged          HookEvent = "CwdChanged"
	DirectoryAdded      HookEvent = "DirectoryAdded"
	FileChanged         HookEvent = "FileChanged"
	PreCompact          HookEvent = "PreCompact"
	PostCompact         HookEvent = "PostCompact"
	PreModelSwitch      HookEvent = "PreModelSwitch"
	PostModelSwitch     HookEvent = "PostModelSwitch"
	Elicitation         HookEvent = "Elicitation"
	ElicitationResult   HookEvent = "ElicitationResult"
	WorktreeCreate      HookEvent = "WorktreeCreate"
	WorktreeRemove      HookEvent = "WorktreeRemove"
)

// Every event a configuration may name
var HookEvents = []HookEvent{
	SessionStart, SessionEnd, Setup, InstructionsLoaded, UserPromptSubmit, UserPromptExpansion, MessageDisplay,
	PreToolUse, PermissionRequest, PermissionDenied, PostToolUse, PostToolUseFailure, PostToolBatch, Notification,
	SubagentStart, SubagentStop, TaskCreated, TaskCompleted, TeammateIdle, Stop, StopFailure, ConfigChange, CwdChanged,
	DirectoryAdded, FileChanged, PreCompact, PostCompact, PreModelSwitch, PostModelSwitch, Elicitation, ElicitationResult,
	WorktreeCreate, WorktreeRemove,
}

// Kinds of handler a hook may be
const (
	HookTypeCommand = "command"
	HookTypePrompt  = "prompt"
	HookTypeAgent   = "agent"
	HookTypeHTTP    = "http"
	HookTypeMCP     = "mcp_tool"
)

// Origins a hook may come from, shown on the prompts it raises and deciding what it may do
const (
	SourceSettings = "settings"
	SourcePlugin   = "plugin"
	SourceSkill    = "skill"
	SourceAgent    = "agent"
)

// Turns the agent may take inside one agent hook
const agentHookTurns = 50

// Characters a context, system message or initial message may carry before it goes to a file
const contextCap = 10000

// Characters of a capped text shown beside the path of its file
const contextPreview = 2000

// Continuations a stop hook may force in a row before its block is overridden
const stopBlockCap = 8

// One handler to run
type HookCommand struct {
	Type    string   `json:"type,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Command run instead on Windows
	Windows string            `json:"windows,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Prompt  string            `json:"prompt,omitempty"`
	Model   string            `json:"model,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Environment variables that may be interpolated into the headers
	AllowedEnvVars []string       `json:"allowedEnvVars,omitempty"`
	Server         string         `json:"server,omitempty"`
	Tool           string         `json:"tool,omitempty"`
	Input          map[string]any `json:"input,omitempty"`
	// Seconds the handler may take, the default of its type when zero
	Timeout       int    `json:"timeout,omitempty"`
	StatusMessage string `json:"statusMessage,omitempty"`
	// Removed after its first successful run, honored for skill hooks
	Once bool `json:"once,omitempty"`
	// Permission rule the tool call must match for the handler to run
	If string `json:"if,omitempty"`
	// Runs in the background, its context delivered at the next turn
	Async bool `json:"async,omitempty"`
	// Runs in the background and wakes the agent with its stderr on exit 2
	AsyncRewake bool `json:"asyncRewake,omitempty"`
	// Shell of a shell form command, bash or powershell
	Shell string `json:"shell,omitempty"`
	// What a failure to run counts as, continue or block
	OnFailure string `json:"onFailure,omitempty"`
	// Lets a prompt hook block without stopping the run
	ContinueOnBlock bool `json:"continueOnBlock,omitempty"`
	// Where the hook came from, settings, plugin, skill or agent
	Source string `json:"-"`
	// Name of the plugin, skill or agent the hook came from
	Owner string `json:"-"`
	// Values substituted for ${NAME} in the command, arguments, URL and prompt, and exported to the process
	Vars map[string]string `json:"-"`
}

// Commands run when the subject of the event matches the matcher, every subject when empty or a star
type HookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks"`
	re      *regexp.Regexp
	exact   []string
	all     bool
}

type HookConfig map[HookEvent][]HookGroup

// Reads a hooks block from JSON, nested under hooks or at the top level, and checks every entry
func LoadHooks(r io.Reader) (HookConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseHooks(data, false)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// Reads a hooks block from JSON, the event map alone refused when the wrapper is required
func ParseHooks(data []byte, wrapped bool) (HookConfig, error) {
	var doc struct {
		Hooks       map[HookEvent][]HookGroup `json:"hooks"`
		Description string                    `json:"description"`
		Modules     []string                  `json:"modules"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("hooks: %w", err)
	}
	if len(doc.Modules) > 0 {
		return nil, fmt.Errorf("hooks: modules %s are JavaScript hook modules, which need a JavaScript host", strings.Join(doc.Modules, ", "))
	}
	cfg := HookConfig(doc.Hooks)
	if cfg == nil {
		if wrapped {
			return nil, errors.New("hooks: the event map must sit under a top level hooks key")
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("hooks: %w", err)
		}
	}
	return cfg, cfg.Validate()
}

func LoadHooksFile(path string) (HookConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return LoadHooks(f)
}

func knownHook(event HookEvent) bool {
	for _, e := range HookEvents {
		if e == event {
			return true
		}
	}
	return false
}

// Reports whether the matcher holds only the characters of an exact list, letters, digits, underscores, dashes, spaces, commas and bars
func exactMatcher(m string, strict bool) bool {
	for _, r := range m {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '|':
		case (r == '-' || r == ' ' || r == ',') && !strict:
		default:
			return false
		}
	}
	return true
}

// Compiles the matcher of a group, exact names when it holds only list characters and a regular expression otherwise
func (g *HookGroup) compile(event HookEvent) error {
	m := g.Matcher
	g.all, g.exact, g.re = false, nil, nil
	if m == "" || m == "*" {
		g.all = true
		return nil
	}
	strict := event == FileChanged || event == StopFailure
	if exactMatcher(m, strict) {
		seps := func(r rune) bool { return r == '|' || (r == ',' && !strict) }
		for _, name := range strings.FieldsFunc(m, seps) {
			if name = strings.TrimSpace(name); name != "" {
				g.exact = append(g.exact, name)
			}
		}
		return nil
	}
	re, err := regexp.Compile(m)
	if err != nil {
		return err
	}
	g.re = re
	return nil
}

// Reports whether the subject of an event matches the group
func (g *HookGroup) matches(subject string) bool {
	switch {
	case g.all:
		return true
	case len(g.exact) > 0:
		for _, name := range g.exact {
			if name == subject {
				return true
			}
		}
		return false
	case g.re != nil:
		return g.re.MatchString(subject)
	}
	return true
}

// Names the matcher of a FileChanged group registers for watching, split on bars
func (g HookGroup) watched() []string {
	if g.Matcher == "" {
		return nil
	}
	var out []string
	for _, name := range strings.Split(g.Matcher, "|") {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Files every FileChanged group of the configuration watches, relative to the working directory, sorted and unique
func (c HookConfig) Watched() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range c[FileChanged] {
		for _, name := range g.watched() {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Checks event names, handler kinds, fields and matcher patterns, compiling the patterns
func (c HookConfig) Validate() error {
	for event, groups := range c {
		if !knownHook(event) {
			return fmt.Errorf("hooks: unknown event %q", event)
		}
		for i := range groups {
			g := &groups[i]
			if err := g.compile(event); err != nil {
				return fmt.Errorf("hooks: %s matcher %q: %w", event, g.Matcher, err)
			}
			for j := range g.Hooks {
				if err := g.Hooks[j].validate(event); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Checks the kind and the fields the kind needs
func (h *HookCommand) validate(event HookEvent) error {
	if h.Type == "" {
		h.Type = HookTypeCommand
	}
	switch h.Type {
	case HookTypeCommand:
		if strings.TrimSpace(h.Command) == "" {
			return fmt.Errorf("hooks: %s has a command hook without a command", event)
		}
	case HookTypePrompt, HookTypeAgent:
		if strings.TrimSpace(h.Prompt) == "" {
			return fmt.Errorf("hooks: %s has a %s hook without a prompt", event, h.Type)
		}
	case HookTypeHTTP:
		if strings.TrimSpace(h.URL) == "" {
			return fmt.Errorf("hooks: %s has an http hook without a url", event)
		}
	case HookTypeMCP:
		if h.Server == "" || h.Tool == "" {
			return fmt.Errorf("hooks: %s has an mcp_tool hook without a server and a tool", event)
		}
	default:
		return fmt.Errorf("hooks: %s hook type %q is not command, prompt, agent, http or mcp_tool", event, h.Type)
	}
	if (h.Async || h.AsyncRewake) && h.Type != HookTypeCommand {
		return fmt.Errorf("hooks: %s has an async %s hook, only command hooks run async", event, h.Type)
	}
	switch h.OnFailure {
	case "", "continue", "block":
	default:
		return fmt.Errorf("hooks: %s hook onFailure %q is not continue or block", event, h.OnFailure)
	}
	switch h.Shell {
	case "", "bash", "powershell":
	default:
		return fmt.Errorf("hooks: %s hook shell %q is not bash or powershell", event, h.Shell)
	}
	if h.If != "" {
		if _, err := policy.ParseRule(h.If); err != nil {
			return fmt.Errorf("hooks: %s hook if %q: %w", event, h.If, err)
		}
	}
	if h.Timeout < 0 {
		return fmt.Errorf("hooks: %s hook timeout must not be negative", event)
	}
	return nil
}

// Identity of a handler, so one defined in several settings files runs once
func (h HookCommand) identity(event HookEvent, matcher string) string {
	raw, _ := json.Marshal(h)
	return string(event) + "\x00" + matcher + "\x00" + string(raw)
}

// Copies the configuration with its groups labeled by source and owner
func (c HookConfig) Labeled(source, owner string, vars map[string]string) HookConfig {
	out := HookConfig{}
	for event, groups := range c {
		copied := make([]HookGroup, 0, len(groups))
		for _, g := range groups {
			ng := HookGroup{Matcher: g.Matcher}
			for _, h := range g.Hooks {
				h.Source, h.Owner = source, owner
				if len(vars) > 0 {
					merged := make(map[string]string, len(h.Vars)+len(vars))
					for k, v := range h.Vars {
						merged[k] = v
					}
					for k, v := range vars {
						merged[k] = v
					}
					h.Vars = merged
				}
				ng.Hooks = append(ng.Hooks, h)
			}
			ng.compile(event)
			copied = append(copied, ng)
		}
		out[event] = copied
	}
	return out
}

// Joins configurations in order, a settings handler already present skipped so it runs once
func MergeHooks(configs ...HookConfig) HookConfig {
	out := HookConfig{}
	seen := map[string]bool{}
	for _, c := range configs {
		for event, groups := range c {
			for _, g := range groups {
				ng := HookGroup{Matcher: g.Matcher}
				for _, h := range g.Hooks {
					if h.Source == SourceSettings || h.Source == "" {
						id := h.identity(event, g.Matcher)
						if seen[id] {
							continue
						}
						seen[id] = true
					}
					ng.Hooks = append(ng.Hooks, h)
				}
				if len(ng.Hooks) == 0 {
					continue
				}
				ng.compile(event)
				out[event] = append(out[event], ng)
			}
		}
	}
	return out
}

// Copies the configuration with every Stop group moved to SubagentStop, as a subagent definition is read
func (c HookConfig) ForSubagent() HookConfig {
	out := HookConfig{}
	for event, groups := range c {
		if event == Stop {
			event = SubagentStop
		}
		out[event] = append(out[event], groups...)
	}
	return out
}

// Reports whether the configuration holds any handler
func (c HookConfig) Empty() bool {
	for _, groups := range c {
		for _, g := range groups {
			if len(g.Hooks) > 0 {
				return false
			}
		}
	}
	return true
}

// What the host offers hooks, model calls, tool calls, rule matching and ways to reach the person and the agent
type HookHost struct {
	// Answers a prompt hook with the model, the background model when the name is empty
	Prompt func(ctx context.Context, model, prompt string) (string, error)
	// Runs an agent hook with tools for up to the turn limit and returns its final text
	Agent func(ctx context.Context, model, prompt string, turns int) (string, error)
	// Calls a tool of an MCP server for an mcp_tool hook, the text it returned and whether it marked an error
	MCPCall func(ctx context.Context, server, tool string, input map[string]any) (string, bool, error)
	// Reports whether a tool call matches a permission rule and whether that could be determined
	Matches func(rule string, call protocol.ToolCall) (matched, determined bool)
	// Shows the status message of a running hook
	Status func(text string)
	// Shows a system message to the person
	Notify func(text string)
	// Writes a terminal sequence to the terminal
	Terminal func(seq string)
	// Hands context from an async hook to the agent for its next turn
	Deliver func(text string)
	// Permission mode in force, as hooks receive it
	Mode func() string
}

// State shared by every hook runner of one agent, spent once hooks and stop continuations
type HookState struct {
	mu       sync.Mutex
	spent    map[string]bool
	blocks   int
	promptID string
}

// Reports whether a once hook already ran and marks it spent otherwise
func (s *HookState) spend(key string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spent == nil {
		s.spent = map[string]bool{}
	}
	if s.spent[key] {
		return true
	}
	return false
}

func (s *HookState) markSpent(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spent == nil {
		s.spent = map[string]bool{}
	}
	s.spent[key] = true
}

// Counts a stop continuation, reporting whether the cap is reached
func (s *HookState) continued() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks++
	return s.blocks > stopBlockCap
}

// Resets the stop continuation count, done when the agent calls a tool
func (s *HookState) toolCalled() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.blocks = 0
	s.mu.Unlock()
}

// Sets the id of the user prompt the run serves
func (s *HookState) SetPrompt(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.promptID = id
	s.mu.Unlock()
}

func (s *HookState) prompt() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promptID
}

// Runs configured hooks as an agent hook
type HookRunner struct {
	Config HookConfig
	// Configuration read afresh on every event when set, so hooks registered later run too
	Dynamic func() HookConfig
	// Working directory of hook commands, the process directory when empty
	Dir string
	// Current directory consulted ahead of Dir, so a moving shell is followed
	DirFunc func() string
	// Project root passed as CLAUDE_PROJECT_DIR
	ProjectDir string
	// KEY=VALUE pairs layered over the process environment
	Env []string
	// Passed to hooks as session_id and ICEBATH_SESSION_ID
	SessionID string
	// Path of the stored session log, passed to hooks as transcript_path
	Transcript string
	// Scratch directory of the session, passed as scratchpad_dir and holding capped output
	Scratch string
	// Identity of the agent when it is a subagent
	AgentID   string
	AgentType string
	// Effort level of the model, passed to hooks when known
	Effort string
	// Receives hook standard error and any standard output that is not JSON, discarded when nil
	Log io.Writer
	// Services of the host
	Host HookHost
	// State shared with the other runners of the agent
	State *HookState
}

// What one hook execution produced, published as the data of a hook event
type HookExecution struct {
	HookEvent HookEvent
	Type      string
	Command   string
	Source    string
	Owner     string
	Subject   string
	ExitCode  int
	Duration  time.Duration
	Stdout    string
	Stderr    string
	Blocked   bool
}

// Tool result as PostToolUse hooks receive it, every part in its stored JSON form
type hookToolResponse struct {
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// Permission change a PermissionRequest hook asks to keep
type PermissionUpdate struct {
	Type        string           `json:"type"`
	Rules       []PermissionRule `json:"rules,omitempty"`
	Behavior    string           `json:"behavior,omitempty"`
	Mode        string           `json:"mode,omitempty"`
	Directories []string         `json:"directories,omitempty"`
	Destination string           `json:"destination,omitempty"`
}

// One rule of a permission update
type PermissionRule struct {
	ToolName    string `json:"toolName"`
	RuleContent string `json:"ruleContent,omitempty"`
}

// JSON a hook may print
type hookOutput struct {
	Continue          *bool           `json:"continue"`
	StopReason        string          `json:"stopReason"`
	SuppressOutput    bool            `json:"suppressOutput"`
	SystemMessage     string          `json:"systemMessage"`
	TerminalSequence  string          `json:"terminalSequence"`
	Decision          string          `json:"decision"`
	Reason            string          `json:"reason"`
	AdditionalContext string          `json:"additionalContext"`
	UpdatedInput      json.RawMessage `json:"updatedInput"`
	HookSpecific      *hookSpecific   `json:"hookSpecificOutput"`
}

// Event specific part of the JSON a hook may print
type hookSpecific struct {
	HookEventName            string             `json:"hookEventName"`
	PermissionDecision       string             `json:"permissionDecision"`
	PermissionDecisionReason string             `json:"permissionDecisionReason"`
	UpdatedInput             json.RawMessage    `json:"updatedInput"`
	AdditionalContext        string             `json:"additionalContext"`
	UpdatedToolOutput        json.RawMessage    `json:"updatedToolOutput"`
	UpdatedMCPToolOutput     json.RawMessage    `json:"updatedMCPToolOutput"`
	ClassifierContext        string             `json:"classifierContext"`
	Decision                 *permissionAnswer  `json:"decision"`
	Retry                    bool               `json:"retry"`
	DisplayContent           *string            `json:"displayContent"`
	SessionTitle             string             `json:"sessionTitle"`
	InitialUserMessage       string             `json:"initialUserMessage"`
	SuppressOriginalPrompt   bool               `json:"suppressOriginalPrompt"`
	WatchPaths               []string           `json:"watchPaths"`
	ReloadSkills             bool               `json:"reloadSkills"`
	Action                   string             `json:"action"`
	Content                  map[string]any     `json:"content"`
	Path                     string             `json:"path"`
	UpdatedPermissions       []PermissionUpdate `json:"updatedPermissions"`
}

// Answer a PermissionRequest hook gives
type permissionAnswer struct {
	Behavior           string             `json:"behavior"`
	UpdatedInput       json.RawMessage    `json:"updatedInput"`
	UpdatedPermissions []PermissionUpdate `json:"updatedPermissions"`
	Message            string             `json:"message"`
	Interrupt          bool               `json:"interrupt"`
}

// What the hooks of one event asked for, merged
type Verdict struct {
	Block  bool
	Reason string
	// Text added to the context of the agent
	Context string
	// Permission settled for the call
	Decision policy.Decision
	Input    json.RawMessage
	// Text standing in for the whole tool result
	Response string
	// The run stops entirely with the reason
	Stop       bool
	StopReason string
	// Shown to the person
	System string
	// Title a hook gave the session
	Title string
	// First user turn a SessionStart hook supplied
	InitialMessage string
	// Original prompt left out of a blocked prompt message
	SuppressOriginal bool
	// A denied call is retried with the person asked
	Retry bool
	// Text shown in place of the message, set when any hook gave one
	Display    *string
	WatchPaths []string
	// Skills are read again
	ReloadSkills bool
	// Answer of a PermissionRequest hook, empty behavior when none answered
	Behavior  string
	Message   string
	Interrupt bool
	Updates   []PermissionUpdate
	// Action and content of an elicitation hook
	Action  string
	Content map[string]any
	// Path a WorktreeCreate hook returned
	Path string
	// Hook failures that did not block, for the log
	Errors []string
}

// Refusal raised by a hook
type HookBlocked struct {
	HookEvent HookEvent
	Reason    string
}

func (b *HookBlocked) Error() string {
	return fmt.Sprintf("hook %s blocked: %s", b.HookEvent, b.Reason)
}

// Stop of the whole run asked by a hook through continue false
type HookStopped struct {
	HookEvent HookEvent
	Reason    string
}

func (s *HookStopped) Error() string {
	if s.Reason == "" {
		return fmt.Sprintf("hook %s stopped the run", s.HookEvent)
	}
	return fmt.Sprintf("hook %s stopped the run: %s", s.HookEvent, s.Reason)
}

// Wraps text as the system reminder hooks add to the context, naming the hook that added it
func reminder(label, text string) string {
	text = strings.ReplaceAll(text, "<system-reminder>", "&lt;system-reminder>")
	text = strings.ReplaceAll(text, "</system-reminder>", "&lt;/system-reminder>")
	if label != "" {
		text = label + "\n" + text
	}
	return "<system-reminder>\n" + text + "\n</system-reminder>"
}

// Parts the agent sees for a context hooks added, none for an empty one
func contextParts(label, text string) []protocol.Part {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []protocol.Part{protocol.Text{Text: reminder(label, text)}}
}

// Fields every event carries
func (r HookRunner) common(event HookEvent) map[string]any {
	dir := r.cwd()
	p := map[string]any{"session_id": r.SessionID, "hook_event_name": string(event), "cwd": dir}
	if id := r.State.prompt(); id != "" {
		p["prompt_id"] = id
	}
	if r.Transcript != "" {
		p["transcript_path"] = r.Transcript
	}
	if r.Scratch != "" {
		p["scratchpad_dir"] = r.Scratch
	}
	if r.Host.Mode != nil {
		if mode := r.Host.Mode(); mode != "" {
			p["permission_mode"] = mode
		}
	}
	if r.Effort != "" {
		p["effort"] = map[string]string{"level": r.Effort}
	}
	if r.AgentID != "" {
		p["agent_id"] = r.AgentID
		p["agent_type"] = r.AgentType
	}
	return p
}

// Fires UserPromptSubmit, a block refusing the input and context joining it
func (r HookRunner) BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
	r.State.SetPrompt(newPromptID())
	v, err := r.Fire(ctx, UserPromptSubmit, "", map[string]any{"prompt": protocolText(input)})
	if err != nil {
		return nil, err
	}
	if v.Stop {
		return nil, &HookStopped{HookEvent: UserPromptSubmit, Reason: v.StopReason}
	}
	if v.Block {
		reason := v.Reason
		if !v.SuppressOriginal {
			reason = strings.TrimSpace(reason + "\n\nOriginal prompt:\n" + protocolText(input))
		}
		return nil, &HookBlocked{HookEvent: UserPromptSubmit, Reason: reason}
	}
	input = append(input, contextParts("UserPromptSubmit hook", v.Context)...)
	if r.AgentID != "" {
		start, err := r.Fire(ctx, SubagentStart, r.AgentType, map[string]any{})
		if err != nil {
			return nil, err
		}
		input = append(input, contextParts("SubagentStart hook", start.Context)...)
	}
	return input, nil
}

func (HookRunner) BeforeTurn(context.Context, *protocol.Request) error { return nil }

// Fires MessageDisplay for the text of the turn, the display text kept on the session for the transcript
func (r HookRunner) AfterTurn(ctx context.Context, resp protocol.Response) error {
	if len(r.config()[MessageDisplay]) == 0 {
		return nil
	}
	text := resp.Text()
	if strings.TrimSpace(text) == "" {
		return nil
	}
	v, err := r.Fire(ctx, MessageDisplay, "", map[string]any{"turn_id": resp.ID, "message_id": resp.ID, "index": 0, "final": true, "delta": text})
	if err != nil {
		return nil
	}
	if v.Display != nil {
		Observe(ctx, Event{Type: EventDisplay, Text: *v.Display, Data: resp.ID})
	}
	return nil
}

// Fires PreToolUse, carrying the rewritten input, settled permission and context back as advice
func (r HookRunner) BeforeTool(ctx context.Context, call protocol.ToolCall) (Advice, error) {
	r.State.toolCalled()
	v, err := r.fire(ctx, PreToolUse, call.Name, map[string]any{"tool_name": call.Name, "tool_input": rawObject(call.Args), "tool_use_id": call.ID}, &call)
	if err != nil {
		return Advice{Call: call}, err
	}
	if v.Stop {
		return Advice{Call: call}, &HookStopped{HookEvent: PreToolUse, Reason: v.StopReason}
	}
	if v.Block {
		return Advice{Call: call}, &HookBlocked{HookEvent: PreToolUse, Reason: v.Reason}
	}
	if len(v.Input) > 0 {
		call.Args = v.Input
	}
	return Advice{Call: call, Decision: v.Decision, Context: v.Context}, nil
}

// Fires PostToolUse or PostToolUseFailure, an updated output replacing the result and a block adding its reason
func (r HookRunner) AfterTool(ctx context.Context, call protocol.ToolCall, res protocol.ToolResult) (protocol.ToolResult, error) {
	fields := map[string]any{"tool_name": call.Name, "tool_input": rawObject(call.Args), "tool_use_id": call.ID}
	if res.IsError {
		fields["error"] = protocolText(res.Content)
		fields["is_interrupt"] = errors.Is(ctx.Err(), context.Canceled)
		v, err := r.fire(ctx, PostToolUseFailure, call.Name, fields, &call)
		if err != nil {
			return res, err
		}
		if v.Stop {
			return res, &HookStopped{HookEvent: PostToolUseFailure, Reason: v.StopReason}
		}
		res.Content = append(res.Content, contextParts("PostToolUseFailure hook", v.Context)...)
		return res, nil
	}
	content, err := session.MarshalParts(res.Content)
	if err != nil {
		return res, fmt.Errorf("hooks: %w", err)
	}
	response, err := json.Marshal(hookToolResponse{Content: content, IsError: res.IsError})
	if err != nil {
		return res, err
	}
	fields["tool_response"] = json.RawMessage(response)
	v, err := r.fire(ctx, PostToolUse, call.Name, fields, &call)
	if err != nil {
		return res, err
	}
	if v.Stop {
		return res, &HookStopped{HookEvent: PostToolUse, Reason: v.StopReason}
	}
	if v.Response != "" {
		res.Content = []protocol.Part{protocol.Text{Text: v.Response}}
	}
	if v.Block {
		res.Content = append(res.Content, contextParts("PostToolUse hook", v.Reason)...)
	}
	res.Content = append(res.Content, contextParts("PostToolUse hook", v.Context)...)
	return res, nil
}

// Fires Stop, or SubagentStop for a subagent, a block continuing the run with its reason up to the cap
func (r HookRunner) OnStop(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error) {
	event, subject := Stop, ""
	if r.AgentID != "" {
		event, subject = SubagentStop, r.AgentType
	}
	fields := map[string]any{"stop_hook_active": again, "last_assistant_message": resp.Text()}
	if event == SubagentStop {
		fields["agent_transcript_path"] = r.Transcript
	}
	v, err := r.Fire(ctx, event, subject, fields)
	if err != nil {
		return nil, err
	}
	if v.Stop {
		return nil, nil
	}
	if !v.Block && v.Context == "" {
		return nil, nil
	}
	if r.State.continued() {
		return nil, nil
	}
	if v.Block {
		parts := []protocol.Part{protocol.Text{Text: v.Reason}}
		parts = append(parts, contextParts(string(event)+" hook", v.Context)...)
		return parts, nil
	}
	return contextParts(string(event)+" hook feedback", v.Context), nil
}

// Runs the hooks of any event with the subject its matchers see and extra top level fields, returning what they asked
func (r HookRunner) Fire(ctx context.Context, event HookEvent, subject string, fields map[string]any) (Verdict, error) {
	return r.fire(ctx, event, subject, fields, nil)
}

// Events whose exit 2 blocks what they guard
var blockingEvents = map[HookEvent]bool{
	PreToolUse: true, UserPromptSubmit: true, UserPromptExpansion: true, Stop: true, SubagentStop: true, TeammateIdle: true,
	TaskCreated: true, TaskCompleted: true, ConfigChange: true, PostToolBatch: true, PreCompact: true, PreModelSwitch: true,
	Elicitation: true, ElicitationResult: true, WorktreeCreate: true, WorktreeRemove: true,
}

// Events whose plain standard output joins the context of the agent
var contextEvents = map[HookEvent]bool{UserPromptSubmit: true, UserPromptExpansion: true, SessionStart: true, PostModelSwitch: true}

// Events whose exit 2 standard error is shown to the agent without blocking
var feedbackEvents = map[HookEvent]bool{PostToolUse: true, PostToolUseFailure: true}

// Events whose JSON is discarded apart from a terminal sequence
var discardEvents = map[HookEvent]bool{StopFailure: true, Setup: true, Notification: true, SessionEnd: true, PostCompact: true, InstructionsLoaded: true, CwdChanged: true, DirectoryAdded: true, FileChanged: true, WorktreeRemove: true}

// Events that may use the top level decision block
var decisionEvents = map[HookEvent]bool{
	UserPromptSubmit: true, UserPromptExpansion: true, PostToolUse: true, PostToolUseFailure: true, PostToolBatch: true, Stop: true,
	SubagentStop: true, ConfigChange: true, PreCompact: true, TaskCreated: true, PreModelSwitch: true, Elicitation: true, ElicitationResult: true, PreToolUse: true,
}

// Events the if condition of a hook is evaluated on
var toolEvents = map[HookEvent]bool{PreToolUse: true, PostToolUse: true, PostToolUseFailure: true, PermissionRequest: true, PermissionDenied: true}

// One handler chosen to run with its group matcher
type chosen struct {
	cmd     HookCommand
	matcher string
	key     string
}

// Picks the handlers of the event whose matcher takes the subject, whose condition holds and that are not spent
func (r HookRunner) config() HookConfig {
	if r.Dynamic != nil {
		return r.Dynamic()
	}
	return r.Config
}

func (r HookRunner) choose(event HookEvent, subject string, call *protocol.ToolCall) []chosen {
	var out []chosen
	for gi, g := range r.config()[event] {
		group := g
		if group.re == nil && len(group.exact) == 0 && !group.all {
			group.compile(event)
		}
		if !group.matches(subject) {
			continue
		}
		for hi, h := range g.Hooks {
			if h.Type == "" {
				h.Type = HookTypeCommand
			}
			if h.If != "" {
				if !toolEvents[event] || call == nil || r.Host.Matches == nil {
					continue
				}
				matched, determined := r.Host.Matches(h.If, *call)
				if determined && !matched {
					continue
				}
			}
			if event == PermissionRequest && h.Type == HookTypeAgent {
				continue
			}
			if event == SessionStart || event == Setup {
				if h.Type != HookTypeCommand && h.Type != HookTypeMCP {
					continue
				}
			}
			key := fmt.Sprintf("%s/%d/%d/%s", event, gi, hi, h.identity(event, g.Matcher))
			if h.Once && h.Source == SourceSkill && r.State.spend(key) {
				continue
			}
			out = append(out, chosen{cmd: h, matcher: g.Matcher, key: key})
		}
	}
	return out
}

// Runs every handler of the event in parallel and merges what they asked for
func (r HookRunner) fire(ctx context.Context, event HookEvent, subject string, fields map[string]any, call *protocol.ToolCall) (Verdict, error) {
	if !knownHook(event) {
		return Verdict{}, fmt.Errorf("hooks: unknown event %q", event)
	}
	handlers := r.choose(event, subject, call)
	if len(handlers) == 0 {
		return Verdict{}, nil
	}
	payload := r.common(event)
	for k, v := range fields {
		payload[k] = v
	}
	stdin, err := json.Marshal(payload)
	if err != nil {
		return Verdict{}, err
	}
	results := make([]Verdict, len(handlers))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 12)
	for i, h := range handlers {
		if h.cmd.Async || h.cmd.AsyncRewake {
			r.async(ctx, event, subject, h, stdin)
			continue
		}
		wg.Add(1)
		go func(i int, h chosen) {
			defer wg.Done()
			if h.cmd.Type == HookTypePrompt || h.cmd.Type == HookTypeAgent {
				slots <- struct{}{}
				defer func() { <-slots }()
			}
			results[i] = r.one(ctx, event, subject, h, stdin)
		}(i, h)
	}
	wg.Wait()
	merged := Verdict{}
	for i, v := range results {
		if handlers[i].cmd.Async || handlers[i].cmd.AsyncRewake {
			continue
		}
		merged = merge(merged, v)
	}
	for _, e := range merged.Errors {
		if r.Log != nil {
			fmt.Fprintf(r.Log, "[hook %s] %s\n", event, e)
		}
	}
	if merged.System != "" && r.Host.Notify != nil {
		r.Host.Notify(merged.System)
	}
	if merged.Block && merged.Reason == "" {
		merged.Reason = "blocked by hook"
	}
	return merged, nil
}

// Joins two verdicts, the stricter permission, every context and the first of each single value
func merge(a, b Verdict) Verdict {
	if b.Block {
		a.Block = true
		if a.Reason == "" {
			a.Reason = b.Reason
		}
	}
	if b.Context != "" {
		a.Context = strings.TrimSpace(a.Context + "\n\n" + b.Context)
	}
	a.Decision = stricter(a.Decision, b.Decision)
	if len(b.Input) > 0 {
		a.Input = b.Input
	}
	if b.Response != "" {
		a.Response = b.Response
	}
	if b.Stop {
		a.Stop = true
		if a.StopReason == "" {
			a.StopReason = b.StopReason
		}
	}
	if b.System != "" {
		a.System = strings.TrimSpace(a.System + "\n" + b.System)
	}
	if b.Title != "" {
		a.Title = b.Title
	}
	if b.InitialMessage != "" {
		a.InitialMessage = b.InitialMessage
	}
	a.SuppressOriginal = a.SuppressOriginal || b.SuppressOriginal
	a.Retry = a.Retry || b.Retry
	if b.Display != nil {
		a.Display = b.Display
	}
	a.WatchPaths = append(a.WatchPaths, b.WatchPaths...)
	a.ReloadSkills = a.ReloadSkills || b.ReloadSkills
	switch {
	case b.Behavior == "deny" || a.Behavior == "":
		if b.Behavior != "" {
			a.Behavior, a.Message, a.Interrupt = b.Behavior, b.Message, b.Interrupt
			if len(b.Input) > 0 {
				a.Input = b.Input
			}
		}
	}
	a.Updates = append(a.Updates, b.Updates...)
	switch {
	case b.Action == "decline" || a.Action == "":
		if b.Action != "" {
			a.Action, a.Content = b.Action, b.Content
		}
	}
	if b.Path != "" {
		a.Path = b.Path
	}
	a.Errors = append(a.Errors, b.Errors...)
	return a
}

// Picks the stricter of two decisions with defer ranking under deny
func stricter(a, b policy.Decision) policy.Decision {
	return policy.Stricter(a, b)
}

// Seconds a handler may take by default
func defaultTimeout(event HookEvent, cmd HookCommand) time.Duration {
	if cmd.Timeout > 0 {
		return time.Duration(cmd.Timeout) * time.Second
	}
	if event == SessionEnd {
		return 1500 * time.Millisecond
	}
	switch cmd.Type {
	case HookTypePrompt:
		return 30 * time.Second
	case HookTypeAgent:
		return 60 * time.Second
	}
	switch event {
	case UserPromptSubmit, PreModelSwitch, PostModelSwitch:
		return 30 * time.Second
	case MessageDisplay:
		return 10 * time.Second
	}
	return 600 * time.Second
}

// Runs one handler and reads its verdict
func (r HookRunner) one(ctx context.Context, event HookEvent, subject string, h chosen, stdin []byte) Verdict {
	cmd := h.cmd
	if cmd.StatusMessage != "" && r.Host.Status != nil {
		r.Host.Status(cmd.StatusMessage)
	}
	timeout := defaultTimeout(event, cmd)
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	ex := HookExecution{HookEvent: event, Type: cmd.Type, Command: r.describe(cmd), Source: cmd.Source, Owner: cmd.Owner, Subject: subject}
	var stdout, stderr string
	var code int
	var runErr error
	var mcpErr bool
	switch cmd.Type {
	case HookTypeCommand:
		stdout, stderr, code, runErr = r.execCommand(hctx, event, cmd, stdin)
	case HookTypePrompt:
		stdout, runErr = r.execPrompt(hctx, cmd, stdin, false)
	case HookTypeAgent:
		stdout, runErr = r.execPrompt(hctx, cmd, stdin, true)
	case HookTypeHTTP:
		stdout, code, runErr = r.execHTTP(hctx, cmd, stdin)
	case HookTypeMCP:
		stdout, mcpErr, runErr = r.execMCP(hctx, cmd, stdin)
	}
	ex.ExitCode, ex.Duration, ex.Stdout, ex.Stderr = code, time.Since(started), stdout, stderr
	if r.Log != nil && strings.TrimSpace(stderr) != "" && code != 0 {
		fmt.Fprintf(r.Log, "[hook %s] %s\n", event, strings.TrimSpace(stderr))
	}
	timedOut := errors.Is(hctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	blockOnFailure := cmd.OnFailure == "block" && event != Stop && event != SubagentStop && event != TaskCompleted && event != TeammateIdle
	switch {
	case timedOut:
		ex.ExitCode = -1
		r.publish(ctx, ex, fmt.Sprintf("timed out after %s", timeout))
		if blockOnFailure || event == PreModelSwitch {
			return Verdict{Block: true, Reason: fmt.Sprintf("%s hook timed out; blocking because onFailure is \"block\"", event)}
		}
		return Verdict{Errors: []string{fmt.Sprintf("%s hook timed out after %s", r.describe(cmd), timeout)}}
	case runErr != nil:
		r.publish(ctx, ex, "failed: "+runErr.Error())
		if blockOnFailure {
			return Verdict{Block: true, Reason: fmt.Sprintf("%s hook failed; blocking because onFailure is \"block\": %v", event, runErr)}
		}
		return Verdict{Errors: []string{fmt.Sprintf("%s hook failed: %v", r.describe(cmd), runErr)}}
	}
	if cmd.Type == HookTypePrompt || cmd.Type == HookTypeAgent {
		v := r.parseModel(event, cmd, stdout)
		r.publish(ctx, ex, outcome(v))
		if v.Block && h.cmd.Once {
			return v
		}
		r.spent(h)
		return v
	}
	if cmd.Type == HookTypeMCP && mcpErr {
		r.publish(ctx, ex, "failed: tool reported an error")
		if blockOnFailure {
			return Verdict{Block: true, Reason: fmt.Sprintf("%s hook failed; blocking because onFailure is \"block\": %s", event, strings.TrimSpace(stdout))}
		}
		return Verdict{Errors: []string{fmt.Sprintf("%s hook tool %s/%s reported an error: %s", event, cmd.Server, cmd.Tool, strings.TrimSpace(stdout))}}
	}
	if cmd.Type == HookTypeHTTP {
		if code < 200 || code > 299 {
			r.publish(ctx, ex, fmt.Sprintf("failed: status %d", code))
			if blockOnFailure {
				return Verdict{Block: true, Reason: fmt.Sprintf("%s hook failed; blocking because onFailure is \"block\": status %d", event, code)}
			}
			return Verdict{Errors: []string{fmt.Sprintf("%s http hook %s returned status %d", event, cmd.URL, code)}}
		}
		code = 0
	}
	v, parsed, perr := r.parse(event, cmd, strings.TrimSpace(stdout))
	reason := strings.TrimSpace(stderr)
	switch {
	case code == 2:
		ex.Blocked = true
		if perr != nil && reason == "" {
			reason = perr.Error()
		}
		if v.Block && v.Reason != "" {
			reason = v.Reason
		}
		if reason == "" {
			reason = "blocked by hook"
		}
		if missingFile(reason) && strings.TrimSpace(stdout) == "" && (event == Stop || event == SubagentStop || event == TaskCompleted || (event == UserPromptSubmit && cmd.Source == SourcePlugin)) {
			r.publish(ctx, ex, "failed: "+reason)
			return Verdict{Errors: []string{fmt.Sprintf("%s hook failed: %s", r.describe(cmd), reason)}}
		}
		switch {
		case blockingEvents[event]:
			r.publish(ctx, ex, "blocked: "+reason)
			out := Verdict{Block: true, Reason: reason}
			if parsed {
				out.System, out.Title = v.System, v.Title
			}
			return out
		case feedbackEvents[event]:
			r.publish(ctx, ex, "feedback: "+reason)
			return Verdict{Context: reason}
		case event == SessionStart || event == SubagentStart || event == PostModelSwitch || event == SessionEnd || event == CwdChanged || event == FileChanged:
			r.publish(ctx, ex, "error: "+reason)
			if r.Host.Notify != nil {
				r.Host.Notify(fmt.Sprintf("%s hook error: %s", event, reason))
			}
			return Verdict{}
		}
		r.publish(ctx, ex, "exit 2 ignored")
		return Verdict{}
	case code != 0:
		r.publish(ctx, ex, fmt.Sprintf("failed: exit status %d", code))
		if blockOnFailure {
			return Verdict{Block: true, Reason: fmt.Sprintf("%s hook failed; blocking because onFailure is \"block\": %s", event, firstLine(reason))}
		}
		return Verdict{Errors: []string{fmt.Sprintf("%s hook failed with non-blocking status code %d: %s", r.describe(cmd), code, firstLine(reason))}}
	}
	if perr != nil {
		r.publish(ctx, ex, "returned bad output: "+perr.Error())
		if blockOnFailure {
			return Verdict{Block: true, Reason: fmt.Sprintf("%s hook failed; blocking because onFailure is \"block\": %v", event, perr)}
		}
		return Verdict{Errors: []string{fmt.Sprintf("%s hook returned bad output: %v", r.describe(cmd), perr)}}
	}
	if !parsed && stdout != "" {
		if contextEvents[event] {
			v.Context = strings.TrimSpace(stdout)
		} else if r.Log != nil {
			fmt.Fprintf(r.Log, "[hook %s] %s\n", event, strings.TrimSpace(stdout))
		}
	}
	if v.Block {
		ex.Blocked = true
	}
	r.publish(ctx, ex, outcome(v))
	if !v.Block {
		r.spent(h)
	}
	return v
}

// Marks a once hook spent after it ran without blocking
func (r HookRunner) spent(h chosen) {
	if h.cmd.Once && h.cmd.Source == SourceSkill {
		r.State.markSpent(h.key)
	}
}

// Word for a verdict in the event stream
func outcome(v Verdict) string {
	switch {
	case v.Block:
		return "blocked: " + v.Reason
	case v.Stop:
		return "stopped: " + v.StopReason
	case v.Decision != "":
		return string(v.Decision)
	}
	return "ok"
}

// Reports whether an error text says a file is missing
func missingFile(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "no such file or directory") || strings.Contains(lower, "not found") || strings.Contains(lower, "cannot find")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// Runs a command hook in the background, delivering its context to the agent when it ends
func (r HookRunner) async(ctx context.Context, event HookEvent, subject string, h chosen, stdin []byte) {
	cmd := h.cmd
	go func() {
		bctx := context.Background()
		var cancel context.CancelFunc = func() {}
		if cmd.AsyncRewake {
			bctx, cancel = context.WithTimeout(bctx, defaultTimeout(event, cmd))
		}
		defer cancel()
		started := time.Now()
		stdout, stderr, code, err := r.execCommand(bctx, event, cmd, stdin)
		ex := HookExecution{HookEvent: event, Type: cmd.Type, Command: r.describe(cmd), Source: cmd.Source, Owner: cmd.Owner, Subject: subject, ExitCode: code, Duration: time.Since(started), Stdout: stdout, Stderr: stderr}
		if err != nil {
			r.publish(ctx, ex, "failed: "+err.Error())
			return
		}
		if cmd.AsyncRewake && code == 2 {
			text := strings.TrimSpace(stderr)
			if text == "" {
				text = strings.TrimSpace(stdout)
			}
			r.publish(ctx, ex, "woke the agent")
			if r.Host.Deliver != nil && text != "" {
				r.Host.Deliver(reminder(string(event)+" hook", text))
			}
			return
		}
		v, parsed, _ := r.parse(event, cmd, strings.TrimSpace(stdout))
		if !parsed {
			r.publish(ctx, ex, "ok")
			return
		}
		r.publish(ctx, ex, "ok")
		if v.System != "" && r.Host.Notify != nil {
			r.Host.Notify(v.System)
		}
		if v.Context != "" && r.Host.Deliver != nil {
			r.Host.Deliver(reminder(string(event)+" hook", v.Context))
		}
	}()
}

// Words the event stream shows for a handler
func (r HookRunner) describe(cmd HookCommand) string {
	switch cmd.Type {
	case HookTypePrompt, HookTypeAgent:
		return cmd.Type + " hook"
	case HookTypeHTTP:
		return cmd.URL
	case HookTypeMCP:
		return cmd.Server + "/" + cmd.Tool
	}
	if runtime.GOOS == "windows" && cmd.Windows != "" {
		return cmd.Windows
	}
	return cmd.Command
}

// Working directory hooks run in, the first that exists of the current one, the start one, the project, home and temp
func (r HookRunner) cwd() string {
	candidates := []string{}
	if r.DirFunc != nil {
		candidates = append(candidates, r.DirFunc())
	}
	candidates = append(candidates, r.Dir, r.ProjectDir)
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home)
	}
	candidates = append(candidates, os.TempDir())
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// Replaces ${NAME} and $NAME for every variable of the hook in the text
func substitute(text string, vars map[string]string) string {
	if len(vars) == 0 {
		return text
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		text = strings.ReplaceAll(text, "${"+k+"}", vars[k])
		text = strings.ReplaceAll(text, "$"+k, vars[k])
	}
	return text
}

// Environment a hook process sees, the process one, the runner extras, the hook variables and the hook env
func (r HookRunner) environ(event HookEvent, cmd HookCommand) []string {
	env := []string{"ICEBATH_SESSION_ID=" + r.SessionID, "ICEBATH_HOOK_EVENT=" + string(event), "CLAUDE_HOOK_EVENT=" + string(event)}
	if r.ProjectDir != "" {
		env = append(env, "CLAUDE_PROJECT_DIR="+r.ProjectDir, "ICEBATH_PROJECT_DIR="+r.ProjectDir)
	}
	if r.Effort != "" {
		env = append(env, "CLAUDE_EFFORT="+r.Effort)
	}
	env = append(env, r.Env...)
	keys := make([]string, 0, len(cmd.Vars))
	for k := range cmd.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+cmd.Vars[k])
	}
	ekeys := make([]string, 0, len(cmd.Env))
	for k := range cmd.Env {
		ekeys = append(ekeys, k)
	}
	sort.Strings(ekeys)
	for _, k := range ekeys {
		env = append(env, k+"="+substitute(cmd.Env[k], cmd.Vars))
	}
	return env
}

// Runs a command hook, in the shell for a shell form and directly for an exec form
func (r HookRunner) execCommand(ctx context.Context, event HookEvent, cmd HookCommand, stdin []byte) (string, string, int, error) {
	var stdout, stderr bytes.Buffer
	dir := r.cwd()
	env := r.environ(event, cmd)
	line := cmd.Command
	if runtime.GOOS == "windows" && cmd.Windows != "" {
		line = cmd.Windows
	}
	line = substitute(line, cmd.Vars)
	if len(cmd.Args) > 0 {
		args := make([]string, 0, len(cmd.Args))
		for _, a := range cmd.Args {
			args = append(args, substitute(a, cmd.Vars))
		}
		code, err := shell.Exec{Path: line, Args: args, Dir: dir, Env: env, Stdin: bytes.NewReader(stdin), Stdout: &stdout, Stderr: &stderr}.Run(ctx)
		return stdout.String(), stderr.String(), code, err
	}
	if cmd.Shell == "powershell" {
		code, err := shell.Exec{Path: powershell(), Args: []string{"-NoProfile", "-NonInteractive", "-Command", line}, Dir: dir, Env: env, Stdin: bytes.NewReader(stdin), Stdout: &stdout, Stderr: &stderr}.Run(ctx)
		return stdout.String(), stderr.String(), code, err
	}
	code, err := shell.Cmd{Line: line, Dir: dir, Env: env, Stdin: bytes.NewReader(stdin), Stdout: &stdout, Stderr: &stderr}.Run(ctx)
	if err != nil && ctx.Err() != nil {
		return stdout.String(), stderr.String(), code, nil
	}
	return stdout.String(), stderr.String(), code, err
}

// Name of the PowerShell executable on this machine
func powershell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "pwsh"
}

// Runs a prompt or agent hook, the input JSON standing in for $ARGUMENTS or appended
func (r HookRunner) execPrompt(ctx context.Context, cmd HookCommand, stdin []byte, agentic bool) (string, error) {
	prompt := substitute(cmd.Prompt, cmd.Vars)
	prompt = strings.ReplaceAll(prompt, `\$`, "\x00")
	if strings.Contains(prompt, "$ARGUMENTS") {
		prompt = strings.ReplaceAll(prompt, "$ARGUMENTS", string(stdin))
	} else {
		prompt = prompt + "\n\n" + string(stdin)
	}
	prompt = strings.ReplaceAll(prompt, "\x00", "$")
	if agentic {
		if r.Host.Agent == nil {
			return "", errors.New("agent hooks need an agent host")
		}
		return r.Host.Agent(ctx, cmd.Model, prompt, agentHookTurns)
	}
	if r.Host.Prompt == nil {
		return "", errors.New("prompt hooks need a model host")
	}
	return r.Host.Prompt(ctx, cmd.Model, prompt)
}

// Reads the ok and reason a prompt or agent hook answered, any other text counting as ok
func (r HookRunner) parseModel(event HookEvent, cmd HookCommand, text string) Verdict {
	text = strings.TrimSpace(text)
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return Verdict{}
	}
	var out struct {
		OK     *bool  `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &out); err != nil || out.OK == nil {
		return Verdict{Errors: []string{fmt.Sprintf("%s %s hook answered without an ok field", event, cmd.Type)}}
	}
	if *out.OK {
		return Verdict{}
	}
	reason := out.Reason
	if reason == "" {
		reason = "blocked by " + cmd.Type + " hook"
	}
	if cmd.ContinueOnBlock {
		return Verdict{Context: reason}
	}
	if event == PermissionRequest && cmd.Type == HookTypePrompt {
		return Verdict{}
	}
	return Verdict{Block: true, Reason: reason}
}

// Posts the input to the URL of an http hook and reads its body
func (r HookRunner) execHTTP(ctx context.Context, cmd HookCommand, stdin []byte) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, substitute(cmd.URL, cmd.Vars), bytes.NewReader(stdin))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	allowed := map[string]bool{}
	for _, name := range cmd.AllowedEnvVars {
		allowed[name] = true
	}
	for k, v := range cmd.Headers {
		req.Header.Set(k, interpolateEnv(v, allowed))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return "", res.StatusCode, err
	}
	return string(body), res.StatusCode, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// Replaces $NAME and ${NAME} in a header with the environment value when the name is allowed, empty otherwise
func interpolateEnv(text string, allowed map[string]bool) string {
	return envRef.ReplaceAllStringFunc(text, func(m string) string {
		name := strings.Trim(strings.TrimPrefix(m, "$"), "{}")
		if allowed[name] {
			return os.Getenv(name)
		}
		return ""
	})
}

var inputRef = regexp.MustCompile(`\$\{([A-Za-z0-9_.]+)\}`)

// Calls the tool of an mcp_tool hook with its input, ${path} values drawn from the hook input
func (r HookRunner) execMCP(ctx context.Context, cmd HookCommand, stdin []byte) (string, bool, error) {
	if r.Host.MCPCall == nil {
		return "", false, errors.New("mcp_tool hooks need an MCP host")
	}
	var payload map[string]any
	if err := json.Unmarshal(stdin, &payload); err != nil {
		return "", false, err
	}
	input := map[string]any{}
	for k, v := range cmd.Input {
		if s, ok := v.(string); ok {
			input[k] = inputRef.ReplaceAllStringFunc(s, func(m string) string {
				path := strings.Trim(strings.TrimPrefix(m, "$"), "{}")
				if val, ok := lookupPath(payload, path); ok {
					return fmt.Sprint(val)
				}
				return ""
			})
			continue
		}
		input[k] = v
	}
	return r.Host.MCPCall(ctx, cmd.Server, cmd.Tool, input)
}

// Value at a dotted path inside a JSON object
func lookupPath(obj map[string]any, path string) (any, bool) {
	var cur any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	if raw, ok := cur.(json.RawMessage); ok {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			return v, true
		}
	}
	return cur, true
}

// Reports one execution into the event stream of the run that triggered it
func (r HookRunner) publish(ctx context.Context, ex HookExecution, summary string) {
	Observe(ctx, Event{Type: EventHook, Text: fmt.Sprintf("%s %q %s", ex.HookEvent, ex.Command, summary), Data: ex, Duration: ex.Duration})
}

// Reports whether standard output is one JSON object, with nothing but whitespace around it
func looksJSON(stdout string) bool {
	return strings.HasPrefix(stdout, "{") && strings.HasSuffix(stdout, "}")
}

// Reads the JSON a hook printed, reporting whether it parsed and refusing fields the event does not take
func (r HookRunner) parse(event HookEvent, cmd HookCommand, stdout string) (Verdict, bool, error) {
	if stdout == "" || !looksJSON(stdout) {
		return Verdict{}, false, nil
	}
	var out hookOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		return Verdict{}, false, fmt.Errorf("output is not a JSON object: %w", err)
	}
	var v Verdict
	if out.TerminalSequence != "" && r.Host.Terminal != nil && validSequence(out.TerminalSequence) {
		r.Host.Terminal(out.TerminalSequence)
	}
	if discardEvents[event] {
		return Verdict{}, true, nil
	}
	if out.Continue != nil && !*out.Continue {
		v.Stop, v.StopReason = true, out.StopReason
	}
	v.System = r.cap(out.SystemMessage, "system-message")
	switch out.Decision {
	case "":
	case "block":
		if decisionEvents[event] {
			v.Block, v.Reason = true, out.Reason
		}
		if event == PreToolUse {
			v.Decision = policy.Deny
		}
	case "approve":
		if event == PreToolUse {
			v.Decision = policy.Allow
		}
	default:
		return Verdict{}, true, fmt.Errorf("unsupported decision %q", out.Decision)
	}
	if out.AdditionalContext != "" {
		v.Context = r.cap(out.AdditionalContext, "context")
	}
	if len(out.UpdatedInput) > 0 && event == PreToolUse {
		v.Input = out.UpdatedInput
	}
	hs := out.HookSpecific
	if hs == nil {
		return v, true, nil
	}
	if hs.AdditionalContext != "" {
		v.Context = r.cap(hs.AdditionalContext, "context")
	}
	switch event {
	case PreToolUse:
		switch hs.PermissionDecision {
		case "":
		case "allow":
			v.Decision = policy.Allow
		case "ask", "defer":
			v.Decision = policy.Ask
		case "deny":
			v.Block, v.Reason = true, hs.PermissionDecisionReason
			v.Decision = policy.Deny
		default:
			return Verdict{}, true, fmt.Errorf("unsupported permission decision %q", hs.PermissionDecision)
		}
		if len(hs.UpdatedInput) > 0 && hs.PermissionDecision != "defer" {
			v.Input = hs.UpdatedInput
		}
	case PreModelSwitch:
		switch hs.PermissionDecision {
		case "", "allow":
		case "ask":
			v.Decision = policy.Ask
		case "deny":
			v.Block, v.Reason = true, hs.PermissionDecisionReason
		default:
			return Verdict{}, true, fmt.Errorf("unsupported permission decision %q", hs.PermissionDecision)
		}
	case PostToolUse:
		if len(hs.UpdatedToolOutput) > 0 {
			v.Response = rawText(hs.UpdatedToolOutput)
		}
		if len(hs.UpdatedMCPToolOutput) > 0 {
			v.Response = rawText(hs.UpdatedMCPToolOutput)
		}
	case PermissionRequest:
		if hs.Decision != nil {
			switch hs.Decision.Behavior {
			case "allow", "deny":
			default:
				return Verdict{}, true, fmt.Errorf("unsupported permission behavior %q", hs.Decision.Behavior)
			}
			v.Behavior, v.Message, v.Interrupt, v.Updates = hs.Decision.Behavior, hs.Decision.Message, hs.Decision.Interrupt, hs.Decision.UpdatedPermissions
			if hs.Decision.Behavior == "allow" && len(hs.Decision.UpdatedInput) > 0 {
				v.Input = hs.Decision.UpdatedInput
			}
		}
	case PermissionDenied:
		v.Retry = hs.Retry
	case MessageDisplay:
		v.Display = hs.DisplayContent
	case SessionStart:
		v.Title, v.InitialMessage, v.WatchPaths, v.ReloadSkills = hs.SessionTitle, r.cap(hs.InitialUserMessage, "initial-message"), hs.WatchPaths, hs.ReloadSkills
	case UserPromptSubmit:
		v.Title, v.SuppressOriginal = hs.SessionTitle, hs.SuppressOriginalPrompt
	case CwdChanged:
		v.WatchPaths = hs.WatchPaths
	case Elicitation, ElicitationResult:
		v.Action, v.Content = hs.Action, hs.Content
	case WorktreeCreate:
		v.Path = hs.Path
	}
	if len(v.Input) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v.Input, &obj); err != nil {
			return Verdict{}, true, errors.New("updated input is not a JSON object")
		}
	}
	return v, true, nil
}

// Text of an updated output, a string as is and anything else as its JSON
func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// Text under the cap, or the path of the file it was written to with a preview
func (r HookRunner) cap(text, kind string) string {
	if len(text) <= contextCap {
		return text
	}
	dir := r.Scratch
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, fmt.Sprintf("hook-%s-%d.txt", kind, time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if err := os.WriteFile(path, []byte(text), 0o600); err == nil {
			return fmt.Sprintf("Hook output of %d characters was saved to %s. It begins:\n%s", len(text), path, text[:contextPreview])
		}
	}
	return text[:contextCap]
}

// Reports whether the sequence holds only the window title, notification and link escapes a terminal may take
func validSequence(seq string) bool {
	const esc, bel = "\x1b]", "\x07"
	for seq != "" {
		if !strings.HasPrefix(seq, esc) {
			return false
		}
		rest := seq[len(esc):]
		end := strings.IndexAny(rest, bel)
		st := strings.Index(rest, "\x1b\\")
		switch {
		case end >= 0 && (st < 0 || end < st):
			seq = rest[end+1:]
			rest = rest[:end]
		case st >= 0:
			seq = rest[st+2:]
			rest = rest[:st]
		default:
			return false
		}
		code, _, _ := strings.Cut(rest, ";")
		switch code {
		case "0", "1", "2", "9", "99", "777":
		default:
			return false
		}
	}
	return true
}

// Tool arguments as a JSON object, an empty object for no arguments
func rawObject(args json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(args)) == 0 {
		return json.RawMessage("{}")
	}
	return args
}

var promptSeq int64
var promptMu sync.Mutex

// Fresh id for a user prompt
func newPromptID() string {
	promptMu.Lock()
	defer promptMu.Unlock()
	promptSeq++
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(promptSeq, 36)
}

var _ Hook = HookRunner{}
