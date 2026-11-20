package pluginsdk

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Names the host knows the env step forms and the target sentinels of a platform by
const (
	platformEnvModule   = "gateway.platform_env"
	envIntSentinel      = "gateway.platform_env.INT"
	notExplicitSentinel = "gateway.platforms.send_targets.NOT_EXPLICIT"
)

// Layout a message event time crosses in, with the zone offset
const eventTimeLayout = "2006-01-02T15:04:05.999999999Z07:00"

// Messaging platform a plugin adds to the gateway, every hook optional except the adapter
type Platform struct {
	Name        string
	Label       string
	RequiredEnv []string
	InstallHint string
	// Plain registry fields such as allowed_users_env, allow_all_env, max_message_length, emoji, platform_hint, pii_safe or display_tier
	Extra map[string]any
	// Whether the platform can run here, true when unset
	Check func(ctx context.Context) bool
	// Whether a platform config holds what the adapter needs
	ValidateConfig func(ctx context.Context, config map[string]any) (bool, error)
	// Whether a platform config counts as connected in status listings
	IsConnected func(ctx context.Context, config map[string]any) (bool, error)
	// Whether a platform config alone, without the environment, counts as connected
	ConfigConnected func(ctx context.Context, config map[string]any) (bool, error)
	// Installs what the platform needs when Check answered false, true once it can run
	EnsureDeps func(ctx context.Context) (bool, error)
	// Extra config the environment enables the platform with, empty leaving it off
	EnvEnablement func(ctx context.Context) (map[string]any, error)
	// Reads the platform block of config.yaml, changing either document in place and returning extra config to seed
	ApplyYAMLConfig func(ctx context.Context, yamlConfig, block map[string]any) (map[string]any, error)
	// Chat and thread a send target names, ok false leaving the reference to the built in rules
	ParseTargetRef func(ctx context.Context, ref string) (chatID, threadID string, ok bool, err error)
	// Whether a chat id is a valid send target, the reason shown when it is not
	ValidateTargetRef func(ctx context.Context, chatID string) (ok bool, reason string, err error)
	// Native syntax of a send target
	TargetRefSyntax func(ctx context.Context, ref string) (TargetSyntax, error)
	// Send the send_message tool makes when no adapter of the platform runs
	SendMessage func(ctx context.Context, req ToolSendRequest) (map[string]any, error)
	// Send cron jobs and the send command make with no gateway running
	StandaloneSend func(ctx context.Context, req StandaloneSendRequest) (map[string]any, error)
	// How cron and the send command use StandaloneSend
	StandaloneSendPolicy *StandaloneSendPolicy
	// Steps that turn environment variables into gateway config
	EnvOverrides []EnvStep
	// Lines the setup of the platform shows before its questions
	SetupInstructions []string
	// Questions icebath asks when the platform is set up, the first variable of RequiredEnv counting as the token the platform cannot work without
	SetupVars []SetupVar
	// Builds the adapter of one connection, nil for a config the platform does not serve
	Adapter func(ctx context.Context, config map[string]any, gw *Gateway) (*Adapter, error)
}

// One question of a platform setup, asked by icebath in the terminal UI and its answer saved as an environment variable of the profile
type SetupVar struct {
	// Variable the answer is saved under
	Env string
	// Question shown, the variable name when empty
	Prompt string
	// Line shown above the question
	Help string
	// Whether the answer is typed hidden
	Secret bool
	// Whether the answer lists the users allowed to talk to the platform
	Allowlist bool
}

// Verdict of a target syntax parser
type TargetSyntax struct {
	// Whether the parser recognized the reference, false leaving it to the built in rules
	Matched bool
	// Whether a recognized reference is a name to look up in place of an explicit target
	NotExplicit bool
	// Chat an explicit reference names, empty for none
	ChatID string
	// Thread an explicit reference names, empty for none
	ThreadID string
}

// Send the send_message tool asks of a platform without a running adapter
type ToolSendRequest struct {
	Args     map[string]any
	ChatID   string
	Platform string
	Config   map[string]any
}

