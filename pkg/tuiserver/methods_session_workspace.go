package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Declares the cwd, workspace and live session bookkeeping methods on the session module registry
func registerSessionWorkspaceHandlers() {
	sessionMethod("session.cwd.set", false, sessionCwdSet)
	sessionRegistry.method("session.workspace.move", sessionWorkspaceMove)
	sessionRegistry.method("session.active_list", sessionActiveList)
	sessionMethod("session.activate", false, sessionActivate)
	sessionRegistry.method("session.delete", sessionDelete)
}

// Session info after a cwd change, the full agent view or the lazy shape
func cwdInfo(ctx context.Context, session *tuigateway.Session, cwd string, branch *string) map[string]any {
	if session.Get("agent") != nil {
		return sessionInfo(ctx, sessionAgent(session), session)
	}
	current := tuigateway.Branch(cwd)
	if branch != nil {
		current = *branch
	}
	return map[string]any{"cwd": cwd, "branch": current, "project": projectInfoForCwd(ctx, cwd), "lazy": true,
		"desktop_contract": desktopBackendContract}
}

// Live sessions of this process in registry order
func snapshotSessions() []tuigateway.LiveSession {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	return tuigateway.LiveSessions()
}

// Moves an idle live session to a new working directory
func sessionCwdSet(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
	if truthy(session.Get("running")) {
		return rpcErr(rid, 4009, "session busy", nil)
	}
	raw := strParam(params, "cwd", "")
	if raw == "" {
		return rpcErr(rid, 4016, "cwd required", nil)
	}
	cwd, err := setSessionCwd(ctx, session, raw)
	if err != nil {
		if isValueError(err) {
			return rpcErr(rid, 4017, err.Error(), nil)
		}
		panic(err)
	}
	info := cwdInfo(ctx, session, cwd, nil)
	tuigateway.Emit(ctx, "session.info", strOr(params["session_id"]), info)
	return ok(rid, info)
}

// Rehomes a stored session's workspace by key, replacing its git meta and moving a live agent along
func sessionWorkspaceMove(ctx context.Context, rid any, params map[string]any) map[string]any {
	target := strParam(params, "session_key", "")
	if target == "" {
		return rpcErr(rid, 4007, "session_key required", nil)
	}
	raw := strParam(params, "cwd", "")
	if raw == "" {
		return rpcErr(rid, 4016, "cwd required", nil)
	}
	liveSID := ""
	var live *tuigateway.Session
	for _, entry := range snapshotSessions() {
		if entry.Session.Get("session_key") == target {
			liveSID, live = entry.SID, entry.Session
			break
		}
	}
	var profileHomeDir string
	if live != nil {
		profileHomeDir = strOr(live.Get("profile_home"))
	} else {
		resolved, err := profileHome(strOr(params["profile"]))
		panicIf(err)
		profileHomeDir = resolved
	}
	targetCwd, err := workspaceCwd(ctx, profileHomeDir, home.TranslateCwdForWSLBackend(raw))
	if err != nil {
		if isValueError(err) {
			return rpcErr(rid, 4017, "working directory does not exist: "+raw, nil)
		}
		panic(err)
	}
	branch, root := tuigateway.Branch(targetCwd), tuigateway.CommonRepoRoot(targetCwd)
	if resp := func() map[string]any {
		db, release, err := profileDB(ctx, params, true)
		panicIf(err)
		defer release()
		if db == nil {
			return dbUnavailableError(ctx, rid, 5007)
		}
		row, err := db.GetSession(ctx, target)
		panicIf(err)
		if row == nil || row.Len() == 0 {
			if live == nil {
				return rpcErr(rid, 4007, "session not found", nil)
			}
			return nil
		}
		if _, err := db.UpdateSessionCwdClaim(ctx, target, targetCwd, branch, root, true); err != nil {
			return rpcErr(rid, 5007, "move failed: "+err.Error(), nil)
		}
		return nil
	}(); resp != nil {
		return resp
	}
	if live != nil {
		if _, err := setSessionCwd(ctx, live, targetCwd); err != nil {
			if isValueError(err) {
				return rpcErr(rid, 4017, err.Error(), nil)
			}
			panic(err)
		}
		tuigateway.Emit(ctx, "session.info", liveSID, cwdInfo(ctx, live, targetCwd, &branch))
	}
	return ok(rid, map[string]any{"cwd": targetCwd, "branch": branch, "git_repo_root": root})
}

// Live sessions of this process in insertion order, finalized ones left out
func sessionActiveList(ctx context.Context, rid any, params map[string]any) map[string]any {
	current := ""
	if truthy(params["current_session_id"]) {
		current = strOr(params["current_session_id"])
	}
	rows := []any{}
	for _, entry := range snapshotSessions() {
		if !truthy(entry.Session.Get("_finalized")) {
			rows = append(rows, sessionLiveItem(ctx, entry.SID, entry.Session, current))
		}
	}
	return ok(rid, map[string]any{"sessions": rows})
}

// Attaches the caller to a live session without closing the one it focused before
func sessionActivate(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
	sid := ""
	if truthy(params["session_id"]) {
		sid = strOr(params["session_id"])
	}
	sessionResumeLock.Lock()
	if refusal := reattachRefusal(rid, sid, session); refusal != nil {
		sessionResumeLock.Unlock()
		return refusal
	}
	unlock := lockHistory(session)
	rebindLiveTransport(ctx, sid, session, callerTransport(ctx))
	unlock()
	sessionResumeLock.Unlock()
	omit, present := params["omit_messages"]
	if !present {
		omit = false
	}
	return ok(rid, liveSessionPayload(ctx, sid, session, livePayloadOptions{touch: true, omitMessages: isTruthyValue(omit), inlineImages: true}))
}

// Deletes a stored session with its transcripts, refused while it is live in this process
func sessionDelete(ctx context.Context, rid any, params map[string]any) map[string]any {
	if !truthy(params["session_id"]) {
		return rpcErr(rid, 4006, "session_id required", nil)
	}
	target := strOr(params["session_id"])
	for _, entry := range snapshotSessions() {
		if entry.Session.Get("session_key") == target {
			return rpcErr(rid, 4023, "cannot delete an active session", nil)
		}
	}
	profileHomeDir, err := profileHome(pystr.Strip(strOr(params["profile"])))
	panicIf(err)
	db, release, err := profileDB(ctx, params, true)
	panicIf(err)
	defer release()
	if db == nil {
		return dbUnavailableError(ctx, rid, 5036)
	}
	root := profileHomeDir
	if root == "" {
		root = home.Dir(ctx)
	}
	var deleteErr error
	deleted, err := tools.DeleteOnboardingChat(ctx, db, target, func() bool {
		removed, err := db.DeleteSession(ctx, target, state.DeleteSessionOptions{SessionsDir: filepath.Join(root, "sessions"),
			ExcludeActiveWriteGuards: true})
		deleteErr = err
		return err == nil && removed
	})
	if err == nil {
		err = deleteErr
	}
	if err != nil {
		var guard *state.SessionActiveWriteGuardError
		if errors.As(err, &guard) {
			return rpcErr(rid, 4023, "cannot delete an active session", nil)
		}
		return rpcErr(rid, 5036, fmt.Sprintf("delete failed: %s", err), nil)
	}
	if !deleted {
		return rpcErr(rid, 4007, "session not found", nil)
	}
	return ok(rid, map[string]any{"deleted": target})
}
