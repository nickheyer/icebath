package dashboard

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Effective config of the home in ctx, the load_config the dashboard routes read through
func webLoadConfig(ctx context.Context) (map[string]any, error) {
	tree, err := config.LoadContext(ctx, home.Dir(ctx), config.EnvRefLookupContext(ctx))
	return map[string]any(tree), err
}

// Memory provider option discovery, the module attribute hermes tests patch
var memoryProviderOptions = discoverMemoryProviderOptions

// Memory providers offered by the memory provider select, built in only first and discovery failures leaving just it
func discoverMemoryProviderOptions(ctx context.Context) []string {
	options := []string{""}
	func() {
		defer func() {
			if r := recover(); r != nil {
				routersLog().Debug("memory provider discovery failed", "panic", fmt.Sprint(r))
			}
		}()
		options = append(options, memory.ListMemoryProviderNames(ctx, nil)...)
	}()
	return dedupeStrings(options)
}

// Strings in first seen order without repeats, the list of dict.fromkeys
func dedupeStrings(items []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Sorted IANA timezone identifiers, UTC alone when none are installed
func timezoneOptions() []string {
	zones := pydatetime.AvailableTimezones()
	if len(zones) == 0 {
		return []string{"UTC"}
	}
	out := make([]string, 0, len(zones))
	for z := range zones {
		out = append(out, z)
	}
	sort.Strings(out)
	return out
}

// Select field schema with its options and any extra keys after them
func selectField(description string, options []string, extra ...any) *pyjson.Object {
	o := pyjson.ObjectOf("type", "select", "description", description, "options", stringsAny(options))
	o.Update(pyjson.ObjectOf(extra...))
	return o
}

// Strings as a JSON list value
func stringsAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Manual schema overrides for fields that need select options or custom types
func schemaOverrides(ctx context.Context) *pyjson.Object {
	sel := func(description string, options ...string) *pyjson.Object { return selectField(description, options) }
	plain := func(pairs ...any) *pyjson.Object { return pyjson.ObjectOf(pairs...) }
	return pyjson.ObjectOf(
		"timezone", selectField("IANA timezone (e.g. America/New_York). Blank uses the system timezone.",
			timezoneOptions(), "searchable", true, "clearable", true),
		"memory.provider", selectField("Memory provider plugin", memoryProviderOptions(ctx)),
		"model", plain("type", "string", "description", "Default model (e.g. anthropic/claude-sonnet-4.6)", "category", "general"),
		"model_context_length", plain("type", "number",
			"description", "Context window override (0 = auto-detect from model metadata)", "category", "general"),
		"terminal.backend", sel("Terminal execution backend", "local", "docker", "ssh", "modal", "daytona", "vercel_sandbox"),
		"terminal.vercel_image", plain("type", "string",
			"description", "Vercel Sandbox image: a Vercel managed image (vercel/sandbox/universal:latest) or a VCR repository[:tag]"),
		"terminal.vercel_runtime", selectField(
			"Legacy Vercel Sandbox runtime (deprecated by Vercel; a pinned runtime overrides the image; clear to use the image)",
			[]string{"node24", "node22", "python3.13"}, "clearable", true),
		"terminal.modal_mode", sel("Modal sandbox mode", "sandbox", "function"),
		"proxy.enabled", plain("type", "boolean", "description",
			"Docker-only egress credential firewall. Requires `icebath egress setup` "+
				"and `icebath egress start`; Modal/SSH/Daytona are not wired yet.",
			"category", "security"),
		"proxy.credential_source", selectField("Where iron-proxy loads real upstream secrets at start time",
			[]string{"env", "bitwarden"}, "category", "security"),
		"proxy.enforce_on_docker", plain("type", "boolean",
			"description", "Refuse Docker sandboxes when egress is enabled but not configured/running", "category", "security"),
		"auth.adopt_external_logins", plain("type", "boolean", "description",
			"Borrow and refresh the Codex CLI / Claude Code logins when Hermes has no usable login of its own. "+
				"Off: Hermes uses only its own logins (`icebath auth add <provider>`).",
			"category", "security"),
		"wake_word.provider", selectField(
			"Wake engine. Auto selects a platform-supported engine; Porcupine requires PORCUPINE_ACCESS_KEY.",
			append([]string{"auto"}, media.WakeProviderPreference...)),
		"tts.provider", sel("Text-to-speech provider",
			"edge", "elevenlabs", "openai", "xai", "minimax", "mistral", "gemini", "neutts", "kittentts", "piper"),
		"stt.provider", sel("Speech-to-text provider", "local", "groq", "openai", "mistral", "xai", "elevenlabs", "deepinfra"),
		"stt.local.model", selectField("Local faster-whisper model size", media.STTModelCatalog("local")),
		"stt.groq.model", selectField("Groq Whisper model", media.STTModelCatalog("groq")),
		"stt.openai.model", selectField("OpenAI transcription model", media.STTModelCatalog("openai")),
		"stt.openai.streaming_model", sel("OpenAI live transcription model (stt.streaming)", "gpt-live-transcribe",
			"gpt-transcribe", "gpt-4o-transcribe", "gpt-4o-mini-transcribe"),
		"stt.mistral.model", selectField("Mistral Voxtral transcription model", media.STTModelCatalog("mistral")),
		"stt.xai.model", selectField("xAI transcription model", media.STTModelCatalog("xai")),
		"stt.elevenlabs.model_id", selectField("ElevenLabs Scribe model", media.STTModelCatalog("elevenlabs")),
		"display.skin", sel("CLI visual theme", "default", "ares", "mono", "slate"),
		"dashboard.theme", sel("Web dashboard visual theme", "default", "midnight", "ember", "mono", "cyberpunk", "rose"),
		"display.resume_display", sel("How resumed sessions display history", "minimal", "full", "off"),
		"display.busy_input_mode", sel("Input behavior while agent is running", "interrupt", "queue", "steer"),
		"approvals.mode", sel("Dangerous command approval mode", "manual", "smart", "off"),
		"context.engine", sel("Context management engine", "default", "custom"),
		"human_delay.mode", sel("Simulated typing delay mode", "off", "typing", "fixed"),
		"logging.level", sel("Log level for agent.log", "DEBUG", "INFO", "WARNING", "ERROR"),
		"agent.service_tier", sel("Fast mode: fast = always, auto = first N seconds of each turn, cold = first turn only",
			"", "normal", "fast", "auto", "cold"),
		"delegation.reasoning_effort", sel("Reasoning effort for delegated subagents",
			"", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"),
		"updates.non_interactive_local_changes", sel(
			"When the chat app / gateway updates Hermes (no terminal prompt), "+
				"what to do with uncommitted local source edits. 'stash' keeps them "+
				"and re-applies them after the update; 'discard' throws them away. "+
				"Terminal updates always ask, regardless of this setting.",
			"stash", "discard"),
		"updates.refresh_cua_driver", plain("type", "boolean", "description",
			"Refresh an already-installed cua-driver during icebath update. "+
				"Disable this on non-admin macOS accounts where /Applications is "+
				"not writable."),
		"browser.headed", plain("type", "boolean", "description",
			"Run the local browser in headed mode (visible window). Also keeps the window open between turns; idle sessions are still reaped after browser.inactivity_timeout."),
		"plugins.hook_callback_timeout", plain("type", "number", "description",
			"Wall-clock cap (seconds) for timeout-bounded in-process Python "+
				"plugin hook callbacks (hot-path observers + pre_tool_call). "+
				"Timed-out pre_tool_call fails closed. 0 disables the cap; "+
				"values above 600 are clamped. Caller-thread hooks such as "+
				"subagent_stop are never moved onto a timeout worker."),
		"plugins.load_timeout_seconds", plain("type", "number", "description",
			"Deadline (seconds) for one plugin's import + register() at load. A plugin that "+
				"overruns it is skipped with the reason 'load timed out' and the rest keep loading. "+
				"0 disables the deadline; values above 600 are clamped."),
	)
}

// Small categories folded into a bigger tab so no tab holds a single field
var categoryMerge = map[string]string{
	"privacy": "security", "context": "agent", "skills": "agent", "cron": "agent", "network": "agent",
	"models_dev": "agent", "checkpoints": "agent", "approvals": "security", "human_delay": "display",
	"dashboard": "display", "code_execution": "agent", "prompt_caching": "agent", "bot_mode": "agent",
	"goals": "agent", "updates": "general", "onboarding": "agent", "telegram": "discord", "mcp": "agent",
	"computer_use": "agent", "telemetry": "security", "plugins": "agent", "doctor": "general",
	"runtime": "agent", "session": "general", "nous": "agent", "connections": "agent", "auth": "security",
	"fallback": "agent",
}

// UI field type of a config value, string for anything that is not a bool, number, list or dict
func inferType(value any) string {
	if _, ok := value.(bool); ok {
		return "boolean"
	}
	if _, ok := pyrepr.Int(value); ok {
		return "number"
	}
	switch value.(type) {
	case float64, float32:
		return "number"
	case *pyjson.Object:
		return "object"
	}
	if value == nil {
		return "string"
	}
	switch reflect.TypeOf(value).Kind() {
	case reflect.Slice, reflect.Array:
		if _, isBytes := value.([]byte); !isBytes {
			return "list"
		}
	case reflect.Map:
		return "object"
	}
	return "string"
}

// Flat dot path field schema of a default config tree, overrides applied and small categories merged
func buildSchemaFromConfig(cfg map[string]any, prefix string, overrides, schema *pyjson.Object) {
	for _, key := range config.MappingKeys(cfg) {
		value := cfg[key]
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}
		if fullKey == "_config_version" {
			continue
		}
		if sub, ok := value.(map[string]any); ok {
			buildSchemaFromConfig(sub, fullKey, overrides, schema)
			continue
		}
		category := "general"
		if prefix != "" {
			category = strings.Split(prefix, ".")[0]
		}
		description := strings.ReplaceAll(strings.ReplaceAll(fullKey, ".", " → "), "_", " ")
		entry := pyjson.ObjectOf("type", inferType(value), "description", pystr.Title(description), "category", category)
		if o, ok := overrides.Get(fullKey); ok {
			entry.Update(o.(*pyjson.Object))
		}
		current := pyrepr.Str(entry.GetOr("category", nil))
		if merged, ok := categoryMerge[current]; ok {
			entry.Set("category", merged)
		}
		schema.Set(fullKey, entry)
	}
}

// Default config schema with the virtual context length field right after model
func configSchemaWithVirtualFields(ctx context.Context) *pyjson.Object {
	overrides := schemaOverrides(ctx)
	built := pyjson.NewObject()
	buildSchemaFromConfig(config.Defaults(), "", overrides, built)
	ordered := pyjson.NewObject()
	for _, key := range built.Keys {
		ordered.Set(key, built.Vals[key])
		if key == "model" {
			v, _ := overrides.Get("model_context_length")
			ordered.Set("model_context_length", v)
		}
	}
	return ordered
}

// Config schema the dashboard serves, built once like the module level CONFIG_SCHEMA
var configSchema = sync.OnceValue(func() *pyjson.Object { return configSchemaWithVirtualFields(context.Background()) })

// Whether a value declares a command type voice provider with a non empty command
func isCommandProviderBlock(value any) bool {
	block, ok := value.(map[string]any)
	if !ok {
		return false
	}
	ptype := pystr.Lower(pystr.Strip(pyrepr.Str(pyrepr.Or(block["type"], ""))))
	if ptype != "" && ptype != "command" {
		return false
	}
	command, ok := block["command"].(string)
	return ok && pystr.Strip(command) != ""
}

// Merged tts or stt provider options, built ins first then command providers, plugin names and the active one
func customProviderOptions(ctx context.Context, kind string, builtinNames []string, cfg map[string]any) []string {
	names := slices.Clone(builtinNames)
	seen := map[string]bool{}
	for _, n := range names {
		seen[pystr.Lower(pystr.Strip(n))] = true
	}
	runtimeBuiltin := media.IsBuiltinSTTProvider
	if kind == "tts" {
		runtimeBuiltin = media.IsBuiltinTTSProvider
	}
	add := func(name any) {
		s, _ := name.(string)
		stripped := pystr.Strip(s)
		if stripped != "" && !seen[pystr.Lower(stripped)] {
			names = append(names, stripped)
			seen[pystr.Lower(stripped)] = true
		}
	}
	section, ok := cfg[kind].(map[string]any)
	if !ok {
		section = map[string]any{}
	}
	var blocks []map[string]any
	if providersMap, ok := section["providers"].(map[string]any); ok {
		blocks = append(blocks, providersMap)
	}
	rest := map[string]any{}
	var restKeys []string
	for _, k := range config.MappingKeys(section) {
		if k != "providers" {
			rest[k] = section[k]
			restKeys = append(restKeys, k)
		}
	}
	config.SetMappingKeys(rest, restKeys)
	blocks = append(blocks, rest)
	for _, block := range blocks {
		for _, name := range config.MappingKeys(block) {
			if !runtimeBuiltin(pystr.Lower(pystr.Strip(name))) && isCommandProviderBlock(block[name]) {
				add(name)
			}
		}
	}
	func() {
		defer func() { _ = recover() }()
		if kind == "tts" {
			for _, p := range media.ListTTSProviders(ctx, "") {
				add(p.Name())
			}
			return
		}
		for _, p := range media.ListTranscriptionProviders(ctx, "") {
			add(p.Name())
		}
	}()
	active, _ := config.CfgGet(cfg, kind, "provider")
	add(active)
	return names
}

// Memory provider name with the core provider aliases mapped to the empty built in choice
func normalizeMemoryProviderName(name any) string {
	p := pystr.Strip(pyrepr.Str(pyrepr.Or(name, "")))
	if memory.IsCoreMemoryProvider(p) {
		return ""
	}
	return p
}

// Discovered memory providers plus the configured one so a removed plugin stays selectable
func memoryProviderSchemaOptions(ctx context.Context, cfg map[string]any) []string {
	options := memoryProviderOptions(ctx)
	var current string
	if mem, ok := cfg["memory"].(map[string]any); ok {
		current = normalizeMemoryProviderName(mem["provider"])
	} else {
		current = normalizeMemoryProviderName(nil)
	}
	if current != "" && !slices.Contains(options, current) {
		options = append(options, current)
	}
	return options
}

// Select options of a schema entry, false when the entry has no option list
func schemaSelectOptions(schema *pyjson.Object, key string) ([]string, bool) {
	raw, ok := schema.Get(key)
	if !ok {
		return nil, false
	}
	entry, ok := raw.(*pyjson.Object)
	if !ok {
		return nil, false
	}
	list, ok := entry.GetOr("options", nil).([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, pyrepr.Str(v))
	}
	return out, true
}

// Config schema with the provider selects recomputed from the scoped config and current plugin discovery
func schemaWithDynamicProviderOptions(ctx context.Context) *pyjson.Object {
	schema := configSchema()
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		return schema
	}
	overlay := pyjson.NewObject()
	merge := func(key string, options []string) {
		current, ok := schemaSelectOptions(schema, key)
		if ok && !slices.Equal(options, current) {
			entry, _ := schema.Get(key)
			copied := entry.(*pyjson.Object).Copy()
			copied.Set("options", stringsAny(options))
			overlay.Set(key, copied)
		}
	}
	for _, kind := range []string{"tts", "stt"} {
		if existing, ok := schemaSelectOptions(schema, kind+".provider"); ok {
			merge(kind+".provider", customProviderOptions(ctx, kind, existing, cfg))
		}
	}
	merge("memory.provider", memoryProviderSchemaOptions(ctx, cfg))
	if tbOptions, ok := schemaSelectOptions(schema, "terminal.backend"); ok {
		known := map[string]bool{}
		for _, o := range tbOptions {
			known[o] = true
		}
		extra := map[string]bool{}
		for _, row := range pluginTerminalBackendRows(ctx) {
			if name := pyrepr.Str(row.GetOr("name", nil)); !known[name] {
				extra[name] = true
			}
		}
		pluginNames := make([]string, 0, len(extra))
		for n := range extra {
			pluginNames = append(pluginNames, n)
		}
		sort.Strings(pluginNames)
		if len(pluginNames) > 0 {
			merge("terminal.backend", append(slices.Clone(tbOptions), pluginNames...))
		}
	}
	if overlay.Len() == 0 {
		return schema
	}
	out := schema.Copy()
	out.Update(overlay)
	return out
}

