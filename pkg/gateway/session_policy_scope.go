package gateway

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Waits on the session's gateway approval for one proposed file change, a one use consent never a grant
func requestEditorEdit(ctx context.Context, env *tools.Env, proposal *tools.EditProposal) (bool, error) {
	var old any
	if proposal.OldText != nil {
		old = *proposal.OldText
	}
	paths := make([]any, len(proposal.Paths))
	for i, p := range proposal.Paths {
		paths[i] = p
	}
	key := "editor-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	data := map[string]any{
		"command": "Edit " + proposal.Path, "description": "Approve proposed file change", "pattern_key": key,
		"allow_session": false, "allow_permanent": false,
		"edit": map[string]any{"tool_name": proposal.ToolName, "path": proposal.Path, "old_text": old,
			"new_text": proposal.NewText, "arguments": proposal.Arguments, "paths": paths},
		"pattern_keys": []any{key},
	}
	return tools.AwaitEditorEditDecision(ctx, env, data), nil
}

// Edit requester of a policy, Accept Edits and Don't Ask letting an edit run unasked outside sensitive paths
func policyEditRequester(policy *LocalSessionPolicy) tools.EditApprovalRequester {
	if policy.EditApprovalPolicy == tools.AutoApproveAsk {
		return requestEditorEdit
	}
	return func(ctx context.Context, env *tools.Env, proposal *tools.EditProposal) (bool, error) {
		if tools.ShouldAutoApproveEdit(proposal, policy.EditApprovalPolicy, policy.Cwd) {
			return true, nil
		}
		return requestEditorEdit(ctx, env, proposal)
	}
}

// Context whose file edits ask the editor of an ACP session, unguarded for every other source
func editorScope(ctx context.Context, policy *LocalSessionPolicy) context.Context {
	if policy.Source == "acp" {
		return tools.WithEditApprovalRequester(ctx, policyEditRequester(policy))
	}
	return tools.WithEditApprovalRequester(ctx, nil)
}

// Runs body under a session's frozen launch policy, its workspace, terminal, profile read, editor and client env bound
func policyScope(ctx context.Context, policy *LocalSessionPolicy, a *SessionAuthority, body func(ctx context.Context) error) error {
	if policy == nil {
		return body(ctx)
	}
	if !workspaceReady(policy) {
		field, detail := "cwd", policy.Cwd
		return state.NewRuntimeStoreError("workspace_unavailable", &field, &detail)
	}
	loaded, err := pyjson.Loads(policy.TerminalJSON)
	if err != nil {
		return err
	}
	fields, ok := pyjson.AsObject(loaded)
	if !ok {
		return &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(loaded) + "' object does not support item assignment"}
	}
	terminal := tools.TerminalPolicy{}
	for _, key := range fields.Keys {
		value, _ := fields.Get(key)
		terminal[key] = pyrepr.Str(value)
	}
	var owner launchSecretOwner
	if a != nil {
		owner = a
	}
	if policy.ConfigSecretRef != nil {
		recovered, err := recoverConfigSecrets(ctx, owner, policy)
		if err != nil {
			return err
		}
		for i, path := range recovered.paths {
			if len(path) > 1 && path[0] == nil {
				terminal[pyrepr.Str(path[1])] = pyrepr.Str(recovered.values[i])
			}
		}
	}
	profile, err := policy.readProfile(ctx, owner)
	if err != nil {
		return err
	}
	ctx = withTurnProfile(ctx, policy, profile)
	active := policy
	if policy.FollowsProfile() {
		cfg, err := policy.config(ctx, owner, false)
		if err != nil {
			return err
		}
		toolsets, err := policy.currentToolsets(ctx, cfg)
		if err != nil {
			return err
		}
		followed := *policy
		followed.Toolsets = toolsets
		active = &followed
	}
	ctx = tools.SetTerminalScope(ctx, &tools.TerminalScope{Policy: terminal})
	ctx = WithActivePolicy(ctx, active)
	var auxiliary any
	if a != nil {
		cfg, err := policy.config(ctx, a, true)
		if err != nil {
			return err
		}
		auxiliary = cfg["auxiliary"]
	}
	ctx = editorScope(ctx, policy)
	return editorMCPScope(ctx, a, policy, func(scoped context.Context) error {
		if a != nil {
			scoped = provider.SessionAuxiliaryScope(scoped, auxiliary)
		}
		scoped, err := policy.ClientEnvScope(scoped)
		if err != nil {
			return err
		}
		return body(scoped)
	})
}