// Send made with no running gateway, Options holding the thread, media and other keywords of the caller
type StandaloneSendRequest struct {
	Config  map[string]any
	ChatID  string
	Message string
	Options map[string]any
}

// How cron and the send command use the standalone sender of a platform
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

// One environment to config step, exactly one of its forms set
type EnvStep struct {
	Credentials *EnvCredentials
	HomeChannel *EnvHomeChannel
	ReplyMode   *EnvReplyMode
	// Changes the gateway config document in place
	Apply func(ctx context.Context, gatewayConfig map[string]any) error
}

// Enables a platform when its credential variables are set and copies the listed variables into its config
type EnvCredentials struct {
	// Platform the step enables, the registering platform when empty
	Platform string
	// Groups of variable names, one name of every group having to be set
	Credentials [][]string
	// Variable copied into the platform token
	TokenEnv string
	// Variables always written into the extra config, a default standing in for an unset one
	Fixed []EnvSpec
	// Variables written into the extra config when set
	Optional []EnvSpec
	// Variables written into the extra config when set, surrounding space removed
	OptionalStripped []EnvSpec
	// Variable whose absence logs WarnMissingText
	WarnMissingEnv  string
	WarnMissingText string
	// Variable base of the home channel, read as <HomeEnv>_HOME_CHANNEL
	HomeEnv   string
	HomeStrip bool
	// Runs once the platform is enabled, changing the gateway config or the platform config in place
	Then func(ctx context.Context, gatewayConfig, platformConfig map[string]any) error
}

// One variable copied into the extra config of a platform
type EnvSpec struct {
	Key string
	Env string
	// Variables read in order in place of Env, the first one set winning
	AnyOf []string
	// Value of a fixed spec whose variable is unset
	Default string
	// Whether an optional spec stores a whole number, a malformed one skipped
	Integer bool
	// Value stored for the variable text
	Convert func(ctx context.Context, value string) (any, error)
}

// Step reading the home channel of a platform from <EnvBase>_HOME_CHANNEL and its name
type EnvHomeChannel struct {
	Platform string
	EnvBase  string
	Strip    bool
}

// Step reading the reply mode of a platform from one variable
type EnvReplyMode struct {
	Platform string
	Env      string
}

// Gateway side of one platform adapter, taking its inbound messages and its connection state
type Gateway struct {
	mu                     sync.Mutex
	handle, run, lifecycle *HostFunc
}

// Keeps the callbacks the gateway gave for the adapter
func (g *Gateway) bind(handle, run, lifecycle *HostFunc) {
	g.mu.Lock()
	g.handle, g.run, g.lifecycle = handle, run, lifecycle
	g.mu.Unlock()
}

// Callbacks of the bound gateway, an error before the gateway built the adapter
func (g *Gateway) bound() (handle, run, lifecycle *HostFunc, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.handle == nil {
		return nil, nil, nil, &Error{Type: "RuntimeError", Message: "the gateway has not bound this adapter yet, it does so once the adapter factory returns"}
	}
	return g.handle, g.run, g.lifecycle, nil
}

// Hands an inbound message to the gateway pipeline with its access checks, batching and busy session queueing
func (g *Gateway) HandleMessage(ctx context.Context, event MessageEvent) error {
	handle, _, _, err := g.bound()
	if err != nil {
		return err
	}
	_, err = handle.Call(ctx, []any{event.fields()}, nil)
	return err
}

// Runs the gateway message handler directly for an adapter that dispatches its own messages and returns the reply
func (g *Gateway) RunHandler(ctx context.Context, event MessageEvent) (any, error) {
	_, run, _, err := g.bound()
	if err != nil {
		return nil, err
	}
	return run.Call(ctx, []any{event.fields()}, nil)
}

// One connection state change of the adapter
func (g *Gateway) state(ctx context.Context, name string, args []any, kwargs map[string]any) error {
	_, _, lifecycle, err := g.bound()
	if err != nil {
		return err
	}
	_, err = lifecycle.Call(ctx, []any{name, listOrEmpty(args), mapOrEmpty(kwargs)}, nil)
	return err
}

