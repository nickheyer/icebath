package dashboard

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Own profile name of this process, empty unless the name provably resolves back to the process home
func servingProfileName(ctx context.Context) string {
	name := pystr.Strip(profiles.GetActiveProfileName(ctx))
	if name == "" || name == "custom" {
		return ""
	}
	if profiles.ProfileMatchesHome(ctx, name, home.Process()) {
		return name
	}
	return ""
}

// Whether a profile names a home other than this process's own, an unknown profile counting as other
func isOtherProfile(profile string) (bool, error) {
	if isCurrentProfile(profile) {
		return false, nil
	}
	target, err := resolveProfileDir(pystr.Strip(profile))
	if err != nil {
		if _, ok := asHTTPException(err); ok {
			return true, nil
		}
		return false, err
	}
	resolvedTarget, err := pypath.Resolve(target)
	if err != nil {
		return false, err
	}
	resolvedHome, err := pypath.Resolve(home.Process())
	if err != nil {
		return false, err
	}
	return resolvedTarget != resolvedHome, nil
}

// Normalized approvals mode of an in memory config document, the shipped default when unset
func approvalModeOf(cfg map[string]any) string {
	defaultMode := any("manual")
	if defApprovals, ok := config.Defaults()["approvals"].(map[string]any); ok {
		if m, held := defApprovals["mode"]; held {
			defaultMode = m
		}
	}
	mode := defaultMode
	if approvals, ok := cfg["approvals"].(map[string]any); ok {
		if m, held := approvals["mode"]; held {
			mode = m
		}
	}
	return tools.NormalizeApprovalMode(mode)
}

// Re-emits session info to the live gateway sessions of this process, logging a failed broadcast
func broadcastGatewaySessionInfo(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			routersLog().Error("session.info broadcast after config save failed", "panic", fmt.Sprint(r))
		}
	}()
	tuiserver.BroadcastSessionInfo(ctx)
}

// Alias metadata a gateway may advertise on a model row
var modelEntryMetadata = []string{"canonical_model", "reasoning_effort"}

