package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Session info after a cwd change, the full agent view or the lazy shape
func cwdInfo(ctx context.Context, session *tuihost.Session, cwd string, branch *string) map[string]any {
	if session.Get("agent") != nil {
		return sessionInfo(ctx, sessionAgent(session), session)
	}
	current := tuihost.Branch(cwd)
	if branch != nil {
		current = *branch
	}
	return map[string]any{"cwd": cwd, "branch": current, "project": projectInfoForCwd(ctx, cwd), "lazy": true}
}

// Live sessions of this process in registry order
func snapshotSessions() []tuihost.LiveSession {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	return tuihost.LiveSessions()
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
	var live *tuihost.Session
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
	branch, root := tuihost.Branch(targetCwd), tuihost.CommonRepoRoot(targetCwd)
	if resp := func() map[string]any {
		db, release, err := profileDB(ctx, params, true)
		if err != nil {
			return storeFailure(ctx, rid, 5007, err)
		}
		defer release()
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
		tuihost.Emit(ctx, "session.info", liveSID, cwdInfo(ctx, live, targetCwd, &branch))
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
func sessionActivate(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) map[string]any {
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
	deleted, err := db.DeleteSession(ctx, target, state.DeleteSessionOptions{SessionsDir: filepath.Join(root, "sessions"),
		ExcludeActiveWriteGuards: true})
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
