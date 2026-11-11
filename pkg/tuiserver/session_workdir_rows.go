package tuiserver

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Slot of a staged submit row naming the session id the row was written under
const submitRowSessionKey = "_submit_row_session_id"

// Moves a session that settled in another worktree of the same repo there, reporting whether it moved
func reconcileSessionCwdFromTerminal(ctx context.Context, session *tuigateway.Session) bool {
	if session == nil || (truthy(session.Get("cwd_pinned")) && !truthy(session.Get("cwd_from_settle"))) {
		return false
	}
	if !sessionIsLocalBackend(ctx, session) {
		return false
	}
	recorded := ""
	failed := true
	func() {
		defer func() { _ = recover() }()
		env := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
		recorded, _ = tools.GetSessionCwd(env, strOr(session.Get("session_key")))
		failed = false
	}()
	if failed || recorded == "" {
		return false
	}
	resolved := absPath(pypath.Expanduser(recorded))
	current := absPath(pypath.Expanduser(sessionCwd(ctx, session)))
	if info, err := os.Stat(resolved); resolved == current || err != nil || !info.IsDir() {
		return false
	}
	landed, currentRoot := tuigateway.RepoRoot(resolved), tuigateway.RepoRoot(current)
	if landed == "" || currentRoot == "" || landed == currentRoot {
		return false
	}
	landedCommon := tuigateway.CommonRepoRoot(resolved)
	if landedCommon == "" || landedCommon != tuigateway.CommonRepoRoot(current) {
		return false
	}
	session.Set("cwd", resolved)
	session.Set("explicit_cwd", true)
	session.Set("cwd_from_settle", true)
	registerSessionCwd(ctx, session)
	persistSessionCwdAndScheduleGitMeta(ctx, session, resolved, nil)
	return true
}

// Emits end of turn session info after reconciling a settled cwd
func emitSettledSessionInfo(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger().Debug("failed to reconcile settled session cwd", "error", r)
			}
		}()
		reconcileSessionCwdFromTerminal(ctx, session)
	}()
	tuigateway.Emit(ctx, "session.info", sid, sessionInfo(ctx, a, session))
}

// Pushes the session cwd into the agent, its subdirectory hints and the terminal task overrides of the routed home
func registerSessionCwd(ctx context.Context, session *tuigateway.Session) {
	if session == nil || len(session.Fields()) == 0 {
		return
	}
	a := sessionAgent(session)
	cwd := strOr(session.Get("cwd"))
	if a != nil {
		a.SetSessionCwd(cwd)
		if cwd != "" {
			a.RebindSubdirectoryHints(cwd)
		}
	}
	suppress(func() {
		taskCwd, cwdSource := terminalTaskCwdWithSource(ctx, session)
		scoped := ctx
		if profileHome := strOr(session.Get("profile_home")); profileHome != "" && home.Key(home.Dir(ctx)) != home.Key(profileHome) {
			scoped = tools.WithProfileTerminalScope(home.WithOverride(ctx, profileHome), profileHome, nil)
		}
		overrides := map[string]any{"cwd": taskCwd, "cwd_source": cwdSource}
		env := &tools.Env{Home: home.Dir(scoped), TerminalScope: tools.GetTerminalScope(scoped)}
		unmountedDocker := false
		backend, err := tools.TerminalEnvRead(env, "TERMINAL_ENV", "local")
		if err == nil {
			var mount string
			if mount, err = tools.TerminalEnvRead(env, "TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false"); err == nil {
				unmountedDocker = backend == "docker" && !envTruthWords[strings.ToLower(mount)]
			}
		}
		var unavailable *tools.TerminalPolicyUnavailable
		if err != nil && !errors.As(err, &unavailable) {
			panic(err)
		}
		if unmountedDocker {
			overrides = map[string]any{"cwd": "", "cwd_source": "process"}
			if cwdSource == "session" {
				overrides["host_workspace"] = taskCwd
			}
		}
		if err := tools.RegisterTaskEnvOverrides(env, strOr(session.Get("session_key")), overrides); err != nil {
			panic(err)
		}
	})
}

// true, 1 and yes, the words a scope aware boolean terminal read accepts
var envTruthWords = map[string]bool{"true": true, "1": true, "yes": true}

