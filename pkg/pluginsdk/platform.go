package pluginsdk

import (
	"context"
	"fmt"
	"time"
)

// Messaging platform a plugin adds to the gateway, every hook optional except the adapter
type Platform struct {
	Name        string
	Label       string
	RequiredEnv []string
	InstallHint string
	// Plain registry fields such as allowed_users_env, max_message_length, emoji, platform_hint, docs_url or display_tier
	Fields               map[string]any
	Check                func(ctx context.Context) bool
	ValidateConfig       func(ctx context.Context, config map[string]any) (bool, error)
	IsConnected          func(ctx context.Context, config map[string]any) (bool, error)
	ConfigConnected      func(ctx context.Context, config map[string]any) (bool, error)
	EnsureDeps           func(ctx context.Context) (bool, error)
	Setup                func(ctx context.Context, ui *SetupUI) error
	EnvEnablement        func(ctx context.Context) (map[string]any, error)
	ApplyYAMLConfig      func(ctx context.Context, yamlConfig, block map[string]any) (YAMLConfigChange, error)
	ParseTargetRef       func(ctx context.Context, ref string) (chatID, threadID string, ok bool, err error)
	ValidateTargetRef    func(ctx context.Context, ref string) (any, error)
	TargetRefSyntax      func(ctx context.Context, ref string) (TargetRefSyntax, error)
	SendMessage          func(ctx context.Context, req ToolSendRequest) (map[string]any, error)
	StandaloneSend       func(ctx context.Context, req StandaloneSendRequest) (map[string]any, error)
	StandaloneSendPolicy *StandaloneSendPolicy
	EnvValueRules        map[string]EnvValueRule
	EnvOverrides         []EnvStep
	Adapter              func(ctx context.Context, config map[string]any, gw *Gateway) (*Adapter, error)
}

// Config change an apply_yaml_config hook made, the two documents after the change and its result
type YAMLConfigChange struct {
	YAMLConfig map[string]any
	Block      map[string]any
	Result     map[string]any
}

// Verdict of a target syntax parser, explicit when the ref named this platform outright
type TargetRefSyntax struct {
	Matched     bool
	NotExplicit bool
	ChatID      string
	HasChatID   bool
	ThreadID    string
	HasThreadID bool
}

// Send the send_message tool asks of a platform without a running adapter
type ToolSendRequest struct {
	Args     map[string]any
	ChatID   string
	Platform string
	Config   map[string]any
}

// Send cron jobs and icebath send make with no running gateway
type StandaloneSendRequest struct {
	Config  map[string]any
	ChatID  string
	Message string
	Options map[string]any
}

// How cron and the send command use a platform's standalone sender
type StandaloneSendPolicy struct {
	Label                  string
	Media                  bool
	AllSends               bool
	Captionable            bool
	EmptyListBetweenChunks bool
	ForwardForceDocument   bool
	Mentions               bool
	TextDirect             bool
}

// Rule an env value must pass for the platform to enable from env, with the message shown when it fails
type EnvValueRule struct {
	Accepts func(ctx context.Context, value string) bool
	Message string
}

// One env to config step, exactly one of the forms set
type EnvStep struct {
	Credentials *EnvCredentials
	HomeChannel *EnvHomeChannel
	ReplyMode   *EnvReplyMode
	// Rewrites the gateway config document and returns it changed
	Apply func(ctx context.Context, gatewayConfig map[string]any) (map[string]any, error)
}

// Enables a platform from credential env vars and copies the listed env vars into its config
type EnvCredentials struct {
	Platform         string
	Credentials      [][]string
	TokenEnv         string
	Fixed            []EnvSpec
	Optional         []EnvSpec
	OptionalStripped []EnvSpec
	WarnMissingEnv   string
	WarnMissingText  string
	HomeEnv          string
	HomeStrip        bool
	// Runs after the platform enabled, given the gateway config document and returning it changed
	Then func(ctx context.Context, gatewayConfig map[string]any) (map[string]any, error)
}

// One env var copied into the platform extra config, AnyOf naming alternatives read in order
type EnvSpec struct {
	Key     string
	Env     string
	AnyOf   []string
	Default string
	Integer bool
	Convert func(ctx context.Context, value string) (any, error)
}

// Home channel step reading <EnvBase>_HOME_CHANNEL and its name
type EnvHomeChannel struct {
	Platform string
	EnvBase  string
	Strip    bool
}

// Reply mode step reading one env var
type EnvReplyMode struct {
	Platform string
	Env      string
}

