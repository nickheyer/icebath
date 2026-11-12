package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Refresh of a session's policy ready to commit with the stored receipt it replaces and its generation
type preparedRefresh struct {
	stored     any
	old        *LocalSessionPolicy
	policy     *LocalSessionPolicy
	secrets    *RecoveredSecrets
	replaced   []any
	generation int64
}

// Decoded JSON object of a policy column, the JSONDecodeError or TypeError json.loads and dict use raise
func loadsPolicyMap(text string) (map[string]any, error) {
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, &PyError{Kind: "JSONDecodeError", Msg: err.Error()}
	}
	out, ok := pyjson.Plain(parsed).(map[string]any)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(parsed) + "' object is not a mapping"}
	}
	return out, nil
}

// Deep copy through a JSON dump and load, the json.loads(json.dumps(value)) of the Python
func jsonRoundTrip(value any) (any, error) {
	text, err := pyjson.Dumps(value)
	if err != nil {
		return nil, &PyError{Kind: "TypeError", Msg: err.Error()}
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	return pyjson.Plain(parsed), nil
}

// Copies key from live into target, or drops it from target when live lacks it
func followLive(target map[string]any, live any, key string) error {
	switch source := live.(type) {
	case map[string]any:
		if value, ok := source[key]; ok {
			target[key] = value
		} else {
			delete(target, key)
		}
		return nil
	case string:
		if !strings.Contains(source, key) {
			delete(target, key)
			return nil
		}
		return &PyError{Kind: "TypeError", Msg: "string indices must be integers, not 'str'"}
	case []any:
		if !slices.ContainsFunc(source, func(item any) bool { return pyjson.Equal(item, key) }) {
			delete(target, key)
			return nil
		}
		return &PyError{Kind: "TypeError", Msg: "list indices must be integers or slices, not str"}
	}
	return &PyError{Kind: "TypeError", Msg: "argument of type '" + jsonValueTypeName(live) + "' is not iterable"}
}

// Mapping a dict() call builds from a config value, an empty one for a falsy value
func dictOf(value any) (map[string]any, error) {
	if !pyrepr.Truthy(value) {
		return map[string]any{}, nil
	}
	source, ok := value.(map[string]any)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "cannot convert dictionary update sequence element #0 to a sequence"}
	}
	out := make(map[string]any, len(source))
	for key, item := range source {
		out[key] = item
	}
	return out, nil
}

// Adds a section to the replaced set once
func addReplaced(replaced []any, section any) []any {
	if slices.Contains(replaced, section) {
		return replaced
	}
	return append(replaced, section)
}