// Marks the adapter connected, listenerBase naming its webhook listener when it has one
func (g *Gateway) MarkConnected(ctx context.Context, listenerBase string) error {
	kwargs := map[string]any{}
	if listenerBase != "" {
		kwargs["listener_base"] = listenerBase
	}
	return g.state(ctx, "_mark_connected", nil, kwargs)
}

// Marks the adapter disconnected
func (g *Gateway) MarkDisconnected(ctx context.Context) error {
	return g.state(ctx, "_mark_disconnected", nil, nil)
}

// Records a fatal connection error, retryable when the gateway may connect again
func (g *Gateway) SetFatalError(ctx context.Context, code, message string, retryable bool) error {
	return g.state(ctx, "_set_fatal_error", []any{code, message}, map[string]any{"retryable": retryable})
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
	Text string
	// Kind of message, text when empty
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
	// Time the message was sent, the time the gateway receives it when zero
	Timestamp time.Time
	// Whether the message may not run gateway control commands
	DenyGatewayControl bool
	ReplyExpected      *bool
	InheritChannelPin  bool
	EditRevision       string
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

// Mapping argument at a position, empty when absent
func argMap(args []any, i int) map[string]any {
	return mapOrEmpty(asMap(argAt(args, i)))
}

// Argument at a position, nil when absent
func argAt(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return nil
}

// String argument at a position, empty when absent
func argText(args []any, i int) string { return text(argAt(args, i)) }

// Flag of a mapping by name
func argFlag(m map[string]any, name string) bool {
	b, _ := m[name].(bool)
	return b
}

// Wire list of strings
func stringsAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Wire value of an optional text, none when empty
func optionalText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Sets every non empty text of a wire mapping
func setTexts(out map[string]any, pairs ...string) {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			out[pairs[i]] = pairs[i+1]
		}
	}
}

// Wire fields of a session source
func (s SessionSource) fields() map[string]any {
	out := map[string]any{"platform": s.Platform, "chat_id": s.ChatID, "auto_thread_created": s.AutoThreadCreated}
	setTexts(out, "chat_type", s.ChatType, "chat_name", s.ChatName, "user_id", s.UserID, "user_name", s.UserName, "thread_id", s.ThreadID,
		"chat_topic", s.ChatTopic, "user_id_alt", s.UserIDAlt, "chat_id_alt", s.ChatIDAlt, "scope_id", s.ScopeID,
		"parent_chat_id", s.ParentChatID, "message_id", s.MessageID, "profile", s.Profile,
		"auto_thread_initial_name", s.AutoThreadInitialName, "prospective_thread_id", s.ProspectiveThreadID)
	return out
}

// Session source of wire fields
func sourceOf(m map[string]any) SessionSource {
	return SessionSource{Platform: text(m["platform"]), ChatID: scalarText(m["chat_id"]), ChatName: text(m["chat_name"]), ChatType: text(m["chat_type"]),
		UserID: text(m["user_id"]), UserName: text(m["user_name"]), ThreadID: text(m["thread_id"]), ChatTopic: text(m["chat_topic"]),
		UserIDAlt: text(m["user_id_alt"]), ChatIDAlt: text(m["chat_id_alt"]), ScopeID: text(m["scope_id"]), ParentChatID: text(m["parent_chat_id"]),
		MessageID: text(m["message_id"]), Profile: text(m["profile"]), AutoThreadCreated: argFlag(m, "auto_thread_created"),
		AutoThreadInitialName: text(m["auto_thread_initial_name"]), ProspectiveThreadID: text(m["prospective_thread_id"])}
}