// Terminal the setup wizard talks to the user through
type SetupUI struct {
	prompt, secret, choose, print *HostFunc
}

// Asks for a line of text, the default answering an empty line
func (u *SetupUI) Prompt(ctx context.Context, question, def string) (string, error) {
	v, err := u.prompt.Call(ctx, []any{question, def}, nil)
	return text(v), err
}

// Asks for a secret without echoing it
func (u *SetupUI) Secret(ctx context.Context, question string) (string, error) {
	v, err := u.secret.Call(ctx, []any{question}, nil)
	return text(v), err
}

// Asks to pick one of the options and returns its index
func (u *SetupUI) Choose(ctx context.Context, question string, options []string, def int) (int, error) {
	v, err := u.choose.Call(ctx, []any{question, options, int64(def)}, nil)
	n, _ := integer(v)
	return int(n), err
}

// Prints one line to the user
func (u *SetupUI) Print(ctx context.Context, line string) error {
	_, err := u.print.Call(ctx, []any{line}, nil)
	return err
}

// Gateway one adapter delivers inbound messages and connection state to
type Gateway struct {
	handle, run, lifecycle *HostFunc
}

// Hands an inbound message to the gateway pipeline, with its busy session queueing and access checks
func (g *Gateway) HandleMessage(ctx context.Context, event MessageEvent) error {
	_, err := g.handle.Call(ctx, []any{event.Map()}, nil)
	return err
}

// Runs the gateway message handler directly for an adapter that does its own dispatch
func (g *Gateway) RunHandler(ctx context.Context, event MessageEvent) (any, error) {
	return g.run.Call(ctx, []any{event.Map()}, nil)
}

// Marks the adapter connected, listenerBase naming its webhook listener when it has one
func (g *Gateway) MarkConnected(ctx context.Context, listenerBase string) error {
	_, err := g.lifecycle.Call(ctx, []any{"mark_connected"}, map[string]any{"listener_base": listenerBase})
	return err
}

// Marks the adapter disconnected
func (g *Gateway) MarkDisconnected(ctx context.Context) error {
	_, err := g.lifecycle.Call(ctx, []any{"mark_disconnected"}, nil)
	return err
}

// Records a fatal connection error, retryable when the gateway may reconnect
func (g *Gateway) SetFatalError(ctx context.Context, code, message string, retryable bool) error {
	_, err := g.lifecycle.Call(ctx, []any{"set_fatal_error"}, map[string]any{"code": code, "message": message, "retryable": retryable})
	return err
}

// Chat a message arrived in, the routing identity of a session
type SessionSource struct {
	Platform              string
	ChatID                string
	ChatName              string
	ChatType              string
	UserID                string
	UserName              string
	ThreadID              string
	ChatTopic             string
	UserIDAlt             string
	ChatIDAlt             string
	ScopeID               string
	ParentChatID          string
	MessageID             string
	Profile               string
	AutoThreadCreated     bool
	AutoThreadInitialName string
	ProspectiveThreadID   string
}

// One inbound or replayed message
type MessageEvent struct {
	Text                string
	MessageType         string
	Source              *SessionSource
	UserID              string
	UserName            string
	MessageID           string
	LedgerMessageID     string
	ReplyAnchorOverride string
	PlatformUpdateID    *int64
	RawMessage          any
	MediaURLs           []string
	MediaTypes          []string
	MediaFilenames      []string
	MediaTextInlined    []*bool
	ReplyToMessageID    string
	ReplyToText         string
	ReplyToAuthorID     string
	ReplyToAuthorName   string
	ReplyToIsOwnMessage bool
	PromptResponse      map[string]any
	AutoSkill           any
	ChannelPrompt       string
	ChannelContext      string
	Internal            bool
	Metadata            map[string]any
	Timestamp           time.Time
	AllowGatewayControl bool
	ReplyExpected       *bool
	InheritChannelPin   bool
	EditRevision        string
}

// Outcome of one send
type SendResult struct {
	Success                bool
	MessageID              any
	Error                  any
	RawResponse            any
	Retryable              bool
	RetryAfter             *float64
	ContinuationMessageIDs []any
	ErrorKind              string
}

// Text send of an adapter
type SendRequest struct {
	ChatID   string
	Content  string
	ReplyTo  string
	Metadata map[string]any
}

// Media send of an adapter, URL for remote images and Path for local files
type MediaRequest struct {
	ChatID   string
	URL      string
	Path     string
	Caption  string
	FileName string
	ReplyTo  string
	Metadata map[string]any
	Extra    map[string]any
}

// One image of a gallery send
type ImageRef struct {
	URL string
	Alt string
}