// Model and model config a fresh session row carries, the session's own pick owning the row over the default
func workdirRowModelConfig(ctx context.Context, session *tuigateway.Session) (string, map[string]any) {
	override, _ := session.Get("model_override").(map[string]any)
	rowModel := strings.TrimSpace(strOr(override["model"]))
	if rowModel == "" {
		rowModel, _ = sessionDefaultRoute(ctx, session)
	}
	modelConfig := map[string]any{}
	for _, k := range []string{"model", "provider", "base_url", "api_mode"} {
		if v := override[k]; truthy(v) {
			modelConfig[k] = strOr(v)
		}
	}
	if strings.ToLower(strings.TrimSpace(strOr(modelConfig["provider"]))) == "custom" {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger().Debug("custom provider identity recovery failed (db row)", "error", r)
				}
			}()
			model := strOr(modelConfig["model"])
			if model == "" {
				model = rowModel
			}
			if healed := provider.CanonicalCustomIdentity(ctx, strOr(modelConfig["base_url"]), "", model, ""); healed != "" {
				modelConfig["provider"] = healed
			}
		}()
	}
	if reasoning := session.Get("create_reasoning_override"); reasoning != nil {
		modelConfig["reasoning_config"] = reasoning
	}
	if tier, present := session.Lookup("create_service_tier_override"); present && tier != nil {
		if truthy(tier) {
			modelConfig["service_tier"] = tier
		} else {
			modelConfig["service_tier"] = "normal"
		}
	}
	if parent := session.Get("parent_session_id"); truthy(parent) {
		modelConfig["_branched_from"] = parent
	}
	for _, flag := range []string{"room_plumbing", "follow_profile_config"} {
		if truthy(session.Get(flag)) {
			modelConfig[flag] = true
		}
	}
	if composer, isDict := session.Get("composer_override_profile").(map[string]any); isDict {
		modelConfig["composer_override_profile"] = composer
	}
	return rowModel, tools.WithSessionYolo(modelConfig, strOr(session.Get("session_key")))
}

// Persists the session row on first real activity, false only when the store failed to open
func ensureSessionDBRow(ctx context.Context, session *tuigateway.Session) bool {
	key := strOr(session.Get("session_key"))
	if key == "" {
		return true
	}
	profileHome := strOr(session.Get("profile_home"))
	db, failed, release := workdirOwnerDB(ctx, session, "failed to open profile db for session row")
	defer release()
	if failed {
		return false
	}
	if db == nil {
		dbMu.Lock()
		defer dbMu.Unlock()
		return dbError == ""
	}
	rowModel, modelConfig := workdirRowModelConfig(ctx, session)
	func() {
		defer func() {
			if r := recover(); r != nil {
				workdirReraiseDiskFull(r, "failed to persist desktop session row")
			}
		}()
		userID := ""
		if owner := tuigateway.SessionAuthUserID(session); owner != nil {
			userID = *owner
		}
		profileName := home.ProfileNameFor(profileHome)
		if profileName == "" {
			profileName = currentProfileName(ctx)
		}
		if len(modelConfig) == 0 {
			modelConfig = nil
		}
		if err := db.CreateSession(state.NewSession{ID: key, Source: sessionSource(session), Model: rowModel,
			ModelConfig: modelConfig, ParentSessionID: strOr(session.Get("parent_session_id")),
			Cwd: persistedSessionCwd(ctx, session), UserID: userID, ProfileName: profileName}); err != nil {
			panic(err)
		}
		if truthy(session.Get("pending_hidden")) {
			if changed, err := db.SetSessionHidden(ctx, key, true); err != nil {
				logger().Debug("failed to apply pending hidden flag", "error", err)
			} else if changed {
				session.Pop("pending_hidden")
			}
		}
		if truthy(session.Get("pending_archived")) {
			if changed, err := db.SetSessionArchived(ctx, key, true, true); err != nil {
				logger().Debug("failed to apply pending archived flag", "error", err)
			} else if changed {
				session.Pop("pending_archived")
			}
		}
		scheduleRowGitMeta(ctx, session, key, db)
	}()
	return true
}

// Git enriches a lazily created row once per live session when its row lacks branch or repo root
func scheduleRowGitMeta(ctx context.Context, session *tuigateway.Session, key string, db *state.DB) {
	if truthy(session.Get("row_git_meta_checked")) {
		return
	}
	session.Set("row_git_meta_checked", true)
	row, err := db.GetSession(ctx, key)
	if err != nil {
		panic(err)
	}
	if row == nil {
		row = pyjson.NewObject()
	}
	cwd := strings.TrimSpace(strOr(row.GetOr("cwd", nil)))
	if cwd != "" && !(truthy(row.GetOr("git_branch", nil)) && truthy(row.GetOr("git_repo_root", nil))) {
		persistSessionCwdAndScheduleGitMeta(ctx, session, cwd, db)
	}
}

// Re raises a disk full write failure for the caller to surface and debug logs any other
func workdirReraiseDiskFull(failure any, logMsg string) {
	if state.IsDiskFullError(failure) {
		panic(failure)
	}
	logger().Debug(logMsg, "error", failure)
}

