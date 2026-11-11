package tuiserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Re anchors the live session of a project tool call at its primary folder and pushes session info
func applyProjectWorkspace(taskID, path, _ string) {
	if path == "" {
		return
	}
	key := taskID
	var sid string
	var session *tuigateway.Session
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		if live := tuigateway.SessionRecord(key); live != nil {
			sid, session = key, live
			return
		}
		for _, live := range tuigateway.LiveSessions() {
			if strOr(live.Session.Get("session_key")) == key {
				sid, session = live.SID, live.Session
				return
			}
			if a := sessionAgent(live.Session); a != nil && a.SessionID() == key {
				sid, session = live.SID, live.Session
				return
			}
		}
	}()
	resolved := absPath(pypath.Expanduser(path))
	if session == nil {
		return
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return
	}
	ctx := sessionProfileRuntimeScope(context.Background(), session, false)
	session.Update(map[string]any{"cwd": resolved, "explicit_cwd": true, "cwd_from_settle": false})
	registerSessionCwd(ctx, session)
	persistSessionCwdAndScheduleGitMeta(ctx, session, resolved, nil)
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger().Debug("failed to emit session.info after project workspace move", "error", r)
			}
		}()
		var info map[string]any
		if a := sessionAgent(session); a != nil {
			info = sessionInfo(ctx, a, session)
		} else {
			info = map[string]any{"cwd": resolved, "branch": tuigateway.Branch(resolved),
				"project": projectInfoForCwd(ctx, resolved), "lazy": true, "desktop_contract": desktopBackendContract}
		}
		tuigateway.Emit(ctx, "session.info", sid, info)
	}()
}

// Secret capture of the UI session the calling context is bound to, skipped when none owns it or it closed
func tuiSecretCapture(ctx context.Context, envVar, prompt string, metadata map[string]any) (map[string]any, error) {
	payload := map[string]any{"prompt": prompt, "env_var": envVar}
	if len(metadata) > 0 {
		payload["metadata"] = metadata
	}
	ownerSID := tools.GetSessionEnv(tools.ToolEnvFrom(ctx), "ICEBATH_UI_SESSION_ID", "")
	value := ""
	if ownerSID != "" && tuigateway.SessionRecord(ownerSID) == nil {
		logger().Info("secret prompt for " + ownerSID + " refused: its UI session is closed")
	} else if ownerSID != "" {
		value = ask(ctx, "secret", ownerSID, payload, nil)
	}
	if value == "" {
		return map[string]any{"success": true, "stored_as": envVar, "validated": false, "skipped": true, "message": "skipped"}, nil
	}
	saved, err := config.SaveEnvValueSecure(ctx, home.Dir(ctx), envVar, value)
	if err != nil {
		return nil, err
	}
	return map[string]any{"success": saved.Success, "stored_as": saved.StoredAs, "validated": saved.Validated,
		"skipped": false, "message": "ok"}, nil
}

// Installs the sudo, project workspace, secret and vault prompts of a session on the calling thread
func wireCallbacks(ctx context.Context, sid string) {
	sudoTimeout, vaultTimeout, loginTimeout := 120.0, 120.0, 180.0
	tools.SetSudoPasswordCallback(ctx, &tools.SudoPasswordCallback{Prompt: func(pctx context.Context) string {
		return ask(pctx, "sudo", sid, map[string]any{"command": gateway.RedactApprovalCommand(pctx, tools.GetSudoPromptCommand(pctx))}, &sudoTimeout)
	}})
	tools.SetProjectWorkspaceCallback(applyProjectWorkspace)
	skills.SetContextSecretCaptureCallback(tuiSecretCapture)
	browser.SetVaultSessionID(ctx, &sid)
	browser.SetUnlockPromptCallback(ctx, func(backend, displayName string) string {
		return ask(ctx, "vault.unlock_prompt", sid, map[string]any{"backend": backend, "display_name": displayName}, &vaultTimeout)
	})
	browser.SetSaveLoginPromptCallback(ctx, func(origin, site string) map[string]string {
		raw := ask(ctx, "vault.save_login", sid, map[string]any{"origin": origin, "site": site}, &loginTimeout)
		if raw == "" {
			return nil
		}
		var data any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil
		}
		dict, isDict := data.(map[string]any)
		if !isDict || !truthy(dict["password"]) {
			return nil
		}
		out := make(map[string]string, len(dict))
		for k, v := range dict {
			if text, isText := v.(string); isText {
				out[k] = text
			} else {
				out[k] = pyrepr.Str(v)
			}
		}
		return out
	})
	browser.SetCodePromptCallback(ctx, func(site, hint string) string {
		return ask(ctx, "vault.code", sid, map[string]any{"site": site, "hint": hint}, &loginTimeout)
	})
}

// Adopts fallback provider edits into the cached agent at turn start, keeping the last good chain on a bad config
func syncAgentFallbackWithConfig(ctx context.Context, sid string, session *tuigateway.Session) {
	a := sessionAgent(session)
	if a == nil {
		return
	}
	cfg, err := config.LoadUserEffectiveFailClosed(filepath.Join(home.Dir(ctx), "config.yaml"), os.LookupEnv)
	if err != nil {
		logger().Warn("fallback chain sync skipped for " + sid + " (keeping current chain): " + err.Error())
		return
	}
	a.ApplyFallbackChain(provider.GetFallbackChain(cfg))
}