// Approval a dangerous command waits on
type ExecApprovalRequest struct {
	ChatID         string
	Command        string
	SessionKey     string
	Description    string
	Metadata       map[string]any
	AllowPermanent bool
	AllowSession   bool
	SmartDenied    bool
	RequestID      string
}

// Rendered approval prompt with its action buttons
type ExecApprovalPrompt struct {
	ChatID      string
	SessionKey  string
	Text        string
	Actions     []ApprovalAction
	Command     string
	Description string
	SmartDenied bool
	Metadata    map[string]any
	RequestID   string
}

// One button of an approval prompt
type ApprovalAction struct {
	Label  string
	Choice string
	Style  string
}

// Warning a platform shows the user, LogicalPlatform naming the surface it renders on
type WarningRequest struct {
	ChatID          string
	Content         string
	Hidden          string
	ReplyTo         string
	Metadata        map[string]any
	ShownMetadata   map[string]any
	Caption         string
	LogicalPlatform string
}

// One streamed reply event, Kind one of message_chunk, message_stop, commentary, tool_call_chunk, tool_call_finished, long_tool_hint or gateway_notice
type StreamEvent struct {
	Kind     string
	Text     string
	Final    bool
	ToolName string
	Preview  string
	Args     map[string]any
	Index    int
	Duration float64
	OK       bool
	Notice   string
	Extra    map[string]any
}

// Sink a stream renderer writes through
type StreamSink struct {
	delta, segment, commentary *HostFunc
}

// Appends text to the current message
func (s *StreamSink) Delta(ctx context.Context, text string) error {
	_, err := s.delta.Call(ctx, []any{text}, nil)
	return err
}

// Ends the current message segment
func (s *StreamSink) SegmentBreak(ctx context.Context) error {
	_, err := s.segment.Call(ctx, nil, nil)
	return err
}

// Writes commentary beside the message
func (s *StreamSink) Commentary(ctx context.Context, text string) error {
	_, err := s.commentary.Call(ctx, []any{text}, nil)
	return err
}

// Preview of one tool call a tool progress line shows
type ToolPreview struct {
	Text      string
	Truncated bool
	URL       string
}

// PCM format of streamed speech
type AudioFormat struct {
	SampleRate  int
	Channels    int
	SampleWidth int
}

// Streaming speech playback an adapter began, Audible and Aborted kept in step with the gateway
type TTSStream struct {
	ChatID  string
	Format  AudioFormat
	Audible bool
	Aborted bool
	// Platform state the adapter keeps for this playback
	State any
}

