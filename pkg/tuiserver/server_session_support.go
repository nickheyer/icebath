package tuiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Personalities a config offers, the built ins merged with its own, assigned by the command wiring
var AvailablePersonalities func(cfg map[string]any) map[string]any

// Stable leading text of the self replacing model switch marker
const modelSwitchMarkerPrefix = "[System: The active model for this chat has changed to "

// Whether a history entry is a model switch marker
func isModelSwitchMarker(entry any) bool {
	msg, isDict := entry.(map[string]any)
	if !isDict {
		return false
	}
	content, isText := msg["content"].(string)
	return isText && strings.HasPrefix(content, modelSwitchMarkerPrefix)
}

// Whether a history entry is a mid turn user pivot, a model switch or a personality change
func isPivotMarker(entry any) bool {
	if isModelSwitchMarker(entry) {
		return true
	}
	msg, isDict := entry.(map[string]any)
	return isDict && msg["display_kind"] == "personality_switch"
}

// Rebuilds and stores the system prompt of a live session after its runtime identity changed
func persistLiveSessionSystemPrompt(ctx context.Context, session *tuigateway.Session) {
	a, sessionKey, db, ok := liveSessionAgentDB(ctx, session)
	if !ok {
		return
	}
	cwd := sessionCwd(ctx, session)
	sessionCtx, env := setSessionContext(ctx, sessionKey, &cwd, "")
	defer clearSessionContext(env)
	defer func() {
		if r := recover(); r != nil {
			logger().Warn(fmt.Sprintf("failed to persist live session system prompt for session %s", sessionKey), "error", r)
		}
	}()
	scoped := sessionProfileRuntimeScope(sessionCtx, session, true)
	prompt, err := agent.BuildSystemPrompt(scoped, a, nil)
	if err != nil {
		panic(err)
	}
	a.SetCachedSystemPrompt(prompt)
	target := a.SessionID()
	if target == "" {
		target = sessionKey
	}
	if err := db.UpdateSystemPrompt(sessionCtx, target, &prompt); err != nil {
		panic(err)
	}
}

// Pushes live usage snapshots while a turn runs and returns the stop that halts and joins the pusher
func startUsageTicker(ctx context.Context, sid string, a *agent.AIAgent, interval time.Duration) (stop func()) {
	var baseline map[string]any
	suppress(func() { baseline = getUsage(ctx, a) })
	halt, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer threadExceptHook("usage-ticker-" + sid)
		defer close(done)
		last := baseline
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-halt:
				return
			case <-ticker.C:
			}
			stopped := false
			suppress(func() {
				usage := getUsage(ctx, a)
				if reflect.DeepEqual(usage, last) {
					return
				}
				last = usage
				select {
				case <-halt:
					stopped = true
					return
				default:
				}
				tuigateway.Emit(ctx, "session.usage", sid, map[string]any{"usage": usage})
			})
			if stopped {
				return
			}
		}
	}()
	var once bool
	return func() {
		if !once {
			once = true
			close(halt)
		}
		<-done
	}
}

// Hands a dedicated profile store to the agent holding it, never the shared launch store
func transferDBToAgent(ctx context.Context, a *agent.AIAgent, db *state.DB) (transferred bool) {
	defer func() {
		if recover() != nil {
			transferred = false
		}
	}()
	if a == nil || db == nil || a.SessionDB() != db {
		return false
	}
	if db == getDB(ctx) {
		logger().Warn("Refused transfer of the shared launch SessionDB to a session agent — the caller's owns_db gate should have prevented this.")
		return false
	}
	a.SetOwnsSessionDB(true)
	return true
}

// Dedicated handle on a profile state.db, an error rather than any fallback to the launch store
func openProfileSessionDB(ctx context.Context, profileHome string) (*state.DB, error) {
	dbPath := filepath.Join(profileHome, "state.db")
	db, err := state.Acquire(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("profile session store unavailable: %s: %s", dbPath, err)
	}
	return db, nil
}