// Wire fields of a message event as the gateway reads an inbound one
func (e MessageEvent) fields() map[string]any {
	out := map[string]any{"text": e.Text, "message_type": "text", "raw_message": e.RawMessage, "media_urls": stringsAny(e.MediaURLs),
		"media_types": stringsAny(e.MediaTypes), "media_filenames": stringsAny(e.MediaFilenames), "reply_to_is_own_message": e.ReplyToIsOwnMessage,
		"auto_skill": e.AutoSkill, "internal": e.Internal, "metadata": mapOrEmpty(e.Metadata), "allow_gateway_control": !e.DenyGatewayControl,
		"inherit_channel_pin": e.InheritChannelPin}
	setTexts(out, "message_type", e.MessageType, "user_id", e.UserID, "user_name", e.UserName, "message_id", e.MessageID,
		"ledger_message_id", e.LedgerMessageID, "reply_anchor_override", e.ReplyAnchorOverride, "reply_to_message_id", e.ReplyToMessageID,
		"reply_to_text", e.ReplyToText, "reply_to_author_id", e.ReplyToAuthorID, "reply_to_author_name", e.ReplyToAuthorName,
		"channel_prompt", e.ChannelPrompt, "channel_context", e.ChannelContext, "edit_revision", e.EditRevision)
	if e.Source != nil {
		out["source"] = e.Source.fields()
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
		out["timestamp"] = map[string]any{"__datetime__": e.Timestamp.Format(eventTimeLayout)}
	}
	return out
}