// Shallow copy of a config mapping keeping its key order
func copyConfigMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	config.SetMappingKeys(out, config.MappingKeys(m))
	return out
}

// Sets a key the way a Python dict assignment does, a new key moving to the end of the order
func setConfigKey(m map[string]any, key string, value any) {
	if _, held := m[key]; held {
		m[key] = value
		return
	}
	keys := config.MappingKeys(m)
	m[key] = value
	config.SetMappingKeys(m, append(keys, key))
}

// Whether a value passes isinstance int, bools included
func isPyInt(v any) bool {
	_, ok := pyint.Instance(v)
	return ok
}

// Config with a dict model flattened to its default name and its context length surfaced as a top level field
func normalizeConfigForWeb(cfg map[string]any) map[string]any {
	out := copyConfigMap(cfg)
	if modelVal, ok := out["model"].(map[string]any); ok {
		ctxLen, held := modelVal["context_length"]
		if !held {
			ctxLen = int64(0)
		}
		name, held := modelVal["default"]
		if !held {
			name, held = modelVal["name"]
			if !held {
				name = ""
			}
		}
		setConfigKey(out, "model", name)
		if !isPyInt(ctxLen) {
			ctxLen = int64(0)
		}
		setConfigKey(out, "model_context_length", ctxLen)
	} else {
		setConfigKey(out, "model_context_length", int64(0))
	}
	return out
}