// Warning for empty config sections and an unknown display personality, empty when healthy
func probeConfigHealth(cfg map[string]any) string {
	if cfg == nil {
		return ""
	}
	var warnings []string
	var nullKeys []string
	for k, v := range cfg {
		if v == nil {
			nullKeys = append(nullKeys, k)
		}
	}
	if len(nullKeys) > 0 {
		sort.Strings(nullKeys)
		quoted := make([]string, len(nullKeys))
		for i, k := range nullKeys {
			quoted[i] = "`" + k + "`"
		}
		warnings = append(warnings, fmt.Sprintf("config.yaml has empty section(s): %s. Remove the line(s) or set them to `{}` — "+
			"empty sections silently drop nested settings.", strings.Join(quoted, ", ")))
	}
	if display, isDict := cfg["display"].(map[string]any); isDict {
		personality := strings.ToLower(strings.TrimSpace(strOr(display["personality"])))
		if personality != "" && personality != "default" && personality != "none" && personality != "neutral" {
			suppress(func() {
				if _, known := AvailablePersonalities(cfg)[personality]; !known {
					warnings = append(warnings, fmt.Sprintf("`display.personality: %s` does not match any built-in or "+
						"`agent.personalities` entry; personality overlay will be skipped.", personality))
				}
			})
		}
	}
	return strings.TrimSpace(strings.Join(warnings, " "))
}

// Refreshes the tool snapshot of a session whose MCP discovery lands after its build, before its first turn
func scheduleMCPLateRefresh(ctx context.Context, sid string, a *agent.AIAgent) {
	if !mcpDiscoveryInFlight(ctx) {
		return
	}
	go func() {
		defer threadExceptHook("tui-mcp-late-refresh-" + sid)
		bound := 30 * time.Second
		if !joinMCPDiscovery(ctx, &bound) {
			return
		}
		var info map[string]any
		func() {
			sessionsLock.Lock()
			defer sessionsLock.Unlock()
			session := tuigateway.SessionRecord(sid)
			if session == nil || sessionAgent(session) != a {
				return
			}
			if a.UserTurnCount() > 0 || a.APICallCount() > 0 {
				return
			}
			var added map[string]bool
			failed := func() (failed bool) {
				defer func() {
					if r := recover(); r != nil {
						logger().Warn(fmt.Sprintf("Late MCP refresh: tool snapshot rebuild failed for %s: %v", sid, r))
						failed = true
					}
				}()
				added = mcp.RefreshAgentMCPTools(ctx, a, mcp.RefreshOptions{Quiet: true})
				return false
			}()
			if failed || len(added) == 0 {
				return
			}
			info = sessionInfo(ctx, a, session)
		}()
		if info != nil {
			tuigateway.Emit(ctx, "session.info", sid, info)
		}
	}()
}

// Adopts the stored row cwd and fills missing git metadata, or persists a fresh cwd
func hydrateSessionCwd(ctx context.Context, sid, key string, sessionDB *state.DB, profileHome string) {
	db, ownsDB := sessionDB, false
	if db == nil && profileHome == "" {
		db = getDB(ctx)
	} else if db == nil {
		opened, err := openProfileSessionDB(ctx, profileHome)
		if err != nil {
			logger().Warn(fmt.Sprintf("profile session store unavailable for %s — skipping cwd hydration instead of touching the launch state.db", profileHome), "error", err)
		} else {
			db, ownsDB = opened, true
		}
	}
	defer func() {
		if ownsDB && db != nil {
			suppress(func() { state.ReleaseOrClose(db) })
		}
	}()
	if db == nil {
		return
	}
	row, err := db.GetSession(ctx, key)
	if err != nil {
		panic(err)
	}
	var storedCwd any
	if row != nil {
		storedCwd = row.GetOr("cwd", nil)
	}
	if row != nil && resumableStoredCwd(ctx, storedCwd, profileHome) != "" {
		remote := !hostCwdIsTerminalCwd(ctx, profileHome)
		func() {
			sessionsLock.Lock()
			defer sessionsLock.Unlock()
			if live := tuigateway.SessionRecord(sid); live != nil {
				live.Set("cwd", storedCwd)
				if remote {
					live.Set("explicit_cwd", true)
				}
			}
		}()
		if !truthy(row.GetOr("git_branch", nil)) || !truthy(row.GetOr("git_repo_root", nil)) {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger().Debug("failed to enrich resumed session git metadata", "error", r)
					}
				}()
				persistSessionCwdAndScheduleGitMeta(ctx, tuigateway.SessionRecord(sid), strOr(storedCwd), db)
			}()
		}
		return
	}
	live := tuigateway.SessionRecord(sid)
	if (row == nil || !truthy(storedCwd) || isOtherProfilesCwd(ctx, strOr(storedCwd), profileHome)) && !isRemoteLaunchCwd(ctx, live) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger().Debug("failed to persist resumed session cwd", "error", r)
				}
			}()
			persistSessionCwdAndScheduleGitMeta(ctx, live, strOr(live.Get("cwd")), db)
		}()
	}
}