// Policy with its config derived fields or MCP servers from the profile config, itself when nothing moved
func refreshedPolicy(ctx context.Context, policy *LocalSessionPolicy, live, terminal map[string]any, picked []string, mcpOnly bool) (*LocalSessionPolicy, *RecoveredSecrets, []any, error) {
	old, err := loadsPolicyMap(policy.ConfigJSON)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := loadsPolicyMap(policy.ConfigJSON)
	if err != nil {
		return nil, nil, nil, err
	}
	live = presentSections(live)
	secrets, replaced := &RecoveredSecrets{}, []any{}
	var toolsets []string
	if mcpOnly {
		toolsets = policy.Toolsets
		if terminal, err = loadsPolicyMap(policy.TerminalJSON); err != nil {
			return nil, nil, nil, err
		}
	} else {
		if toolsets, err = refreshSettings(ctx, policy, cfg, live, picked); err != nil {
			return nil, nil, nil, err
		}
		if terminal, err = refreshTerminal(policy, terminal, picked); err != nil {
			return nil, nil, nil, err
		}
		if err := extractConfigSecrets(terminal, secrets, secretPath{nil}); err != nil {
			return nil, nil, nil, err
		}
		replaced = addReplaced(replaced, nil)
	}
	toolsets, replaced, err = refreshMCPServers(ctx, policy, cfg, live, toolsets, secrets, replaced)
	if err != nil {
		return nil, nil, nil, err
	}
	frozenTerminal, err := loadsPolicyMap(policy.TerminalJSON)
	if err != nil {
		return nil, nil, nil, err
	}
	if pyjson.Equal(cfg, old) && slices.Equal(toolsets, policy.Toolsets) && pyjson.Equal(terminal, frozenTerminal) {
		moved, err := secretsMoved(policy.ConfigSecretRef, secrets, replaced)
		if err != nil {
			return nil, nil, nil, err
		}
		if !moved {
			return policy, &RecoveredSecrets{}, nil, nil
		}
	}
	configJSON, err := pyjson.Dumps(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	terminalJSON, err := pyjson.Dumps(terminal)
	if err != nil {
		return nil, nil, nil, err
	}
	refreshed := *policy
	refreshed.Toolsets, refreshed.ConfigJSON, refreshed.TerminalJSON = toolsets, configJSON, terminalJSON
	return &refreshed, secrets, replaced, nil
}

// Follows the config derived settings into the config and returns the toolsets they select
func refreshSettings(ctx context.Context, policy *LocalSessionPolicy, cfg, live map[string]any, picked []string) ([]string, error) {
	request, err := loadsPolicyMap(policy.RequestJSON)
	if err != nil {
		return nil, err
	}
	liveAgent := live["agent"]
	if !pyrepr.Truthy(liveAgent) {
		liveAgent = map[string]any{}
	}
	agent, err := dictOf(cfg["agent"])
	if err != nil {
		return nil, err
	}
	_, requestedTurns := request["max_turns"]
	keys := []string{}
	if !requestedTurns {
		keys = append(keys, "max_turns")
	}
	_, requestedReasoning := request["reasoning"]
	if !sessionPinned(cfg, "reasoning", requestedReasoning || slices.Contains(picked, "reasoning")) {
		keys = append(keys, "reasoning_effort", "reasoning_overrides")
	}
	toolsets := policy.Toolsets
	if _, pick := request["toolsets"]; !pick && policy.EditorMCPJSON == nil {
		keys = append(keys, "disabled_toolsets")
		if err := followLive(cfg, live, "platform_toolsets"); err != nil {
			return nil, err
		}
		selected, err := resolveToolsets(ctx, map[string]any{}, live, policy.Source, policy.SafeMode, policy.CodingPosture, false)
		if err != nil {
			return nil, err
		}
		toolsets = sortedNames(selected)
	}
	for _, key := range keys {
		if err := followLive(agent, liveAgent, key); err != nil {
			return nil, err
		}
	}
	if _, held := cfg["agent"]; len(agent) > 0 || held {
		cfg["agent"] = agent
	}
	if !requestedTurns {
		if err := followLive(cfg, live, "max_turns"); err != nil {
			return nil, err
		}
	}
	if err := followLive(cfg, live, "checkpoints"); err != nil {
		return nil, err
	}
	if request["checkpoints"] == true {
		section := map[string]any{}
		switch held := cfg["checkpoints"].(type) {
		case map[string]any:
			for key, value := range held {
				section[key] = value
			}
		default:
			if pyrepr.Truthy(held) {
				section["enabled"] = true
			}
		}
		section["enabled"] = true
		cfg["checkpoints"] = section
	}
	return toolsets, nil
}

// Sorted names of a toolset selection
func sortedNames(selected map[string]bool) []string {
	out := make([]string, 0, len(selected))
	for name := range selected {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Terminal scope of a refresh with the session folder kept where the user or the backend pins it
func refreshTerminal(policy *LocalSessionPolicy, terminal map[string]any, picked []string) (map[string]any, error) {
	out := make(map[string]any, len(terminal)+1)
	for key, value := range terminal {
		out[key] = value
	}
	request, err := loadsPolicyMap(policy.RequestJSON)
	if err != nil {
		return nil, err
	}
	if request["cwd_explicit"] == true || slices.Contains(picked, "workspace") || terminalUsesHostCwd(out) {
		out["TERMINAL_CWD"] = policy.Cwd
	}
	return out, nil
}

// Items a Python for loop over a decoded JSON value yields, a dict giving its keys
func pickItems(value any) ([]any, error) {
	if dict, ok := value.(map[string]any); ok {
		out := make([]any, 0, len(dict))
		for key := range dict {
			out = append(out, key)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].(string) < out[j].(string) })
		return out, nil
	}
	return pyIterable(value)
}

// Profile MCP servers as the session's own selection takes them now, definitions into the config and names into the toolsets
func refreshMCPServers(ctx context.Context, policy *LocalSessionPolicy, cfg, live map[string]any, toolsets []string, secrets *RecoveredSecrets, replaced []any) ([]string, []any, error) {
	if policy.EditorMCPJSON != nil {
		return toolsets, replaced, nil
	}
	request, err := loadsPolicyMap(policy.RequestJSON)
	if err != nil {
		return nil, nil, err
	}
	frozen, _ := cfg["mcp_servers"].(map[string]any)
	current, _ := live["mcp_servers"].(map[string]any)
	pick, hasPick := request["toolsets"]
	picked := map[string]bool{}
	enabled := tools.EnabledMCPServerNames(config.Tree(live), nil)
	if hasPick && pick != nil {
		known := map[string]bool{}
		for name := range frozen {
			known[name] = true
		}
		for name := range current {
			known[name] = true
		}
		server := func(name string) string { return mcpPickServer(known, name) }
		items, err := pickItems(pick)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range items {
			if name, isStr := item.(string); isStr && known[server(name)] {
				picked[server(name)] = true
			}
		}
		kept := []any{}
		for _, item := range items {
			name, isStr := item.(string)
			if !isStr || !(picked[server(name)] && !enabled[server(name)]) {
				kept = append(kept, item)
			}
		}
		pick = kept
	}
	params := map[string]any{}
	if hasPick && pick != nil {
		params["toolsets"] = pick
	}
	copied, err := jsonRoundTrip(live)
	if err != nil {
		return nil, nil, err
	}
	copiedLive, _ := copied.(map[string]any)
	selected, err := resolveToolsets(ctx, params, copiedLive, policy.Source, policy.SafeMode, policy.CodingPosture, false)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return toolsets, replaced, nil
		}
		return nil, nil, err
	}
	disabledPicks := map[string]any{}
	for name := range picked {
		if _, present := current[name]; !present {
			disabledPicks[name] = map[string]any{"enabled": false}
		}
	}
	if raw, present := live["mcp_servers"]; present || len(disabledPicks) > 0 {
		var source any = raw
		if len(disabledPicks) > 0 {
			merged := map[string]any{}
			for name, value := range current {
				merged[name] = value
			}
			for name, value := range disabledPicks {
				merged[name] = value
			}
			source = merged
		}
		servers, err := jsonRoundTrip(source)
		if err != nil {
			return nil, nil, err
		}
		cfg["mcp_servers"] = servers
		if err := extractConfigSecrets(servers, secrets, secretPath{"mcp_servers"}); err != nil {
			return nil, nil, err
		}
	} else {
		delete(cfg, "mcp_servers")
	}
	replaced = addReplaced(replaced, "mcp_servers")
	names := map[string]bool{}
	for _, name := range toolsets {
		_, inFrozen := frozen[name]
		_, inCurrent := current[name]
		if !inFrozen && !inCurrent {
			names[name] = true
		}
	}
	for name := range selected {
		if enabled[name] && (len(picked) == 0 || picked[name]) {
			names[name] = true
		}
	}
	return sortedNames(names), replaced, nil
}