// Adapter of one platform connection, every hook optional except Connect, Disconnect and Send
type Adapter struct {
	Name                       string
	Connect                    func(ctx context.Context, reconnect bool) (bool, error)
	Disconnect                 func(ctx context.Context) error
	Send                       func(ctx context.Context, req SendRequest) (SendResult, error)
	GetChatInfo                func(ctx context.Context, chatID string) (map[string]any, error)
	EditMessage                func(ctx context.Context, chatID, messageID, content string, finalize bool) (SendResult, error)
	DeleteMessage              func(ctx context.Context, chatID, messageID string) (bool, error)
	CreateHandoffThread        func(ctx context.Context, parentChatID, name string) (threadID string, ok bool, err error)
	SendTyping                 func(ctx context.Context, chatID string, metadata map[string]any) error
	StopTyping                 func(ctx context.Context, chatID string, metadata map[string]any) error
	SendImage                  func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendAnimation              func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendImageFile              func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendVoice                  func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendVideo                  func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendDocument               func(ctx context.Context, req MediaRequest) (SendResult, error)
	PlayTTS                    func(ctx context.Context, req MediaRequest) (SendResult, error)
	SendMultipleImages         func(ctx context.Context, chatID string, images []ImageRef, metadata map[string]any, humanDelay float64) (SendResult, error)
	SendPrivateNotice          func(ctx context.Context, chatID, userID, content, replyTo string, metadata map[string]any) (SendResult, error)
	SendDraft                  func(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) (SendResult, error)
	SendExecApproval           func(ctx context.Context, req ExecApprovalRequest) (SendResult, error)
	SendSlashConfirm           func(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (SendResult, error)
	SendClarify                func(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (SendResult, error)
	FormatMessage              func(ctx context.Context, content string) (string, error)
	MaxMessageLengthForChat    func(ctx context.Context, chatID string) (int, error)
	MessageLength              func(ctx context.Context, chatID, text string) (int, error)
	EnforcesOwnAccessPolicy    func(ctx context.Context) bool
	AuthorizationIsUpstream    func(ctx context.Context) bool
	SupportsDraftStreaming     func(ctx context.Context, chatType string, metadata map[string]any, chatID string) bool
	PrefersFreshFinalStreaming func(ctx context.Context, content string, metadata map[string]any, chatID string) bool
	StreamingOverflowLimit     func(ctx context.Context) (limit int, ok bool)
	SendPathDegraded           func(ctx context.Context) bool
	IsConnected                func(ctx context.Context) bool
	PrepareTTSText             func(ctx context.Context, text string) (string, error)
	OnProcessingStart          func(ctx context.Context, event MessageEvent) error
	OnProcessingComplete       func(ctx context.Context, event MessageEvent, outcome string) error
	HandleMessage              func(ctx context.Context, event MessageEvent) error
	ToolsetsForSource          func(ctx context.Context, source SessionSource) (toolsets []string, ok bool)
	CancelBackgroundTasks      func(ctx context.Context) error
	RenderMessageEvent         func(ctx context.Context, event StreamEvent, sink *StreamSink) error
	FormatToolEvent            func(ctx context.Context, event StreamEvent, mode string, previewMaxLen int) (string, bool, error)
	FormatToolPreview          func(ctx context.Context, preview ToolPreview) (string, error)
	TextBatchKey               func(ctx context.Context, event MessageEvent) (string, error)
	EnqueueTextEvent           func(ctx context.Context, event MessageEvent) error
	TextBatchDelay             func(ctx context.Context, pending *MessageEvent) float64
	PopTextBatch               func(ctx context.Context, key string) (*MessageEvent, error)
	DispatchTextBatch          func(ctx context.Context, event MessageEvent) error
	FlushTextBatch             func(ctx context.Context, key string) error
	ApprovalHeader             func(ctx context.Context) string
	ApprovalReasonLabel        func(ctx context.Context) string
	ApprovalSmartDenyLine      func(ctx context.Context) string
	ApprovalActionLabels       func(ctx context.Context) map[string]string
	ApprovalEscape             func(ctx context.Context, text string) string
	ApprovalCommandBudget      func(ctx context.Context, description string, smartDenied bool) int
	SendExecApprovalPrompt     func(ctx context.Context, prompt ExecApprovalPrompt) (SendResult, error)
	EmitWarning                func(ctx context.Context, req WarningRequest) (*SendResult, error)
	EmitMediaWarning           func(ctx context.Context, req WarningRequest) (SendResult, error)
	WarningText                func(ctx context.Context, req WarningRequest) (text string, ok bool, err error)
	WarningNotificationsOn     func(ctx context.Context, req WarningRequest) bool
	SupportsStreamingTTS       func(ctx context.Context, chatID string, format AudioFormat) bool
	BeginStreamingTTS          func(ctx context.Context, chatID string, format AudioFormat, metadata map[string]any) (*TTSStream, error)
	WriteStreamingTTS          func(ctx context.Context, stream *TTSStream, chunk []byte) error
	FinishStreamingTTS         func(ctx context.Context, stream *TTSStream, interrupted bool) error
	AbortStreamingTTS          func(ctx context.Context, stream *TTSStream, errText string) error
	SendRetryIsFinal           func(ctx context.Context, result SendResult) bool
}

func (h *host) RegisterPlatform(platform Platform) (*Handle, error) {
	if platform.Adapter == nil {
		return nil, fmt.Errorf("platform %q has no adapter", platform.Name)
	}
	kwargs := map[string]any{}
	for k, v := range platform.Fields {
		kwargs[k] = v
	}
	p := h.peer
	kwargs["name"], kwargs["label"], kwargs["install_hint"] = platform.Name, platform.Label, platform.InstallHint
	if len(platform.RequiredEnv) > 0 {
		kwargs["required_env"] = platform.RequiredEnv
	}
	check := platform.Check
	if check == nil {
		check = func(context.Context) bool { return true }
	}
	kwargs["check_fn"] = h.checkRef(platform.Name+".check", check)
	kwargs["adapter_factory"] = p.funcRef(platform.Name+".adapter", func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
		gw := &Gateway{}
		gw.handle, _ = kwargs["handle_message"].(*HostFunc)
		gw.run, _ = kwargs["run_handler"].(*HostFunc)
		gw.lifecycle, _ = kwargs["lifecycle"].(*HostFunc)
		if gw.handle == nil || gw.run == nil || gw.lifecycle == nil {
			return nil, fmt.Errorf("platform %q adapter factory received no gateway callbacks", platform.Name)
		}
		adapter, err := platform.Adapter(ctx, argMap(args, 0), gw)
		if err != nil || adapter == nil {
			return nil, err
		}
		return adapterObject(p, adapter), nil
	})
	configHook := func(name string, fn func(ctx context.Context, config map[string]any) (bool, error)) {
		if fn != nil {
			kwargs[name] = p.funcRef(platform.Name+"."+name, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
				return fn(ctx, argMap(args, 0))
			})
		}
	}
	configHook("validate_config", platform.ValidateConfig)
	configHook("is_connected", platform.IsConnected)
	configHook("config_connected_fn", platform.ConfigConnected)
	if platform.EnsureDeps != nil {
		kwargs["ensure_deps_fn"] = p.funcRef(platform.Name+".ensure_deps", func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
			return platform.EnsureDeps(ctx)
		})
	}
	if platform.Setup != nil {
		kwargs["setup_fn"] = p.funcRef(platform.Name+".setup", func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
			ui := &SetupUI{}
			ui.prompt, _ = kwargs["prompt"].(*HostFunc)
			ui.secret, _ = kwargs["secret"].(*HostFunc)
			ui.choose, _ = kwargs["choose"].(*HostFunc)
			ui.print, _ = kwargs["print"].(*HostFunc)
			if ui.prompt == nil || ui.secret == nil || ui.choose == nil || ui.print == nil {
				return nil, fmt.Errorf("platform %q setup received no terminal callbacks", platform.Name)
			}
			return nil, platform.Setup(ctx, ui)
		})
	}
	if platform.EnvEnablement != nil {
		kwargs["env_enablement_fn"] = p.funcRef(platform.Name+".env_enablement", func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
			return platform.EnvEnablement(ctx)
		})
	}
	if platform.ApplyYAMLConfig != nil {
		kwargs["apply_yaml_config_fn"] = p.funcRef(platform.Name+".apply_yaml_config", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			change, err := platform.ApplyYAMLConfig(ctx, argMap(args, 0), argMap(args, 1))
			if err != nil {
				return nil, err
			}
			return map[string]any{"yaml_config": change.YAMLConfig, "block": change.Block, "result": change.Result}, nil
		})
	}
	if platform.ParseTargetRef != nil {
		kwargs["parse_target_ref_fn"] = p.funcRef(platform.Name+".parse_target_ref", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			chat, thread, ok, err := platform.ParseTargetRef(ctx, argText(args, 0))
			if err != nil || !ok {
				return nil, err
			}
			var threadValue any
			if thread != "" {
				threadValue = thread
			}
			return map[string]any{"chat_id": chat, "thread_id": threadValue}, nil
		})
	}
	if platform.ValidateTargetRef != nil {
		kwargs["validate_target_ref_fn"] = p.funcRef(platform.Name+".validate_target_ref", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			return platform.ValidateTargetRef(ctx, argText(args, 0))
		})
	}
	if platform.TargetRefSyntax != nil {
		kwargs["target_ref_syntax_fn"] = p.funcRef(platform.Name+".target_ref_syntax", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			syntax, err := platform.TargetRefSyntax(ctx, argText(args, 0))
			if err != nil {
				return nil, err
			}
			out := map[string]any{"matched": syntax.Matched, "not_explicit": syntax.NotExplicit}
			if syntax.HasChatID {
				out["chat_id"] = syntax.ChatID
			}
			if syntax.HasThreadID {
				out["thread_id"] = syntax.ThreadID
			}
			return out, nil
		})
	}
	if platform.SendMessage != nil {
		kwargs["send_message_handler"] = p.funcRef(platform.Name+".send_message", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			return platform.SendMessage(ctx, ToolSendRequest{Args: argMap(args, 0), ChatID: argText(args, 1), Platform: argText(args, 2), Config: argMap(args, 3)})
		})
	}
	if platform.StandaloneSend != nil {
		kwargs["standalone_sender_fn"] = p.funcRef(platform.Name+".standalone_send", func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
			return platform.StandaloneSend(ctx, StandaloneSendRequest{Config: argMap(args, 0), ChatID: argText(args, 1), Message: argText(args, 2), Options: mapOrEmpty(kwargs)})
		})
	}
	if s := platform.StandaloneSendPolicy; s != nil {
		kwargs["standalone_send"] = map[string]any{"label": s.Label, "media": s.Media, "all_sends": s.AllSends, "captionable": s.Captionable,
			"empty_list_between_chunks": s.EmptyListBetweenChunks, "forward_force_document": s.ForwardForceDocument,
			"mentions": s.Mentions, "text_direct": s.TextDirect}
	}
	if len(platform.EnvValueRules) > 0 {
		rules := map[string]any{}
		for env, rule := range platform.EnvValueRules {
			accepts := rule.Accepts
			if accepts == nil {
				return nil, fmt.Errorf("platform %q env value rule %s has no accepts function", platform.Name, env)
			}
			rules[env] = map[string]any{"message": rule.Message, "accepts": p.funcRef(platform.Name+"."+env, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
				return accepts(ctx, argText(args, 0)), nil
			})}
		}
		kwargs["env_value_rules"] = rules
	}
	if len(platform.EnvOverrides) > 0 {
		steps := make([]any, 0, len(platform.EnvOverrides))
		for i, step := range platform.EnvOverrides {
			wire, err := envStepWire(p, fmt.Sprintf("%s.env_step_%d", platform.Name, i), step)
			if err != nil {
				return nil, fmt.Errorf("platform %q env step %d: %w", platform.Name, i, err)
			}
			steps = append(steps, wire)
		}
		kwargs["env_overrides"] = steps
	}
	return h.register("register_platform", nil, kwargs)
}