// Message event of the wire fields the gateway sends
func eventOf(m map[string]any) MessageEvent {
	e := MessageEvent{Text: text(m["text"]), MessageType: text(m["message_type"]), UserID: text(m["user_id"]), UserName: text(m["user_name"]),
		MessageID: text(m["message_id"]), LedgerMessageID: text(m["ledger_message_id"]), ReplyAnchorOverride: text(m["reply_anchor_override"]),
		RawMessage: m["raw_message"], MediaURLs: stringsOf(m["media_urls"]), MediaTypes: stringsOf(m["media_types"]),
		MediaFilenames: stringsOf(m["media_filenames"]), ReplyToMessageID: text(m["reply_to_message_id"]), ReplyToText: text(m["reply_to_text"]),
		ReplyToAuthorID: text(m["reply_to_author_id"]), ReplyToAuthorName: text(m["reply_to_author_name"]),
		ReplyToIsOwnMessage: argFlag(m, "reply_to_is_own_message"), PromptResponse: asMap(m["prompt_response"]), AutoSkill: m["auto_skill"],
		ChannelPrompt: text(m["channel_prompt"]), ChannelContext: text(m["channel_context"]), Internal: argFlag(m, "internal"),
		Metadata: asMap(m["metadata"]), InheritChannelPin: argFlag(m, "inherit_channel_pin"), EditRevision: text(m["edit_revision"])}
	if allowed, ok := m["allow_gateway_control"].(bool); ok {
		e.DenyGatewayControl = !allowed
	}
	if src := asMap(m["source"]); src != nil {
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
	stamp := text(m["timestamp"])
	if ts, err := time.Parse(eventTimeLayout, stamp); err == nil {
		e.Timestamp = ts
	} else if ts, err := time.ParseInLocation("2006-01-02T15:04:05", stamp, time.Local); err == nil {
		e.Timestamp = ts
	}
	return e
}

// Wire fields of a send result
func (r SendResult) fields() map[string]any {
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
	if m["retry_after"] != nil {
		f := number(m["retry_after"])
		r.RetryAfter = &f
	}
	return r
}

// Registers a platform, every hook crossing as a callback under the keyword the registry knows it by
func (h *host) RegisterPlatform(platform Platform) (*Handle, error) {
	if platform.Adapter == nil {
		return nil, fmt.Errorf("platform %q has no adapter", platform.Name)
	}
	p := h.peer
	kwargs := map[string]any{}
	for k, v := range platform.Extra {
		kwargs[k] = v
	}
	kwargs["name"], kwargs["label"], kwargs["install_hint"] = platform.Name, platform.Label, platform.InstallHint
	if len(platform.RequiredEnv) > 0 {
		kwargs["required_env"] = platform.RequiredEnv
	}
	check := platform.Check
	if check == nil {
		check = func(context.Context) bool { return true }
	}
	kwargs["check_fn"] = h.checkRef(platform.Name+".check", check)
	hook := func(keyword string, set bool, fn Func) {
		if set {
			kwargs[keyword] = p.funcRef(platform.Name+"."+keyword, fn)
		}
	}
	hook("adapter_factory", true, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		gw := &Gateway{}
		adapter, err := platform.Adapter(ctx, argMap(args, 0), gw)
		if err != nil || adapter == nil {
			return nil, err
		}
		return adapterObject(p, platform.Name, adapter, gw)
	})
	configHook := func(keyword string, fn func(ctx context.Context, config map[string]any) (bool, error)) {
		hook(keyword, fn != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) { return fn(ctx, argMap(args, 0)) })
	}
	configHook("validate_config", platform.ValidateConfig)
	configHook("is_connected", platform.IsConnected)
	configHook("config_connected_fn", platform.ConfigConnected)
	hook("ensure_deps_fn", platform.EnsureDeps != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		return platform.EnsureDeps(ctx)
	})
	hook("env_enablement_fn", platform.EnvEnablement != nil, func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		return platform.EnvEnablement(ctx)
	})
	hook("apply_yaml_config_fn", platform.ApplyYAMLConfig != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return platform.ApplyYAMLConfig(ctx, argMap(args, 0), argMap(args, 1))
	})
	hook("parse_target_ref_fn", platform.ParseTargetRef != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		chat, thread, ok, err := platform.ParseTargetRef(ctx, argText(args, 0))
		if err != nil || !ok {
			return nil, err
		}
		return []any{chat, optionalText(thread)}, nil
	})
	hook("validate_target_ref_fn", platform.ValidateTargetRef != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		ok, reason, err := platform.ValidateTargetRef(ctx, argText(args, 0))
		if err != nil || ok {
			return ok, err
		}
		if reason == "" {
			return false, nil
		}
		return reason, nil
	})
	hook("target_ref_syntax_fn", platform.TargetRefSyntax != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		syntax, err := platform.TargetRefSyntax(ctx, argText(args, 0))
		switch {
		case err != nil || !syntax.Matched:
			return nil, err
		case syntax.NotExplicit:
			return map[string]any{"__sentinel__": notExplicitSentinel}, nil
		}
		return []any{optionalText(syntax.ChatID), optionalText(syntax.ThreadID)}, nil
	})
	hook("send_message_handler", platform.SendMessage != nil, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		return platform.SendMessage(ctx, ToolSendRequest{Args: argMap(args, 0), ChatID: scalarText(argAt(args, 1)), Platform: argText(args, 2), Config: argMap(args, 3)})
	})
	hook("standalone_sender_fn", platform.StandaloneSend != nil, func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
		return platform.StandaloneSend(ctx, StandaloneSendRequest{Config: argMap(args, 0), ChatID: scalarText(argAt(args, 1)), Message: argText(args, 2), Options: mapOrEmpty(kwargs)})
	})
	if s := platform.StandaloneSendPolicy; s != nil {
		kwargs["standalone_send"] = &Object{Type: "StandaloneSendPolicy", Attrs: map[string]any{"label": s.Label, "media": s.Media, "all_sends": s.AllSends,
			"captionable": s.Captionable, "empty_list_between_chunks": s.EmptyListBetweenChunks, "forward_force_document": s.ForwardForceDocument,
			"mentions": s.Mentions, "text_direct": s.TextDirect}}
	}
	if len(platform.SetupInstructions) > 0 {
		kwargs["setup_instructions"] = platform.SetupInstructions
	}
	if len(platform.SetupVars) > 0 {
		vars := make([]any, 0, len(platform.SetupVars))
		for _, v := range platform.SetupVars {
			if v.Env == "" {
				return nil, fmt.Errorf("platform %q has a setup question that names no variable", platform.Name)
			}
			vars = append(vars, map[string]any{"name": v.Env, "prompt": v.Prompt, "help": v.Help, "password": v.Secret, "is_allowlist": v.Allowlist})
		}
		kwargs["setup_vars"] = vars
	}
	if len(platform.EnvOverrides) > 0 {
		steps := make([]any, 0, len(platform.EnvOverrides))
		for i, step := range platform.EnvOverrides {
			ref, err := envStepRef(p, platform.Name, fmt.Sprintf("%s.env_step_%d", platform.Name, i), step)
			if err != nil {
				return nil, fmt.Errorf("platform %q env step %d: %w", platform.Name, i, err)
			}
			steps = append(steps, ref)
		}
		kwargs["env_overrides"] = steps
	}
	return h.register("register_platform", nil, kwargs)
}

