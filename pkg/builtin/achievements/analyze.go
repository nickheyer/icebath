package achievements

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	errorRE        = ci(`\b(error|failed|failure|traceback|exception|permission denied|not found|eaddrinuse|already in use|timed out|blocked)\b`)
	portRE         = ci(`\b(port\s+)?(3000|5173|8000|8080|9119)\b.*\b(in use|already|taken|eaddrinuse)\b|\beaddrinuse\b`)
	installRE      = ci(`\b(npm|pnpm|yarn|pip|uv)\b.*\b(install|add)\b`)
	successRE      = ci(`\b(success|passed|built|compiled|done|exit_code[\"']?\s*[:=]\s*0|verified|ok)\b`)
	fileRE         = mustRE(`(?:/home/|~/?|\./|/mnt/)[\w./-]+\.(?:py|js|ts|tsx|jsx|css|html|md|json|yaml|yml|svg|sql|sh)`)
	skillWordRE    = mustRE(`\bskill`)
	restartRE      = ci(`\brestart|reload|kill|start\b`)
	tinyPatchRE    = ci(`one character|single character|typo`)
	backgroundRE   = ci(`background\s*=\s*true`)
	tracebackRE    = ci(`traceback|exception`)
	logReadRE      = ci(`gateway\.log|errors\.log|agent\.log|/api/logs|\blogs\b`)
	permissionRE   = ci(`permission denied|eacces|operation not permitted`)
	envVarRE       = ci(`missing .*env|api key|environment variable|not configured|unauthorized|auth`)
	yamlRE         = ci(`yaml|yml|colon|parse error`)
	dockerRE       = ci(`docker.*(name|container).*already|container name conflict|Conflict\. The container`)
	frontendRE     = ci(`\.(css|svg|tsx|jsx)|frontend|tailwind|react`)
	cssRE          = ci(`\.css|tailwind|style|className|visual`)
	gitRE          = ci(`\bgit\s+(commit|push|merge|rebase|status|diff)`)
	contextRE      = ci(`compress|context window|token|cache`)
	gatewayRE      = ci(`gateway|discord|telegram|slack|api_server`)
	pluginRE       = ci(`plugin|dashboard-plugins|__HERMES_PLUGIN|manifest\.json`)
	rollbackRE     = ci(`rollback|checkpoint`)
	docsRE         = ci(`docs|documentation|docusaurus|README`)
	modelRE        = ci(`model|provider|openrouter|codex|gemini|claude|anthropic|openai|mistral|qwen|deepseek|llama|ollama|vllm|gguf`)
	openrouterRE   = ci(`openrouter`)
	codexRE        = ci(`codex`)
	claudeRE       = ci(`claude|anthropic`)
	geminiRE       = ci(`gemini|google ai|google model`)
	localModelRE   = ci(`ollama|llama\.cpp|gguf|vllm|local model|open[- ]weight|open weights`)
	toolsetRE      = ci(`toolset|enabled_toolsets|browser tool|terminal tool|file tool|web tool`)
	configRE       = ci(`config\.ya?ml|\b[a-z0-9_-]+config\.(?:js|ts|json|ya?ml)|\.env(?:\b|\.)|manifest\.json|settings\.json|pyproject\.toml|package\.json`)
	gitHistoryRE   = ci(`\bgit\s+(rebase|merge|fetch|pull|push|tag|checkout)|merge conflict|conflict\s*\(|rebase --continue`)
	testRE         = ci(`pytest|unittest|vitest|playwright|npm test|pnpm test|node --check|py_compile|tests? passed|\bOK\b`)
	screenshotRE   = ci(`screenshot|playwright|vision_analyze|browser_vision|\.png|image data`)
	releaseRE      = ci(`\bgit\s+tag|release|version bump|changelog|publish|pushed? tag`)
	cacheRE        = ci(`cache hit|prompt caching|cache_read`)
	providerMarker = []string{"openai", "anthropic", "google", "gemini", "mistral", "meta", "qwen", "deepseek", "xai", "nous", "ollama", "groq", "openrouter", "codex"}
	localMarker    = []string{"ollama", "llama.cpp", "localhost", "127.0.0.1", "local/", "local:", "gguf", "vllm-local"}
)

// Tool name of a tool call entry from its name or function name, nil when it carries neither
func toolNameFromCall(call any) (any, error) {
	c, ok := call.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	if n := get(c, "name"); truthy(n) {
		return n, nil
	}
	fn := get(c, "function")
	if !truthy(fn) {
		return nil, nil
	}
	f, ok := fn.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(fn))
	}
	return get(f, "name"), nil
}

