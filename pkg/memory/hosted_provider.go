package memory

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Provider whose MemoryProvider object lives in the plugin host, every hook a call into the child
type hostedProvider struct {
	obj  *plugins.HostedObject
	name string
}

// Wraps a captured host object, reading its name property once
func newHostedProvider(ctx context.Context, obj *plugins.HostedObject) (Provider, error) {
	name, err := obj.Get(ctx, "name")
	if err != nil {
		return nil, err
	}
	return &hostedProvider{obj: obj, name: pyrepr.Str(name)}, nil
}

func hostedStr(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

func hostedMaps(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch m := item.(type) {
		case map[string]any:
			out = append(out, m)
		case plugins.Record:
			out = append(out, map[string]any(m))
		}
	}
	return out
}

// Keyword arguments the method signature accepts, every one when it takes var keywords or is uninspectable
func (p *hostedProvider) accepted(method string, kwargs map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range kwargs {
		if p.obj.AcceptsKeyword(method, k) {
			out[k] = v
		}
	}
	return out
}

func setIf(kwargs map[string]any, key, value string) {
	if value != "" {
		kwargs[key] = value
	}
}

func (p *hostedProvider) Name() string { return p.name }

func (p *hostedProvider) Available(ctx context.Context) bool {
	v, err := p.obj.Call(ctx, "is_available", nil, nil)
	return err == nil && pyrepr.Truthy(v)
}

func (p *hostedProvider) UnavailableReason(ctx context.Context) string {
	v, _ := p.obj.Call(ctx, "unavailable_reason", nil, nil)
	return hostedStr(v)
}

func (p *hostedProvider) Initialize(ctx context.Context, sessionID string, opts InitOptions) error {
	kwargs := map[string]any{"session_id": sessionID}
	for key, value := range map[string]string{"platform": opts.Platform, "hermes_home": opts.Home, "agent_context": opts.AgentContext,
		"agent_identity": opts.AgentIdentity, "agent_workspace": opts.AgentWorkspace, "agent_id": opts.AgentID,
		"parent_session_id": opts.ParentSessionID, "session_title": opts.SessionTitle, "session_title_source": opts.SessionTitleSource,
		"cwd": opts.Cwd, "user_id": opts.UserID, "user_id_alt": opts.UserIDAlt, "user_name": opts.UserName, "chat_id": opts.ChatID,
		"chat_name": opts.ChatName, "chat_type": opts.ChatType, "thread_id": opts.ThreadID, "gateway_session_key": opts.GatewaySessionKey} {
		setIf(kwargs, key, value)
	}
	if opts.Warn != nil {
		warn := opts.Warn
		kwargs["warning_callback"] = plugins.HostCallable(func(_ context.Context, args []any, _ map[string]any) (any, error) {
			if len(args) > 0 {
				warn(hostedStr(args[0]))
			}
			return nil, nil
		})
	}
	if opts.Status != nil {
		status := opts.Status
		kwargs["status_callback"] = plugins.HostCallable(func(_ context.Context, args []any, _ map[string]any) (any, error) {
			if len(args) > 0 {
				status(hostedStr(args[0]))
			}
			return nil, nil
		})
	}
	_, err := p.obj.Call(ctx, "initialize", nil, kwargs)
	return err
}

func (p *hostedProvider) SystemPromptBlock(ctx context.Context) (string, error) {
	v, err := p.obj.Call(ctx, "system_prompt_block", nil, nil)
	return hostedStr(v), err
}

func (p *hostedProvider) Prefetch(ctx context.Context, query, sessionID string) (string, error) {
	v, err := p.obj.Call(ctx, "prefetch", []any{query}, map[string]any{"session_id": sessionID})
	return hostedStr(v), err
}

func (p *hostedProvider) QueuePrefetch(ctx context.Context, query, sessionID string) error {
	_, err := p.obj.Call(ctx, "queue_prefetch", []any{query}, map[string]any{"session_id": sessionID})
	return err
}

// Recall indicator the provider reports, read from the returned RecallStatus fields
func (p *hostedProvider) RecallStatus(ctx context.Context) (*RecallStatus, error) {
	v, err := p.obj.Call(ctx, "recall_status", nil, nil)
	if err != nil || v == nil {
		return nil, err
	}
	field := func(name string) any {
		switch s := v.(type) {
		case map[string]any:
			return s[name]
		case plugins.Record:
			return s[name]
		case *plugins.HostedObject:
			out, _ := s.Get(ctx, name)
			return out
		}
		return nil
	}
	count, _ := field("count").(int64)
	status := NewRecallStatus(hostedStr(field("provider_label")), int(count))
	if g := hostedStr(field("glyph")); g != "" {
		status.Glyph = g
	}
	return status, nil
}

func (p *hostedProvider) SyncTurn(ctx context.Context, userContent, assistantContent string, opts SyncOptions) error {
	kwargs := map[string]any{"session_id": opts.SessionID}
	optional := map[string]any{}
	if opts.Messages != nil {
		optional["messages"] = opts.Messages
	}
	if opts.TurnAuthor != nil {
		optional["turn_author"] = opts.TurnAuthor
	}
	for k, v := range p.accepted("sync_turn", optional) {
		kwargs[k] = v
	}
	_, err := p.obj.Call(ctx, "sync_turn", []any{userContent, assistantContent}, kwargs)
	return err
}