// Callback reference of one env step, the forms icebath runs itself carrying their fields in place of code
func envStepRef(p *peer, platform, name string, step EnvStep) (map[string]any, error) {
	inHost := func(context.Context, []any, map[string]any) (any, error) {
		return nil, &Error{Type: "TypeError", Message: "this env step runs in icebath from its fields"}
	}
	or := func(value, fallback string) string {
		if value == "" {
			return fallback
		}
		return value
	}
	switch {
	case step.Credentials != nil:
		c := step.Credentials
		creds := make([]any, len(c.Credentials))
		for i, group := range c.Credentials {
			creds[i] = stringsAny(group)
		}
		fixed, err := envSpecRows(p, name, c.Fixed, true)
		if err != nil {
			return nil, err
		}
		optional, err := envSpecRows(p, name, c.Optional, false)
		if err != nil {
			return nil, err
		}
		stripped, err := envSpecRows(p, name, c.OptionalStripped, false)
		if err != nil {
			return nil, err
		}
		fields := map[string]any{"platform": or(c.Platform, platform), "creds": creds, "token": optionalText(c.TokenEnv), "fixed": fixed,
			"optional": optional, "optional_stripped": stripped, "home": optionalText(c.HomeEnv), "home_strip": c.HomeStrip}
		if c.WarnMissingEnv != "" {
			fields["warn_missing"] = []any{c.WarnMissingEnv, c.WarnMissingText}
		}
		if then := c.Then; then != nil {
			fields["then"] = p.funcRef(name+".then", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
				return nil, then(ctx, argMap(args, 0), argMap(args, 1))
			})
		}
		ref := p.funcRef(name, inHost)
		ref["shape"] = map[string]any{"kind": "dataclass", "module": platformEnvModule, "qualname": "EnvCred", "fields": fields}
		return ref, nil
	case step.HomeChannel != nil:
		s := step.HomeChannel
		ref := p.funcRef(name, inHost)
		ref["shape"] = map[string]any{"kind": "partial", "func": platformEnvModule, "qualname": "env_home_channel", "args": []any{},
			"keywords": map[string]any{"platform": or(s.Platform, platform), "env_base": s.EnvBase, "strip": s.Strip}}
		return ref, nil
	case step.ReplyMode != nil:
		s := step.ReplyMode
		ref := p.funcRef(name, inHost)
		ref["shape"] = map[string]any{"kind": "partial", "func": platformEnvModule, "qualname": "env_reply_mode", "args": []any{},
			"keywords": map[string]any{"platform": or(s.Platform, platform), "env": s.Env}}
		return ref, nil
	case step.Apply != nil:
		apply := step.Apply
		return p.funcRef(name, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			return nil, apply(ctx, argMap(args, 0))
		}), nil
	}
	return nil, fmt.Errorf("the step sets none of Credentials, HomeChannel, ReplyMode or Apply")
}

// Wire rows of env specs, a fixed row carrying its default and an optional row its whole number mark or converter
func envSpecRows(p *peer, name string, specs []EnvSpec, fixed bool) ([]any, error) {
	rows := make([]any, 0, len(specs))
	for i, s := range specs {
		if s.Env == "" && len(s.AnyOf) == 0 {
			return nil, fmt.Errorf("env spec %s names no variable", s.Key)
		}
		var env any = s.Env
		if len(s.AnyOf) > 0 {
			env = stringsAny(s.AnyOf)
		}
		row := []any{s.Key, env}
		if fixed {
			row = append(row, s.Default)
		}
		if convert := s.Convert; convert != nil {
			row = append(row, p.funcRef(fmt.Sprintf("%s.%s_%d", name, s.Key, i), func(ctx context.Context, args []any, _ map[string]any) (any, error) {
				return convert(ctx, argText(args, 0))
			}))
		} else if s.Integer && !fixed {
			row = append(row, map[string]any{"__sentinel__": envIntSentinel})
		}
		rows = append(rows, row)
	}
	return rows, nil
}