// Server a pick name selects, mcp-name being the bare alias unless a server has that name
func mcpPickServer(servers map[string]bool, name string) string {
	if strings.HasPrefix(name, "mcp-") && !servers[name] {
		return name[4:]
	}
	return name
}

// Stored policy, its refresh from this profile home's files and the generation it was derived at, nil when nothing moved
func prepareRefresh(ctx context.Context, a *SessionAuthority, sessionID string, mcpOnly, adoptOnly bool) (*preparedRefresh, error) {
	row, err := a.DB.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
	}
	if _, ok := row.Get("runtime_generation"); !ok {
		return nil, &pyKeyError{"runtime_generation"}
	}
	var generation int64
	if value := rowInt(row, "runtime_generation"); value != nil {
		generation = *value
	}
	receipt, err := a.DB.LocalReceipt(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	stored, ok := receipt.Get("policy")
	if !ok {
		return nil, &pyKeyError{"policy"}
	}
	old, err := restorePolicy(stored)
	if err != nil {
		return nil, err
	}
	if _, surface := Surfaces[old.Source]; old.IgnoreUserConfig || !surface {
		return nil, nil
	}
	dbPath, err := pypath.Resolve(a.DB.Path())
	if err != nil {
		return nil, err
	}
	profileHome := filepath.Dir(dbPath)
	terminal := map[string]any{}
	if !mcpOnly && !adoptOnly {
		scope, err := tools.BuildProfileTerminalScope(ctx, profileHome, nil)
		if err != nil {
			return nil, err
		}
		for key, value := range scope {
			terminal[key] = value
		}
	}
	live := loadGatewayConfig(ctx, filepath.Join(profileHome, "config.yaml"))
	picked, err := a.DB.PolicyPicks(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	base := old
	if !mcpOnly {
		if base, err = followBotChat(ctx, a, sessionID, old, live, picked); err != nil {
			return nil, err
		}
	}
	if adoptOnly {
		if base == old {
			return nil, nil
		}
		return &preparedRefresh{stored: stored, old: old, policy: base, secrets: &RecoveredSecrets{}, generation: generation}, nil
	}
	policy, secrets, replaced, err := refreshedPolicy(ctx, base, live, terminal, picked, mcpOnly)
	if err != nil {
		return nil, err
	}
	if policy == old {
		return nil, nil
	}
	return &preparedRefresh{stored: stored, old: old, policy: policy, secrets: secrets, replaced: replaced, generation: generation}, nil
}

// Binds the refreshed policy's private values like a policy mutation, the fresh sections taking the profile's current values
func rebindRefresh(ctx context.Context, a *SessionAuthority, sessionID string, old, policy *LocalSessionPolicy, fresh *RecoveredSecrets, replaced []any) (*LocalSessionPolicy, error) {
	skip := map[any]bool{}
	if policy.FollowsProfile() {
		for _, name := range FollowSections {
			skip[name] = true
		}
	}
	for _, section := range replaced {
		skip[section] = true
	}
	secrets := &RecoveredSecrets{}
	if old.ConfigSecretRef != nil && *old.ConfigSecretRef != "" {
		recovered, err := recoverConfigSecretsSkipping(ctx, a, old, skip)
		if err != nil {
			return nil, err
		}
		secrets = recovered
	}
	for i, path := range fresh.paths {
		secrets.Set(path, fresh.values[i])
	}
	key, err := launchKey(a, old)
	if err != nil {
		return nil, err
	}
	return bindLaunchKey(ctx, a, sessionID, policy, key, secrets)
}

// Frozen Bot Chats an open found to adopt but could not commit, retried by the next open
func pendingAdoptionsOf(a *SessionAuthority) map[string]bool {
	if a.pendingAdoptions == nil {
		a.pendingAdoptions = map[string]bool{}
	}
	return a.pendingAdoptions
}

// Marks or clears a pending adoption of a session under the authority lock
func setPendingAdoption(a *SessionAuthority, sessionID string, pending bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if pending {
		pendingAdoptionsOf(a)[sessionID] = true
	} else {
		delete(pendingAdoptionsOf(a), sessionID)
	}
}

// Whether an open of the session still owes its frozen Bot Chat adoption
func adoptionPending(a *SessionAuthority, sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return pendingAdoptionsOf(a)[sessionID]
}

// Waits out a refresh of the session in flight before an attach reads its policy
func refreshSettled(ctx context.Context, a *SessionAuthority, sessionID string) error {
	for {
		a.mu.Lock()
		running := a.policyRefreshes[sessionID]
		a.mu.Unlock()
		if running == nil {
			return nil
		}
		select {
		case <-running.done:
			return nil
		default:
		}
		slog.Debug(fmt.Sprintf("Session %s waits for its policy refresh to publish", sessionID))
		select {
		case <-running.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Commits an idle local session's refresh as its next policy generation, true when one was committed
func commitRefresh(ctx context.Context, a *SessionAuthority, ref SessionRef, mcpOnly, adoptOnly bool) (bool, error) {
	if err := refreshSettled(ctx, a, ref.SessionID); err != nil {
		return false, err
	}
	task := trackMutation(ctx, a, func(taskCtx context.Context) (any, error) {
		return commitRefreshTask(taskCtx, a, ref, mcpOnly, adoptOnly)
	})
	a.mu.Lock()
	if a.policyRefreshes == nil {
		a.policyRefreshes = map[string]*trackedTask{}
	}
	a.policyRefreshes[ref.SessionID] = task
	a.mu.Unlock()
	go func() {
		<-task.done
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.policyRefreshes[ref.SessionID] == task {
			delete(a.policyRefreshes, ref.SessionID)
		}
	}()
	select {
	case <-task.done:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if task.err != nil {
		return false, task.err
	}
	committed, _ := task.result.(bool)
	return committed, nil
}

// Prepares, rebinds and commits one refresh, publishing it as the commit's own follow up
func commitRefreshTask(ctx context.Context, a *SessionAuthority, ref SessionRef, mcpOnly, adoptOnly bool) (bool, error) {
	prepared, err := prepareRefresh(ctx, a, ref.SessionID, mcpOnly, adoptOnly)
	if err != nil {
		return false, err
	}
	if prepared == nil {
		if !mcpOnly {
			setPendingAdoption(a, ref.SessionID, false)
		}
		return false, nil
	}
	if !mcpOnly && prepared.policy.FollowsProfile() && !prepared.old.FollowsProfile() {
		setPendingAdoption(a, ref.SessionID, true)
	}
	newTerminal, err := loadsPolicyMap(prepared.policy.TerminalJSON)
	if err != nil {
		return false, err
	}
	oldTerminal, err := loadsPolicyMap(prepared.old.TerminalJSON)
	if err != nil {
		return false, err
	}
	moved := !pyjson.Equal(newTerminal, oldTerminal)
	policy, err := rebindRefresh(ctx, a, ref.SessionID, prepared.old, prepared.policy, prepared.secrets, prepared.replaced)
	if err != nil {
		return false, err
	}
	write := func(ctx context.Context) (any, error) {
		return a.DB.CommitRefreshedPolicy(ctx, a.Epoch, ref.SessionID, prepared.stored, policy.asDict(), prepared.generation)
	}
	then := func(ctx context.Context, _ any) (any, error) { return nil, publishRefresh(ctx, a, ref, moved) }
	if _, err := trackedWrite(ctx, a, write, then, false, nil); err != nil {
		return false, err
	}
	if !mcpOnly {
		setPendingAdoption(a, ref.SessionID, false)
	}
	return true, nil
}

// Commits the config refresh of an idle local session before it is attached, never failing the resume
func refreshResumedPolicy(ctx context.Context, a *SessionAuthority, ref SessionRef, attached bool) {
	if attached && !adoptionPending(a, ref.SessionID) {
		return
	}
	if _, err := commitRefresh(ctx, a, ref, false, attached); err != nil {
		noteSkippedRefresh(ref.SessionID, err)
	}
}

// Logs a resume that kept its stored policy, INFO for a busy session and WARNING for anything else
func noteSkippedRefresh(sessionID string, err error) {
	reason := ""
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		reason = refusal.Reason
	}
	if reason == "" {
		reason = pyErrorKind(err)
	}
	if reason == "" && pypath.IsOSError(err) {
		reason = pypath.OSErrorClass(err)
	}
	if reason == "" {
		reason = pyrepr.ExceptionName(err)
	}
	message := fmt.Sprintf("Session %s keeps its stored policy on resume: %s", sessionID, reason)
	if reason == "session_busy" || reason == "unknown_execution" || reason == "revision_conflict" {
		slog.Info(message)
		return
	}
	slog.Warn(message)
}

// Publishes a committed refresh to the adapter, the drain and every attached viewer
func publishRefresh(ctx context.Context, a *SessionAuthority, ref SessionRef, moved bool) error {
	if moved {
		if err := dropSessionTerminal(ctx, a, ref); err != nil {
			return err
		}
	}
	if err := publishLocalPolicy(ctx, a, ref.SessionID, true); err != nil {
		return err
	}
	if err := scheduleSession(ctx, a, ref); err != nil {
		return err
	}
	if err := a.publishPending(ctx, ref); err != nil {
		return err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return err
	}
	info, err := localSessionInfo(ctx, a, ref)
	if err != nil {
		return err
	}
	live.EventStream.Publish(ref.SessionID, info, "session.info", nil)
	return nil
}