// Message content as text, JSON for structured content and str when JSON fails
func content(msg *pyjson.Object) string {
	c := get(msg, "content")
	switch t := c.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	if s, err := pyjson.Dumps(c); err == nil {
		return s
	}
	return pyrepr.Str(c)
}

// Calls whose lowercased tool name contains any needle
func countTool(names []string, needles ...string) int64 {
	var n int64
	for _, name := range names {
		low := pystr.Lower(name)
		for _, nd := range needles {
			if strings.Contains(low, nd) {
				n++
				break
			}
		}
	}
	return n
}

// Provider of a model name from its slash prefix, a known marker or its leading word, nil when empty
func modelProvider(modelName string) any {
	name := pystr.Lower(pystr.Strip(modelName))
	if name == "" || name == "none" {
		return nil
	}
	if i := strings.Index(name, "/"); i >= 0 {
		return name[:i]
	}
	for _, p := range providerMarker {
		if strings.Contains(name, p) {
			if p == "gemini" {
				return "google"
			}
			return p
		}
	}
	name = strings.SplitN(name, ":", 2)[0]
	return strings.SplitN(name, "-", 2)[0]
}

// Whether a model name names a local or open weight runtime
func isLocalModelName(modelName string) bool {
	name := pystr.Lower(pystr.Strip(modelName))
	if name == "" || name == "none" {
		return false
	}
	for _, m := range localMarker {
		if strings.Contains(name, m) {
			return true
		}
	}
	return false
}

// Per session achievement stats from a session message list
func analyzeMessages(sessionID, title string, messages []*pyjson.Object) (*pyjson.Object, error) {
	toolNames := stringSet{}
	var toolSequence []string
	filesTouched := stringSet{}
	parts := make([]string, 0, len(messages))
	var errorCount int64
	addName := func(name any, sequence bool) error {
		s, ok := name.(string)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'lower'", pyrepr.TypeName(name))
		}
		toolNames[s] = struct{}{}
		if sequence {
			toolSequence = append(toolSequence, s)
		}
		return nil
	}
	for _, msg := range messages {
		text := content(msg)
		parts = append(parts, text)
		if tn := get(msg, "tool_name"); truthy(tn) {
			if err := addName(pyrepr.Str(tn), get(msg, "role") != "tool"); err != nil {
				return nil, err
			}
		}
		calls := get(msg, "tool_calls")
		if !truthy(calls) {
			calls = []any{}
		}
		items, err := iterate(calls)
		if err != nil {
			return nil, err
		}
		for _, call := range items {
			name, err := toolNameFromCall(call)
			if err != nil {
				return nil, err
			}
			if truthy(name) {
				if err := addName(name, true); err != nil {
					return nil, err
				}
			}
		}
		if search(errorRE, text) {
			errorCount++
		}
		blob := text
		if truthy(get(msg, "tool_calls")) {
			dumped, err := pyjson.Dumps(get(msg, "tool_calls"), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
			if err != nil {
				return nil, err
			}
			blob += " " + dumped
		}
		for _, f := range findAll(fileRE, blob) {
			filesTouched[f] = struct{}{}
		}
	}
	full := strings.Join(parts, "\n")
	lower := pystr.Lower(full)
	count := func(re *regexp2.Regexp) int64 { return int64(len(findAll(re, full))) }
	webCalls := countTool(toolSequence, "web_search", "web_extract")
	browserCalls := countTool(toolSequence, "browser")
	portConflict := search(portRE, full)
	b2i := func(b bool) int64 {
		if b {
			return 1
		}
		return 0
	}
	anyErr := search(errorRE, full)
	yamlEvents := int64(0)
	if anyErr {
		yamlEvents = count(yamlRE)
	}
	if title == "" {
		title = "Untitled session"
	}
	return obj(
		"session_id", sessionID,
		"title", title,
		"message_count", int64(len(messages)),
		"tool_call_count", int64(len(toolSequence)),
		"tool_names", toolNames,
		"distinct_tool_count", int64(len(toolNames)),
		"error_count", errorCount,
		"terminal_calls", countTool(toolSequence, "terminal"),
		"web_calls", webCalls,
		"web_extract_calls", countTool(toolSequence, "web_extract"),
		"browser_calls", browserCalls,
		"web_browser_calls", webCalls+browserCalls,
		"patch_calls", countTool(toolSequence, "patch"),
		"file_reads_searches", countTool(toolSequence, "read_file", "search_files"),
		"file_tool_calls", countTool(toolSequence, "read_file", "write_file", "patch", "search_files"),
		"files_touched_count", int64(len(filesTouched)),
		"delegate_calls", countTool(toolSequence, "delegate_task"),
		"process_calls", countTool(toolSequence, "process")+count(backgroundRE),
		"cron_calls", countTool(toolSequence, "cronjob"),
		"image_vision_calls", countTool(toolSequence, "image", "vision"),
		"tts_calls", countTool(toolSequence, "tts", "text_to_speech"),
		"skill_events", countTool(toolSequence, "skill")+int64(len(findAll(skillWordRE, lower))),
		"skill_manage_events", countTool(toolSequence, "skill_manage"),
		"memory_events", countTool(toolSequence, "memory", "mnemosyne"),
		"memory_write_events", countTool(toolSequence, "mnemosyne_remember", "memory"),
		"port_conflict", portConflict,
		"port_conflict_events", b2i(portConflict),
		"traceback_events", count(tracebackRE),
		"log_read_events", count(logReadRE),
		"permission_denied_events", count(permissionRE),
		"install_error_events", b2i(search(installRE, full) && anyErr),
		"install_success_events", b2i(search(installRE, full) && search(successRE, full)),
		"restart_after_error_events", b2i(errorCount != 0 && search(restartRE, full)),
		"env_var_error_events", count(envVarRE),
		"yaml_error_events", yamlEvents,
		"docker_conflict_events", count(dockerRE),
		"frontend_activity_events", count(frontendRE),
		"css_activity_events", count(cssRE),
		"git_events", count(gitRE),
		"tiny_patch_after_errors_events", b2i(errorCount >= 5 && search(tinyPatchRE, full)),
		"context_events", count(contextRE),
		"gateway_events", count(gatewayRE),
		"plugin_events", count(pluginRE),
		"rollback_events", count(rollbackRE),
		"docs_activity_events", count(docsRE),
		"model_events", count(modelRE),
		"openrouter_events", count(openrouterRE),
		"codex_events", count(codexRE),
		"claude_events", count(claudeRE),
		"gemini_events", count(geminiRE),
		"local_model_events", count(localModelRE),
		"toolset_events", count(toolsetRE),
		"config_events", count(configRE),
		"git_history_events", count(gitHistoryRE),
		"test_events", count(testRE),
		"screenshot_events", count(screenshotRE),
		"release_events", count(releaseRE),
		"cache_events", count(cacheRE),
		"model_names", stringSet{},
	), nil
}