// Restart required message when this process runs code older than the checkout, empty when current
func dashboardCodeSkewGuard() string {
	skew := detectCodeSkew()
	if skew == nil {
		return ""
	}
	return fmt.Sprintf("This process is running code from %s but the checkout on disk is now %s. "+
		"The model picker would risk a stale-module crash — %s", skew[0], skew[1], dashboardSkewRestartHint())
}

// Restart advice matching whether Desktop owns this process or it serves the browser dashboard
func dashboardSkewRestartHint() string {
	if os.Getenv("ICEBATH_SERVE_HEADLESS") == "1" {
		return "restart the Desktop-owned backend to load the new code " +
			"(use Restart backend in Hermes Desktop, or quit and reopen the app)"
	}
	return "restart this Hermes process to load the new code " +
		"(icebath dashboard --port <port>, or the equivalent service restart for this install)"
}

// Reverses the web normalization before a save, rebuilding the model dict and rerouting a changed model
func denormalizeConfigFromWeb(ctx context.Context, cfg map[string]any) (map[string]any, error) {
	out := copyConfigMap(cfg)
	delete(out, "_model_meta")
	rawCtx, ctxSent := out["model_context_length"]
	delete(out, "model_context_length")
	var ctxOverride any = int64(0)
	if ctxSent {
		ctxOverride = rawCtx
	}
	ctxValue, isInt := pyint.Instance(ctxOverride)
	if !isInt {
		n, err := pyint.Of(ctxOverride)
		if err != nil {
			if pe, ok := err.(*pyint.Error); !ok || (pe.Kind != "TypeError" && pe.Kind != "ValueError") {
				return nil, err
			}
			n = big.NewInt(0)
		}
		ctxValue, ctxOverride = n, pyint.Native(n)
	}
	modelStr, isStr := out["model"].(string)
	hasModel := isStr && modelStr != ""
	if !(hasModel || ctxSent) {
		return out, nil
	}
	diskCfg, err := webLoadConfig(ctx)
	if err != nil {
		return out, nil
	}
	positive := ctxValue.Sign() > 0
	if diskModel, ok := diskCfg["model"].(map[string]any); ok {
		if hasModel {
			prevDefault := pystr.Strip(pyrepr.Str(pyrepr.Or(diskModel["default"], "")))
			prevProvider := pystr.Strip(pyrepr.Str(pyrepr.Or(diskModel["provider"], "")))
			if modelStr != prevDefault && prevProvider != "" {
				newProvider, resolved := provider.InferProviderOnModelChange(ctx, modelStr, prevProvider)
				if newProvider != "" && pystr.Lower(pystr.Strip(newProvider)) != pystr.Lower(prevProvider) {
					normProvider, normModel := provider.NormalizeMainModelAssignment(ctx, newProvider, resolved)
					result, err := provider.ValidatedMainModelSelection(ctx, diskCfg, normProvider, normModel, "", "", nil)
					if err != nil {
						return nil, asHTTPError(err)
					}
					if diskModel, err = provider.ApplyMainModelAssignment(ctx, diskModel, result, ""); err != nil {
						return nil, asHTTPError(err)
					}
					modelStr = result.NewModel
				}
			}
			setConfigKey(diskModel, "default", modelStr)
		}
		if ctxSent {
			if positive {
				setConfigKey(diskModel, "context_length", ctxOverride)
			} else {
				delete(diskModel, "context_length")
			}
		}
		setConfigKey(out, "model", diskModel)
	} else if ctxSent && positive {
		def := ""
		if hasModel {
			def = modelStr
		} else if s, ok := diskCfg["model"].(string); ok && s != "" {
			def = s
		}
		m := map[string]any{"default": def, "context_length": ctxOverride}
		config.SetMappingKeys(m, []string{"default", "context_length"})
		setConfigKey(out, "model", m)
	}
	return out, nil
}

// Ordered JSON value of a config tree, mappings keeping their recorded key order
func configJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		o := pyjson.NewObject()
		for _, k := range config.MappingKeys(t) {
			o.Set(k, configJSON(t[k]))
		}
		return o
	case *pyjson.Object:
		if t == nil {
			return nil
		}
		o := pyjson.NewObject()
		for _, k := range t.Keys {
			o.Set(k, configJSON(t.Vals[k]))
		}
		return o
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = configJSON(x)
		}
		return out
	case []string:
		return stringsAny(t)
	case []map[string]any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = configJSON(x)
		}
		return out
	}
	return v
}

// Config tree of a decoded JSON value, objects becoming mappings that remember their key order
func configMapOf(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		if t == nil {
			return nil
		}
		m := make(map[string]any, t.Len())
		for _, k := range t.Keys {
			m[k] = configMapOf(t.Vals[k])
		}
		config.SetMappingKeys(m, t.Keys)
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = configMapOf(x)
		}
		return out
	}
	return v
}