// Plain wire form of one env step
func envStepWire(p *peer, name string, step EnvStep) (map[string]any, error) {
	switch {
	case step.Credentials != nil:
		c := step.Credentials
		creds := make([]any, len(c.Credentials))
		for i, group := range c.Credentials {
			creds[i] = stringsAny(group)
		}
		specs := func(rows []EnvSpec) ([]any, error) {
			out := make([]any, 0, len(rows))
			for j, s := range rows {
				row := map[string]any{"key": s.Key, "env": s.Env, "any_of": stringsAny(s.AnyOf), "default": s.Default, "integer": s.Integer}
				if s.Convert != nil {
					convert := s.Convert
					row["convert"] = p.funcRef(fmt.Sprintf("%s.%s_%d", name, s.Key, j), func(ctx context.Context, args []any, _ map[string]any) (any, error) {
						return convert(ctx, argText(args, 0))
					})
				}
				if s.Env == "" && len(s.AnyOf) == 0 {
					return nil, fmt.Errorf("env spec %s names no env var", s.Key)
				}
				out = append(out, row)
			}
			return out, nil
		}
		fixed, err := specs(c.Fixed)
		if err != nil {
			return nil, err
		}
		optional, err := specs(c.Optional)
		if err != nil {
			return nil, err
		}
		stripped, err := specs(c.OptionalStripped)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"kind": "credentials", "platform": c.Platform, "credentials": creds, "token_env": c.TokenEnv,
			"fixed": fixed, "optional": optional, "optional_stripped": stripped, "warn_missing_env": c.WarnMissingEnv,
			"warn_missing_text": c.WarnMissingText, "home_env": c.HomeEnv, "home_strip": c.HomeStrip}
		if c.Then != nil {
			then := c.Then
			out["then"] = p.funcRef(name+".then", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
				return then(ctx, argMap(args, 0))
			})
		}
		return out, nil
	case step.HomeChannel != nil:
		s := step.HomeChannel
		return map[string]any{"kind": "home_channel", "platform": s.Platform, "env_base": s.EnvBase, "strip": s.Strip}, nil
	case step.ReplyMode != nil:
		s := step.ReplyMode
		return map[string]any{"kind": "reply_mode", "platform": s.Platform, "env": s.Env}, nil
	case step.Apply != nil:
		apply := step.Apply
		return map[string]any{"kind": "apply", "apply": p.funcRef(name, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			return apply(ctx, argMap(args, 0))
		})}, nil
	}
	return nil, fmt.Errorf("the step sets none of Credentials, HomeChannel, ReplyMode or Apply")
}