// Model rows of an OpenAI compatible models response with their alias metadata, empty on any error
func parseModelEntries(resp *pyhttpx.Response) []*pyjson.Object {
	entries := []*pyjson.Object{}
	if resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return entries
	}
	payload, err := resp.JSON()
	if err != nil {
		return entries
	}
	data := payload
	if obj, ok := payload.(*pyjson.Object); ok {
		data = obj.GetOr("data", nil)
	}
	items, ok := data.([]any)
	if !ok {
		return entries
	}
	for _, item := range items {
		raw := item
		obj, isDict := item.(*pyjson.Object)
		if isDict {
			raw = obj.GetOr("id", nil)
		}
		modelID := pystr.Strip(pyrepr.Str(pyrepr.Or(raw, "")))
		if modelID == "" {
			continue
		}
		entry := pyjson.ObjectOf("id", modelID)
		if isDict {
			for _, key := range modelEntryMetadata {
				if value := pystr.Strip(pyrepr.Str(pyrepr.Or(obj.GetOr(key, nil), ""))); value != "" {
					entry.Set(key, value)
				}
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// Bare model ids of an OpenAI compatible models response
func parseModelIDs(resp *pyhttpx.Response) []string {
	ids := []string{}
	for _, entry := range parseModelEntries(resp) {
		ids = append(ids, pyrepr.Str(entry.GetOr("id", nil)))
	}
	return ids
}

// Picker rows of the plugin registered terminal backends, empty when discovery or the registry fails
func pluginTerminalBackendRows(ctx context.Context) []*pyjson.Object {
	rows := []*pyjson.Object{}
	func() {
		defer func() { _ = recover() }()
		plugins.DiscoverPlugins(ctx, false)
	}()
	var providers []tools.BackendProvider
	func() {
		defer func() { _ = recover() }()
		providers = tools.ListTerminalEnvProviders(ctx, "")
	}()
	for _, p := range providers {
		func() {
			defer func() { _ = recover() }()
			rows = append(rows, pyjson.ObjectOf("name", pystr.Lower(pystr.Strip(p.Name())), "label", p.DisplayName(),
				"description", p.Description()))
		}()
	}
	return rows
}

var nonSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

// Unique per skill hub action name with its log file registered, a readable slug plus a hash
func hubActionName(verb, key string) string {
	slug := strings.Trim(nonSlugRE.ReplaceAllString(pystr.Lower(key), "-"), "-")
	if len(slug) > 48 {
		slug = slug[:48]
	}
	if slug == "" {
		slug = "skill"
	}
	sum := sha1.Sum([]byte(key))
	name := fmt.Sprintf("skills-%s-%s-%s", verb, slug, hex.EncodeToString(sum[:])[:8])
	actionMu.Lock()
	if _, held := actionLogFiles[name]; !held {
		actionLogFiles[name] = "action-" + name + ".log"
	}
	actionMu.Unlock()
	return name
}

// Identifier to installed lock entry of hub skills in a profile, empty when the lock is unreadable
func installedHubIdentifiers(ctx context.Context, profile string) (*pyjson.Object, error) {
	hub := skills.NewHub(skills.NewScope(home.Dir(ctx), nil))
	lockPath := ""
	if !isCurrentProfile(profile) {
		profileDir, err := resolveProfileDir(pystr.Strip(profile))
		if err != nil {
			return nil, err
		}
		lockPath = filepath.Join(profileDir, "skills", ".hub", "lock.json")
	}
	installed, err := hub.LockFile(lockPath).ByIdentifierObject()
	if err != nil {
		return pyjson.NewObject(), nil
	}
	out := pyjson.NewObject()
	for _, identifier := range installed.Keys {
		entry := installed.Vals[identifier].(*pyjson.Object)
		row := pyjson.NewObject()
		for _, k := range []string{"name", "trust_level", "scan_verdict"} {
			row.Set(k, entry.GetOr(k, nil))
		}
		out.Set(identifier, row)
	}
	return out, nil
}

// Value of fn, def when fn panics
func safeValue[T any](fn func() T, def T) (out T) {
	defer func() {
		if r := recover(); r != nil {
			out = def
		}
	}()
	return fn()
}

// Profile listing row read directly from disk when the profiles module listing fails
func fallbackProfileEntry(ctx context.Context, name, profileHome string, isDefault, hasEnv bool, gatewayRunning func() bool) *pyjson.Object {
	type modelPair struct{ model, provider any }
	pair := safeValue(func() modelPair {
		model, prov, err := profiles.ReadConfigModel(ctx, profileHome)
		if err != nil {
			return modelPair{}
		}
		return modelPair{model, prov}
	}, modelPair{})
	meta := safeValue(func() profiles.ProfileMeta { return profiles.ReadProfileMeta(profileHome) }, profiles.ProfileMeta{})
	return pyjson.ObjectOf(
		"name", name, "path", profileHome, "is_default", isDefault, "model", pair.model,
		"provider", pair.provider, "has_env", hasEnv,
		"skill_count", safeValue(func() int { return manage.CachedSkillCount(profileHome) }, 0),
		"gateway_running", safeValue(gatewayRunning, false),
		"description", meta.Description, "description_auto", meta.DescriptionAuto,
		"bot_title", meta.BotTitle,
		"distribution_name", nil, "distribution_version", nil, "distribution_source", nil,
		"has_alias", false)
}

// Whether a path exists and resolves to a directory
func isDirPath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Profile listing rows of the default home and every valid profile directory, sorted by name
func fallbackProfileDicts(ctx context.Context) []*pyjson.Object {
	rows := []*pyjson.Object{}
	defaultHome := profiles.DefaultHome()
	if isDirPath(defaultHome) {
		_, envErr := os.Stat(filepath.Join(defaultHome, ".env"))
		rows = append(rows, fallbackProfileEntry(ctx, "default", defaultHome, true, envErr == nil,
			func() bool { return manage.CheckGatewayRunning(defaultHome) }))
	}
	root := profiles.ProfilesRoot()
	if !isDirPath(root) {
		return rows
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return rows
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		profileHome := filepath.Join(root, entry.Name())
		if !isDirPath(profileHome) || !home.ProfileIDRe.MatchString(strings.TrimSuffix(entry.Name(), "\n")) {
			continue
		}
		name := entry.Name()
		hasEnv := safeValue(func() bool {
			_, err := os.Stat(filepath.Join(profileHome, ".env"))
			return err == nil
		}, false)
		rows = append(rows, fallbackProfileEntry(ctx, name, profileHome, false, hasEnv, func() bool {
			return manage.CheckGatewayRunning(profileHome) || manage.ServedByRunningMultiplexer(name)
		}))
	}
	return rows
}

// Counters an auxiliary usage row carries
var auxCounters = []string{"input_tokens", "output_tokens", "estimated_cost", "api_calls"}

// Numeric sum of two counter values with None and missing read as zero
func addCounter(a, b any) any {
	af, aIsFloat := a.(float64)
	bf, bIsFloat := b.(float64)
	if aIsFloat || bIsFloat {
		if !aIsFloat {
			af = counterFloat(a)
		}
		if !bIsFloat {
			bf = counterFloat(b)
		}
		return af + bf
	}
	return counterInt(a) + counterInt(b)
}

// Integer value of a counter, zero for None or a non number
func counterInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// Float value of a counter, zero for None or a non number
func counterFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return float64(counterInt(v))
}

// Counter value or zero for a falsy one, the row.get(key) or 0 read
func counterOr0(row *pyjson.Object, key string) any {
	v := row.GetOr(key, nil)
	if !pyrepr.Truthy(v) {
		return int64(0)
	}
	return v
}

// Input plus output tokens of a usage row
func tokenVolume(row *pyjson.Object) float64 {
	return counterFloat(addCounter(counterOr0(row, "input_tokens"), counterOr0(row, "output_tokens")))
}

// Per model and task auxiliary usage within the window, empty when the table predates the task column
func auxUsageRows(ctx context.Context, db *state.SessionDB, cutoff float64) []*pyjson.Object {
	rows, err := db.ReadAll(ctx, `
            SELECT u.model,
                   u.task,
                   u.billing_provider,
                   SUM(u.input_tokens) as input_tokens,
                   SUM(u.output_tokens) as output_tokens,
                   SUM(u.cache_read_tokens) as cache_read_tokens,
                   SUM(u.reasoning_tokens) as reasoning_tokens,
                   COALESCE(SUM(u.estimated_cost_usd), 0) as estimated_cost,
                   COUNT(DISTINCT u.session_id) as sessions,
                   SUM(COALESCE(u.api_call_count, 0)) as api_calls,
                   MAX(u.last_seen) as last_used_at
            FROM session_model_usage u
            JOIN sessions s ON s.id = u.session_id
            WHERE s.started_at > ? AND u.task != ''
            GROUP BY u.model, u.task, u.billing_provider
            ORDER BY SUM(u.input_tokens) + SUM(u.output_tokens) DESC
        `, cutoff)
	if err != nil {
		return []*pyjson.Object{}
	}
	return rows
}

// Folds auxiliary usage into the per model list, models seen only through aux calls getting their own entry
func mergeAuxIntoByModel(byModel, auxRows []*pyjson.Object) []*pyjson.Object {
	if len(auxRows) == 0 {
		return byModel
	}
	merged := pyjson.NewObject()
	for _, row := range byModel {
		merged.Set(pyrepr.Str(pyrepr.Or(row.GetOr("model", nil), "unknown")), row)
	}
	for _, aux := range auxRows {
		model := pyrepr.Str(pyrepr.Or(aux.GetOr("model", nil), "unknown"))
		raw, held := merged.Get(model)
		if !held {
			raw = pyjson.ObjectOf("model", model, "input_tokens", int64(0), "output_tokens", int64(0),
				"estimated_cost", int64(0), "sessions", int64(0), "api_calls", int64(0))
			merged.Set(model, raw)
		}
		target := raw.(*pyjson.Object)
		for _, key := range auxCounters {
			target.Set(key, addCounter(counterOr0(target, key), counterOr0(aux, key)))
		}
		task := pyjson.ObjectOf("task", pyrepr.Or(aux.GetOr("task", nil), ""))
		for _, key := range auxCounters {
			task.Set(key, counterOr0(aux, key))
		}
		tasks, ok := target.GetOr("aux_tasks", nil).([]any)
		if _, held := target.Get("aux_tasks"); !held {
			tasks, ok = []any{}, true
		}
		if ok {
			target.Set("aux_tasks", append(tasks, task))
		}
	}
	out := make([]*pyjson.Object, 0, merged.Len())
	for _, k := range merged.Keys {
		out = append(out, merged.Vals[k].(*pyjson.Object))
	}
	sort.SliceStable(out, func(i, j int) bool { return tokenVolume(out[i]) > tokenVolume(out[j]) })
	return out
}

// Auxiliary usage summed across models into one row per task
func auxTaskSummary(auxRows []*pyjson.Object) []*pyjson.Object {
	byTask := pyjson.NewObject()
	for _, aux := range auxRows {
		task := pyrepr.Str(pyrepr.Or(aux.GetOr("task", nil), ""))
		raw, held := byTask.Get(task)
		if !held {
			raw = pyjson.ObjectOf("task", task, "input_tokens", int64(0), "output_tokens", int64(0),
				"estimated_cost", int64(0), "api_calls", int64(0), "models", []any{})
			byTask.Set(task, raw)
		}
		d := raw.(*pyjson.Object)
		for _, key := range auxCounters {
			d.Set(key, addCounter(d.GetOr(key, int64(0)), counterOr0(aux, key)))
		}
		model := pyrepr.Str(pyrepr.Or(aux.GetOr("model", nil), "unknown"))
		models := d.GetOr("models", []any{}).([]any)
		found := false
		for _, m := range models {
			found = found || m == model
		}
		if !found {
			d.Set("models", append(models, model))
		}
	}
	out := make([]*pyjson.Object, 0, byTask.Len())
	for _, k := range byTask.Keys {
		out = append(out, byTask.Vals[k].(*pyjson.Object))
	}
	sort.SliceStable(out, func(i, j int) bool { return tokenVolume(out[i]) > tokenVolume(out[j]) })
	return out
}