func (p *hostedProvider) ToolSchemas(ctx context.Context) ([]map[string]any, error) {
	v, err := p.obj.Call(ctx, "get_tool_schemas", nil, nil)
	return hostedMaps(v), err
}

func (p *hostedProvider) HandleToolCall(ctx context.Context, toolName string, args map[string]any, opts ToolCallOptions) (string, error) {
	kwargs := map[string]any{}
	setIf(kwargs, "hermes_home", opts.Home)
	setIf(kwargs, "agent_identity", opts.AgentIdentity)
	setIf(kwargs, "caller_gsk", opts.CallerGSK)
	v, err := p.obj.Call(ctx, "handle_tool_call", []any{toolName, args}, p.accepted("handle_tool_call", kwargs))
	return hostedStr(v), err
}

func (p *hostedProvider) Shutdown(ctx context.Context) error {
	_, err := p.obj.Call(ctx, "shutdown", nil, nil)
	return err
}

func (p *hostedProvider) OnTurnStart(ctx context.Context, turnNumber int, message string, opts TurnOptions) error {
	kwargs := map[string]any{}
	setIf(kwargs, "author_id", opts.AuthorID)
	setIf(kwargs, "author_name", opts.AuthorName)
	if opts.AuthorIsBot {
		kwargs["author_is_bot"] = true
	}
	_, err := p.obj.Call(ctx, "on_turn_start", []any{turnNumber, message}, p.accepted("on_turn_start", kwargs))
	return err
}

func (p *hostedProvider) IdentitySignature(ctx context.Context) map[string]any {
	v, err := p.obj.Call(ctx, "identity_signature", nil, nil)
	if err != nil {
		return nil
	}
	switch m := v.(type) {
	case map[string]any:
		return m
	case plugins.Record:
		return map[string]any(m)
	}
	return nil
}

func (p *hostedProvider) OnSessionEnd(ctx context.Context, messages []map[string]any) error {
	_, err := p.obj.Call(ctx, "on_session_end", []any{messages}, nil)
	return err
}

func (p *hostedProvider) OnSessionSwitch(ctx context.Context, newSessionID string, opts SessionSwitchOptions) error {
	kwargs := map[string]any{"parent_session_id": opts.ParentSessionID, "reset": opts.Reset}
	if opts.Rewound {
		kwargs["rewound"] = true
	}
	if opts.Reason != "" {
		kwargs["reason"] = opts.Reason
	}
	_, err := p.obj.Call(ctx, "on_session_switch", []any{newSessionID}, p.accepted("on_session_switch", kwargs))
	return err
}

func (p *hostedProvider) OnPreCompress(ctx context.Context, messages []map[string]any, requireCheckpoint bool) (string, error) {
	kwargs := map[string]any{}
	if p.CheckpointAPIVersion() >= PreCompressCheckpointAPIVersion && p.obj.AcceptsKeyword("on_pre_compress", "require_checkpoint") {
		kwargs["require_checkpoint"] = requireCheckpoint
	}
	v, err := p.obj.Call(ctx, "on_pre_compress", []any{messages}, kwargs)
	return hostedStr(v), err
}

func (p *hostedProvider) CheckpointAPIVersion() int {
	v, err := p.obj.Get(context.Background(), "pre_compress_checkpoint_api_version")
	if n, ok := v.(int64); err == nil && ok {
		return int(n)
	}
	return legacyPreCompressAPIVersion
}

func (p *hostedProvider) OnDelegation(ctx context.Context, task, result string, opts DelegationOptions) error {
	_, err := p.obj.Call(ctx, "on_delegation", []any{task, result}, map[string]any{"child_session_id": opts.ChildSessionID})
	return err
}

func (p *hostedProvider) ConfigSchema() []map[string]any {
	v, _ := p.obj.Call(context.Background(), "get_config_schema", nil, nil)
	return hostedMaps(v)
}

func (p *hostedProvider) SaveConfig(values map[string]any, icebathHome string) error {
	_, err := p.obj.Call(context.Background(), "save_config", []any{values, icebathHome}, nil)
	return err
}

func (p *hostedProvider) OnMemoryWrite(ctx context.Context, action, target, content string, metadata map[string]any) error {
	args := []any{action, target, content}
	if p.obj.AcceptsKeyword("on_memory_write", "metadata") {
		if metadata == nil {
			metadata = map[string]any{}
		}
		args = append(args, metadata)
	}
	_, err := p.obj.Call(ctx, "on_memory_write", args, nil)
	return err
}

func (p *hostedProvider) BackupPaths() ([]string, error) {
	v, err := p.obj.Call(context.Background(), "backup_paths", nil, nil)
	if err != nil {
		return nil, err
	}
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, hostedStr(item))
	}
	return out, nil
}