// Mapping argument at a position, empty when absent
func argMap(args []any, i int) map[string]any {
	if i < len(args) {
		if m, ok := args[i].(map[string]any); ok {
			return m
		}
	}
	return map[string]any{}
}

// String argument at a position, empty when absent
func argText(args []any, i int) string {
	if i < len(args) {
		return text(args[i])
	}
	return ""
}

// Flag argument by keyword
func argFlag(kwargs map[string]any, name string) bool {
	b, _ := kwargs[name].(bool)
	return b
}

// Wire fields of a session source
func (s SessionSource) Map() map[string]any {
	out := map[string]any{"platform": s.Platform, "chat_id": s.ChatID, "chat_type": s.ChatType, "auto_thread_created": s.AutoThreadCreated}
	for k, v := range map[string]string{"chat_name": s.ChatName, "user_id": s.UserID, "user_name": s.UserName, "thread_id": s.ThreadID,
		"chat_topic": s.ChatTopic, "user_id_alt": s.UserIDAlt, "chat_id_alt": s.ChatIDAlt, "scope_id": s.ScopeID, "parent_chat_id": s.ParentChatID,
		"message_id": s.MessageID, "profile": s.Profile, "auto_thread_initial_name": s.AutoThreadInitialName, "prospective_thread_id": s.ProspectiveThreadID} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// Session source of wire fields
func sourceOf(m map[string]any) SessionSource {
	auto, _ := m["auto_thread_created"].(bool)
	return SessionSource{Platform: text(m["platform"]), ChatID: text(m["chat_id"]), ChatName: text(m["chat_name"]), ChatType: text(m["chat_type"]),
		UserID: text(m["user_id"]), UserName: text(m["user_name"]), ThreadID: text(m["thread_id"]), ChatTopic: text(m["chat_topic"]),
		UserIDAlt: text(m["user_id_alt"]), ChatIDAlt: text(m["chat_id_alt"]), ScopeID: text(m["scope_id"]), ParentChatID: text(m["parent_chat_id"]),
		MessageID: text(m["message_id"]), Profile: text(m["profile"]), AutoThreadCreated: auto,
		AutoThreadInitialName: text(m["auto_thread_initial_name"]), ProspectiveThreadID: text(m["prospective_thread_id"])}
}

// Wire fields of a message event, the timestamp in RFC 3339
func (e MessageEvent) Map() map[string]any {
	out := map[string]any{"text": e.Text, "message_type": e.MessageType, "raw_message": e.RawMessage, "media_urls": stringsAny(e.MediaURLs),
		"media_types": stringsAny(e.MediaTypes), "media_filenames": stringsAny(e.MediaFilenames), "reply_to_is_own_message": e.ReplyToIsOwnMessage,
		"auto_skill": e.AutoSkill, "internal": e.Internal, "metadata": mapOrEmpty(e.Metadata), "allow_gateway_control": e.AllowGatewayControl,
		"inherit_channel_pin": e.InheritChannelPin}
	if e.MessageType == "" {
		out["message_type"] = "text"
	}
	if e.Source != nil {
		out["source"] = e.Source.Map()
	}
	for k, v := range map[string]string{"user_id": e.UserID, "user_name": e.UserName, "message_id": e.MessageID, "ledger_message_id": e.LedgerMessageID,
		"reply_anchor_override": e.ReplyAnchorOverride, "reply_to_message_id": e.ReplyToMessageID, "reply_to_text": e.ReplyToText,
		"reply_to_author_id": e.ReplyToAuthorID, "reply_to_author_name": e.ReplyToAuthorName, "channel_prompt": e.ChannelPrompt,
		"channel_context": e.ChannelContext, "edit_revision": e.EditRevision} {
		if v != "" {
			out[k] = v
		}
	}
	if e.PlatformUpdateID != nil {
		out["platform_update_id"] = *e.PlatformUpdateID
	}
	if e.ReplyExpected != nil {
		out["reply_expected"] = *e.ReplyExpected
	}
	if e.PromptResponse != nil {
		out["prompt_response"] = e.PromptResponse
	}
	if len(e.MediaTextInlined) > 0 {
		inlined := make([]any, len(e.MediaTextInlined))
		for i, b := range e.MediaTextInlined {
			if b != nil {
				inlined[i] = *b
			}
		}
		out["media_text_inlined"] = inlined
	}
	if !e.Timestamp.IsZero() {
		out["timestamp"] = e.Timestamp.Format(time.RFC3339Nano)
	}
	return out
}

// Message event of wire fields
func eventOf(m map[string]any) MessageEvent {
	e := MessageEvent{Text: text(m["text"]), MessageType: text(m["message_type"]), UserID: text(m["user_id"]), UserName: text(m["user_name"]),
		MessageID: text(m["message_id"]), LedgerMessageID: text(m["ledger_message_id"]), ReplyAnchorOverride: text(m["reply_anchor_override"]),
		RawMessage: m["raw_message"], MediaURLs: stringsOf(m["media_urls"]), MediaTypes: stringsOf(m["media_types"]),
		MediaFilenames: stringsOf(m["media_filenames"]), ReplyToMessageID: text(m["reply_to_message_id"]), ReplyToText: text(m["reply_to_text"]),
		ReplyToAuthorID: text(m["reply_to_author_id"]), ReplyToAuthorName: text(m["reply_to_author_name"]),
		ReplyToIsOwnMessage: argFlag(m, "reply_to_is_own_message"), AutoSkill: m["auto_skill"], ChannelPrompt: text(m["channel_prompt"]),
		ChannelContext: text(m["channel_context"]), Internal: argFlag(m, "internal"), AllowGatewayControl: argFlag(m, "allow_gateway_control"),
		InheritChannelPin: argFlag(m, "inherit_channel_pin"), EditRevision: text(m["edit_revision"])}
	e.Metadata, _ = m["metadata"].(map[string]any)
	e.PromptResponse, _ = m["prompt_response"].(map[string]any)
	if src, ok := m["source"].(map[string]any); ok {
		s := sourceOf(src)
		e.Source = &s
	}
	if n, ok := integer(m["platform_update_id"]); ok {
		e.PlatformUpdateID = &n
	}
	if b, ok := m["reply_expected"].(bool); ok {
		e.ReplyExpected = &b
	}
	for _, item := range asList(m["media_text_inlined"]) {
		if b, ok := item.(bool); ok {
			e.MediaTextInlined = append(e.MediaTextInlined, &b)
		} else {
			e.MediaTextInlined = append(e.MediaTextInlined, nil)
		}
	}
	if ts, err := time.Parse(time.RFC3339Nano, text(m["timestamp"])); err == nil {
		e.Timestamp = ts
	}
	return e
}

// Wire fields of a send result
func (r SendResult) Map() map[string]any {
	out := map[string]any{"success": r.Success, "message_id": r.MessageID, "error": r.Error, "raw_response": r.RawResponse,
		"retryable": r.Retryable, "continuation_message_ids": listOrEmpty(r.ContinuationMessageIDs)}
	if r.RetryAfter != nil {
		out["retry_after"] = *r.RetryAfter
	}
	if r.ErrorKind != "" {
		out["error_kind"] = r.ErrorKind
	}
	return out
}

// Send result of wire fields
func sendResultOf(m map[string]any) SendResult {
	r := SendResult{Success: argFlag(m, "success"), MessageID: m["message_id"], Error: m["error"], RawResponse: m["raw_response"],
		Retryable: argFlag(m, "retryable"), ContinuationMessageIDs: asList(m["continuation_message_ids"]), ErrorKind: text(m["error_kind"])}
	if _, present := m["retry_after"]; present {
		f := number(m["retry_after"])
		r.RetryAfter = &f
	}
	return r
}

// Stream event of wire fields
func streamEventOf(m map[string]any) StreamEvent {
	idx, _ := integer(m["index"])
	args, _ := m["args"].(map[string]any)
	extra, _ := m["extra"].(map[string]any)
	return StreamEvent{Kind: text(m["kind"]), Text: text(m["text"]), Final: argFlag(m, "final"), ToolName: text(m["tool_name"]),
		Preview: text(m["preview"]), Args: args, Index: int(idx), Duration: number(m["duration"]), OK: argFlag(m, "ok"),
		Notice: text(m["notice"]), Extra: extra}
}

// Audio format of wire fields
func audioFormatOf(m map[string]any) AudioFormat {
	rate, _ := integer(m["sample_rate"])
	channels, _ := integer(m["channels"])
	width, _ := integer(m["sample_width"])
	return AudioFormat{SampleRate: int(rate), Channels: int(channels), SampleWidth: int(width)}
}

// Warning request of keyword wire arguments
func warningOf(kwargs map[string]any) WarningRequest {
	shown, _ := kwargs["shown_metadata"].(map[string]any)
	metadata, _ := kwargs["metadata"].(map[string]any)
	return WarningRequest{ChatID: text(kwargs["chat_id"]), Content: text(kwargs["content"]), Hidden: text(kwargs["hidden"]), ReplyTo: text(kwargs["reply_to"]),
		Metadata: metadata, ShownMetadata: shown, Caption: text(kwargs["caption"]), LogicalPlatform: text(kwargs["logical_platform"])}
}

// Media request of a chat and a target with the keyword options
func mediaOf(args []any, kwargs map[string]any, url bool) MediaRequest {
	metadata, _ := kwargs["metadata"].(map[string]any)
	extra, _ := kwargs["extra"].(map[string]any)
	r := MediaRequest{ChatID: argText(args, 0), Caption: text(kwargs["caption"]), FileName: text(kwargs["file_name"]), ReplyTo: text(kwargs["reply_to"]),
		Metadata: metadata, Extra: extra}
	if url {
		r.URL = argText(args, 1)
	} else {
		r.Path = argText(args, 1)
	}
	return r
}