// Uniform evaluation result in its wire key order
func result(unlocked, discovered bool, state string, tier any, progress int64, nextTier any, nextThreshold, progressPct int64) *pyjson.Object {
	return obj("unlocked", unlocked, "discovered", discovered, "state", state, "tier", tier, "progress", progress,
		"next_tier", nextTier, "next_threshold", nextThreshold, "progress_pct", progressPct)
}

// State and discovered flag where secret badges stay hidden until the first matching signal
func badgeState(def *pyjson.Object, unlocked, anyProgress bool) (string, bool) {
	secret := truthy(get(def, "secret"))
	state := "discovered"
	if unlocked {
		state = "unlocked"
	} else if secret && !anyProgress {
		state = "secret"
	}
	return state, anyProgress || !secret
}

type tier struct {
	name      any
	threshold int64
}

// Tiers of a definition sorted by threshold
func sortedTiers(def *pyjson.Object) ([]tier, error) {
	items, err := iterate(getOr(def, "tiers", []any{}))
	if err != nil {
		return nil, err
	}
	out := make([]tier, 0, len(items))
	for _, it := range items {
		o, ok := it.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(it))
		}
		th, ok := o.Get("threshold")
		if !ok {
			return nil, fmt.Errorf("KeyError: 'threshold'")
		}
		n, err := intOf(th)
		if err != nil {
			return nil, err
		}
		out = append(out, tier{name: get(o, "name"), threshold: n})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].threshold < out[j].threshold })
	return out, nil
}

// Tiered evaluation reaching the highest tier whose threshold the metric meets
func evaluateTiered(def, aggregate *pyjson.Object) (*pyjson.Object, error) {
	metric, _ := get(def, "threshold_metric").(string)
	progress, err := intOr0(getOr(aggregate, metric, int64(0)))
	if err != nil {
		return nil, err
	}
	tiers, err := sortedTiers(def)
	if err != nil {
		return nil, err
	}
	var achieved, next []tier
	for _, t := range tiers {
		if progress >= t.threshold {
			achieved = append(achieved, t)
		} else {
			next = append(next, t)
		}
	}
	nextThreshold := int64(1)
	if len(next) > 0 {
		nextThreshold = next[0].threshold
	} else if len(tiers) > 0 {
		nextThreshold = tiers[len(tiers)-1].threshold
	}
	current := int64(0)
	if len(achieved) > 0 {
		current = achieved[len(achieved)-1].threshold
	}
	denom := max(int64(1), nextThreshold-current)
	pct := int64(100)
	if len(next) > 0 || len(achieved) == 0 {
		pct = max(0, min(99, int64(math.Floor(float64(progress-current)/float64(denom)*100))))
	}
	state, discovered := badgeState(def, len(achieved) > 0, progress > 0)
	var tierName, nextName any
	if len(achieved) > 0 {
		tierName = achieved[len(achieved)-1].name
	}
	if len(next) > 0 {
		nextName = next[0].name
	}
	return result(len(achieved) > 0, discovered, state, tierName, progress, nextName, nextThreshold, pct), nil
}

