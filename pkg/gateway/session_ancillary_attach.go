package gateway

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// File attachment staging of the TUI server, assigned from pkg/tuiserver in cmd/icebath/wiring.go
var PromptAttachments struct {
	StageBrowserFile func(ctx context.Context, session *tuigateway.Session, stagedUpload any, name string) (string, bool, error)
	StageSessionFile func(ctx context.Context, session *tuigateway.Session, rawPath, dataURL, name string) (string, bool, error)
	RefPath          func(ctx context.Context, session *tuigateway.Session, target string) string
	FormatRefValue   func(value string) string
}

// Ancillary verbs of a connection keyed by method name
func ancillaryHandlers(c *AuthorityConnection) map[string]controlHandler {
	out := map[string]controlHandler{}
	for name, kind := range map[string]string{"session.control.read": "control", "process.list": "processes",
		"subagent.list": "subagents", "subagent.tail": "tail"} {
		out[name] = func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return ancillaryRead(ctx, c, ref, params, kind)
		}
	}
	bind := func(fn func(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error)) controlHandler {
		return func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return fn(ctx, c, ref, params)
		}
	}
	out["approval.pending"] = func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
		return approvals(ctx, c, ref, params, false)
	}
	out["approval.received"] = func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
		return approvals(ctx, c, ref, params, true)
	}
	out["process.stop"] = bind(stopProcesses)
	out["process.kill"] = bind(killProcess)
	out["reload.mcp"] = bind(reloadMCP)
	out["file.attach"] = bind(attachFile)
	out["tools.show"] = bind(showTools)
	out["skills.reload"] = bind(reloadSkills)
	for _, name := range []string{"subagent.steer", "subagent.interrupt"} {
		steer := name == "subagent.steer"
		out[name] = func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return subagentControl(ctx, c, ref, params, steer)
		}
	}
	return out
}

// Stages a non image attachment for one subscribed session and returns the file ref its next submit carries
func attachFile(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	rawUpload := params.GetOr("staged_upload", nil)
	upload, isDict := rawUpload.(*pyjson.Object)
	invalid := paramsOutside(params, "session_id", "profile", "path", "data_url", "name", "staged_upload") || ref.SessionID == "" ||
		(rawUpload != nil && !isDict)
	for _, key := range params.Keys {
		if _, isStr := params.GetOr(key, nil).(string); key != "staged_upload" && !isStr {
			invalid = true
		}
	}
	if invalid {
		return nil, runtimeRefusal("invalid_params")
	}
	c.mu.Lock()
	_, subscribed := c.subscriptions[ref.SessionID]
	c.mu.Unlock()
	if !subscribed {
		return nil, runtimeRefusal("permission_denied")
	}
	if err := authorizeBusy(ctx, c.authority, c.actor, ref, params, "session:submit"); err != nil {
		return nil, err
	}
	if rawUpload != nil && !reachableUploadHome(c, upload.GetOr("profile_home", nil)) {
		return nil, runtimeRefusal("permission_denied")
	}
	a := c.authority
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, runtimeRefusal("permission_denied")
	}
	session := tuigateway.NewSession(map[string]any{"cwd": policy.Cwd, "profile_home": a.ProfileID})
	text := func(key string) string { value, _ := params.GetOr(key, "").(string); return value }
	var stored string
	var uploaded bool
	if rawUpload != nil {
		stored, uploaded, err = PromptAttachments.StageBrowserFile(ctx, session, pyjson.Plain(upload), text("name"))
	} else {
		stored, uploaded, err = PromptAttachments.StageSessionFile(ctx, session, text("path"), text("data_url"), text("name"))
	}
	if err != nil {
		if pyTypeNamed(err, "ValueError", "OverflowError") {
			return nil, refusalFrom("invalid_params", err)
		}
		if pypath.IsOSError(err) {
			return nil, refusalFrom("storage_unavailable", err)
		}
		return nil, err
	}
	refPath := PromptAttachments.RefPath(ctx, session, stored)
	return pyjson.ObjectOf("attached", true, "name", filepath.Base(stored), "path", stored, "ref_path", refPath,
		"ref_text", "@file:"+PromptAttachments.FormatRefValue(refPath), "uploaded", uploaded), nil
}

// Whether a staged upload's source profile is this authority's home or a sibling a host scoped grant reaches
func reachableUploadHome(c *AuthorityConnection, rawHome any) bool {
	home, isStr := rawHome.(string)
	if !isStr || home == "" {
		return false
	}
	homes := []string{c.authority.ProfileID}
	if c.HostScoped() {
		if registry := runnerRegistry(c.authority); registry != nil {
			for _, sibling := range registry.Authorities() {
				homes = append(homes, sibling.ProfileID)
			}
		}
	}
	target, err := pypath.Resolve(home)
	if err != nil {
		return false
	}
	for _, candidate := range homes {
		resolved, err := pypath.Resolve(candidate)
		if err != nil {
			return false
		}
		if resolved == target {
			return true
		}
	}
	return false
}