// Transcript fields a branch seed row copies from its parent message
var workdirSeedFields = []string{"content", "reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items",
	"codex_message_items", "display_kind", "display_metadata", "timestamp"}

// Persists a seeded transcript once its row exists, exactly once per session
func persistBranchSeed(ctx context.Context, session *tuigateway.Session) {
	key := strOr(session.Get("session_key"))
	if key == "" || !truthy(session.Get("seeded")) || truthy(session.Get("_branch_seed_persisted")) {
		return
	}
	unlock := lockHistory(session)
	var live, seed []map[string]any
	for _, m := range historyList(session.Get("history")) {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		live = append(live, msg)
		role := msg["role"]
		if _, has := msg["role"]; !has {
			role = "user"
		}
		row := map[string]any{"role": role}
		for _, f := range workdirSeedFields {
			row[f] = msg[f]
		}
		for k, v := range state.MessageIdentity(msg, true) {
			row[k] = v
		}
		seed = append(seed, row)
	}
	unlock()
	if len(seed) == 0 {
		return
	}
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				workdirReraiseDiskFull(r, "branch seed persist failed")
			}
		}()
		chunk := 500
		if _, err := db.AppendMessagesBatch(ctx, key, seed, state.AppendGuards{}, &chunk); err != nil {
			panic(err)
		}
		unlock := lockHistory(session)
		state.SyncFlushedMessageMarkers(live, seed)
		unlock()
		session.Set("_branch_seed_persisted", true)
	}()
}

// Session id a staged submit row lives under, recorded at write time else the current best
func submitRowOwnerKey(staged map[string]any, session *tuigateway.Session) string {
	if recorded := strOr(staged[submitRowSessionKey]); recorded != "" {
		return recorded
	}
	return submitRowTargetKey(session)
}

// Writes the submitted user turn and returns the durable dict without slotting it, nil when nothing was written
func writeSubmitUserRow(ctx context.Context, session *tuigateway.Session, text any, displayKind string, acceptMetadata map[string]any) (staged map[string]any) {
	body, isText := text.(string)
	if !truthy(session.Get("session_key")) || !isText || strings.TrimSpace(body) == "" {
		return nil
	}
	staged = state.StampMessageTimestamp(map[string]any{"role": "user", "content": body}, nil)
	if displayKind != "" {
		staged["display_kind"] = displayKind
	}
	if len(acceptMetadata) > 0 {
		meta := make(map[string]any, len(acceptMetadata))
		for k, v := range acceptMetadata {
			meta[k] = v
		}
		staged["display_metadata"] = meta
	}
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return nil
	}
	target := submitRowTargetKey(session)
	written := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				workdirReraiseDiskFull(r, "submit-time user row persist failed")
				ok = false
			}
		}()
		var kind any
		if displayKind != "" {
			kind = displayKind
		}
		rowID, err := db.AppendMessage(ctx, target, map[string]any{"role": "user", "content": body, "display_kind": kind,
			"timestamp": staged["timestamp"], "message_uid": state.StampMessageUID(staged),
			"display_metadata": staged["display_metadata"]}, state.AppendGuards{})
		if err != nil {
			panic(err)
		}
		staged["_row_id"] = rowID
		return true
	}()
	if !written {
		return nil
	}
	staged[state.DBPersistedMarker] = true
	staged[submitRowSessionKey] = target
	return staged
}

// Writes the submitted user turn at send time and stages it on the session for the turn to adopt
func persistSubmitUserRow(ctx context.Context, session *tuigateway.Session, text any, displayKind string, acceptMetadata map[string]any) {
	session.Pop("_submit_user_row")
	if staged := writeSubmitUserRow(ctx, session, text, displayKind, acceptMetadata); staged != nil {
		session.Set("_submit_user_row", staged)
	}
}

// Hands the submit time row to the turn, first rewriting it when the prologue changed the prompt
func adoptSubmitUserRow(ctx context.Context, session *tuigateway.Session, a *agent.AIAgent, persistUserMessage any, text any) {
	raw, _ := session.Pop("_submit_user_row")
	staged, isDict := raw.(map[string]any)
	if !isDict || a == nil || !pyjsonEqual(staged["content"], text) {
		return
	}
	if !pyjsonEqual(staged["content"], persistUserMessage) {
		db, release := sessionDB(ctx, session)
		defer release()
		if db == nil {
			return
		}
		rowID, _ := staged["_row_id"].(int64)
		if _, err := db.SetUserMessageContent(ctx, submitRowOwnerKey(staged, session), rowID,
			state.DurableContent(persistUserMessage), nil); err != nil {
			logger().Debug("submit-time user row update failed; the turn writes its own row", "error", err)
			return
		}
		staged["content"] = persistUserMessage
	}
	a.AdoptPendingCLIUserMessage(staged)
}