// Multi condition evaluation unlocking only when every requirement is met
func evaluateRequirements(def, aggregate *pyjson.Object) (*pyjson.Object, error) {
	reqs, err := iterate(getOr(def, "requirements", []any{}))
	if err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		state, discovered := badgeState(def, false, false)
		return result(false, discovered, state, nil, 0, nil, 1, 0), nil
	}
	sum := 0.0
	anyProgress, complete := false, true
	for _, r := range reqs {
		ro, ok := r.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(r))
		}
		metric, _ := get(ro, "metric").(string)
		value, err := intOr0(getOr(aggregate, metric, int64(0)))
		if err != nil {
			return nil, err
		}
		threshold, err := intOf(getOr(ro, "gte", int64(1)))
		if err != nil {
			return nil, err
		}
		anyProgress = anyProgress || value > 0
		complete = complete && value >= threshold
		sum += math.Min(1.0, float64(value)/float64(max(1, threshold)))
	}
	pct := int64(math.Floor(sum / float64(len(reqs)) * 100))
	state, discovered := badgeState(def, complete, anyProgress)
	shown := min(99, pct)
	if complete {
		shown = 100
	}
	return result(complete, discovered, state, nil, pct, nil, 100, shown), nil
}

// Tiered or multi condition evaluation by the definition shape
func evaluateDefinition(def, aggregate *pyjson.Object) (*pyjson.Object, error) {
	if _, ok := def.Get("threshold_metric"); ok {
		return evaluateTiered(def, aggregate)
	}
	return evaluateRequirements(def, aggregate)
}

// Human label of a metric, its words when no label is known
func metricLabel(metric string) string {
	if l, ok := metricLabels[metric]; ok {
		return l
	}
	return strings.ReplaceAll(metric, "_", " ")
}

// Requirement sentence of a definition, hidden for an undiscovered secret
func criteriaFor(def *pyjson.Object) (string, error) {
	if truthy(get(def, "secret")) && get(def, "state") == "secret" {
		return "Secret: exact requirement hidden until Hermes sees the first matching signal. Keep using Hermes across debugging, tools, memory, skills, plugins, and model workflows to reveal it.", nil
	}
	if _, ok := def.Get("threshold_metric"); ok {
		tiers, err := sortedTiers(def)
		if err != nil {
			return "", err
		}
		if len(tiers) == 0 {
			return "Requirement: use Hermes in the matching workflow.", nil
		}
		ladder := make([]string, len(tiers))
		for i, t := range tiers {
			ladder[i] = fmt.Sprintf("%s %d", pyrepr.Str(t.name), t.threshold)
		}
		metric, _ := get(def, "threshold_metric").(string)
		return fmt.Sprintf("Requirement: %s. Tier ladder: %s.", metricLabel(metric), strings.Join(ladder, ", ")), nil
	}
	reqs := get(def, "requirements")
	if truthy(reqs) {
		items, err := iterate(reqs)
		if err != nil {
			return "", err
		}
		parts := make([]string, len(items))
		for i, r := range items {
			ro, _ := r.(*pyjson.Object)
			gte, err := intOf(getOr(ro, "gte", int64(1)))
			if err != nil {
				return "", err
			}
			metric, _ := get(ro, "metric").(string)
			parts[i] = fmt.Sprintf("%s \u2265 %d", metricLabel(metric), gte)
		}
		return "Requirement: " + strings.Join(parts, "; ") + ".", nil
	}
	return "Requirement: complete the matching Hermes behavior.", nil
}

// Card payload with criteria, masking name, description and icon of an undiscovered secret
func displayAchievement(item *pyjson.Object) (*pyjson.Object, error) {
	clean := item.Copy()
	criteria, err := criteriaFor(clean)
	if err != nil {
		return nil, err
	}
	if get(clean, "state") == "secret" {
		clean.Set("name", "???")
		clean.Set("description", "Secret achievement: hidden until Hermes detects the first relevant behavior in your session history.")
		clean.Set("criteria", criteria)
		clean.Set("icon", "secret")
		return clean, nil
	}
	clean.Set("criteria", criteria)
	return clean, nil
}
