package config

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Stream that receives managed mode refusals and managed key notices, standard error outside tests
var FacadeNoticeOut io.Writer = os.Stderr

const securityComment = `
# ── Security ──────────────────────────────────────────────────────────
# Secret redaction is ON by default — strings that look like API keys,
# tokens, and passwords are masked in tool output, logs, and chat
# responses before the model or user ever sees them. Set redact_secrets
# to false to disable (e.g. when developing the redactor itself).
#
# security:
#   redact_secrets: true
`

const fallbackComment = `
# ── Fallback Model ────────────────────────────────────────────────────
# Automatic provider failover when primary is unavailable.
# Uncomment and configure to enable. Triggers on rate limits (429),
# overload (529), service errors (503), or connection failures.
#
# Supported providers:
#   openrouter   (OPENROUTER_API_KEY)  — routes to any model
#   openai-codex (OAuth — icebath auth) — OpenAI Codex
#   nous         (OAuth — icebath auth) — Nous Portal
#   zai          (ZAI_API_KEY)         — Z.AI / GLM
#   kimi-coding  (KIMI_API_KEY)        — Kimi / Moonshot
#   kimi-coding-cn (KIMI_CN_API_KEY)   — Kimi / Moonshot (China)
#   minimax      (MINIMAX_API_KEY)     — MiniMax
#   minimax-cn   (MINIMAX_CN_API_KEY)  — MiniMax (China)
#   bedrock      (AWS IAM / boto3)     — AWS Bedrock (Converse API)
#
# For custom OpenAI-compatible endpoints, add base_url and key_env.
#
# fallback_model:
#   provider: openrouter
#   model: anthropic/claude-sonnet-4
`

// How a save treats defaults and the file already on disk
type SaveOptions struct {
	KeepDefaults  bool
	PreserveKeys  [][]string
	MergeExisting bool
}

type appliedWriteKey struct{}

// Context whose saves are Icebath's own writes, never reported as a user turning a feature off
func AppliedWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, appliedWriteKey{}, true)
}

// Copy of a document without the dotted leaves listed, plus the leaves it removed
func stripDottedKeys(cfg map[string]any, dotted []string) (map[string]any, []string) {
	var stripped []string
	for _, key := range dotted {
		parts := strings.Split(key, ".")
		parents, leaf := parts[:len(parts)-1], parts[len(parts)-1]
		var node any = cfg
		if len(parents) > 0 {
			node, _ = CfgGet(cfg, parents...)
		}
		if m, ok := node.(map[string]any); ok {
			if _, has := m[leaf]; has {
				delete(m, leaf)
				stripped = append(stripped, key)
			}
		}
	}
	return cfg, stripped
}

// Document without the leaves the managed layer pins, the dropped keys reported on the notice stream
func stripManagedKeysForSave(cfg map[string]any) map[string]any {
	keys := ManagedConfigKeys()
	if len(keys) == 0 {
		return cfg
	}
	out, stripped := stripDottedKeys(Clone(cfg).(map[string]any), keys)
	if len(stripped) > 0 {
		sort.Strings(stripped)
		fmt.Fprintf(FacadeNoticeOut, "Note: %d managed setting(s) were not saved (managed by your administrator): %s\n",
			len(stripped), strings.Join(stripped, ", "))
	}
	return out
}

// Commented example blocks for features left off, empty when every block is configured
func commentedSectionsForSave(normalized map[string]any) string {
	var parts []string
	security, _ := normalized["security"].(map[string]any)
	if security["redact_secrets"] == nil {
		parts = append(parts, securityComment)
	}
	fb, has := normalized["fallback_model"]
	if !has {
		fb = map[string]any{}
	}
	entries, isList := fb.([]any)
	if !isList {
		entries = []any{fb}
	}
	configured := false
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && PyTruthy(m["provider"]) && PyTruthy(m["model"]) {
			configured = true
		}
	}
	if !configured {
		parts = append(parts, fallbackComment)
	}
	return strings.Join(parts, "")
}

// Items of a list keyed by their unique string name, nil when any item lacks one or repeats it
func itemsByUniqueName(items any) map[string]map[string]any {
	list, ok := items.([]any)
	if !ok {
		return nil
	}
	indexed := map[string]map[string]any{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		name, ok := m["name"].(string)
		if !ok {
			return nil
		}
		if _, dup := indexed[name]; dup {
			return nil
		}
		indexed[name] = m
	}
	return indexed
}

// Value with the raw env templates restored wherever the expanded value is otherwise unchanged
func preserveEnvRefTemplates(current, raw, loaded any, lookup Lookup) any {
	if cs, ok := current.(string); ok {
		if rs, ok := raw.(string); ok && envRefRe.MatchString(rs) {
			if ls, isStr := loaded.(string); cs == rs || (isStr && cs == ls) || ExpandString(rs, lookup) == cs {
				return rs
			}
			return cs
		}
	}
	if cm, ok := current.(map[string]any); ok {
		if rm, ok := raw.(map[string]any); ok {
			lm, _ := loaded.(map[string]any)
			out := make(map[string]any, len(cm))
			for k, v := range cm {
				var lv any
				if lm != nil {
					lv = lm[k]
				}
				out[k] = preserveEnvRefTemplates(v, rm[k], lv, lookup)
			}
			SetMappingKeys(out, MappingKeys(cm))
			return out
		}
	}
	if cl, ok := current.([]any); ok {
		if rl, ok := raw.([]any); ok {
			currentByName, rawByName, loadedByName := itemsByUniqueName(cl), itemsByUniqueName(rl), itemsByUniqueName(loaded)
			out := make([]any, len(cl))
			if currentByName != nil && rawByName != nil {
				for i, item := range cl {
					name := item.(map[string]any)["name"].(string)
					var lv any
					if loadedByName != nil {
						if l, ok := loadedByName[name]; ok {
							lv = l
						}
					}
					var rv any
					if r, ok := rawByName[name]; ok {
						rv = r
					}
					out[i] = preserveEnvRefTemplates(item, rv, lv, lookup)
				}
				return out
			}
			ll, _ := loaded.([]any)
			for i, item := range cl {
				var rv, lv any
				if i < len(rl) {
					rv = rl[i]
				}
				if i < len(ll) {
					lv = ll[i]
				}
				out[i] = preserveEnvRefTemplates(item, rv, lv, lookup)
			}
			return out
		}
	}
	return current
}

// Process environment lookup the facade expands config references against
func envRefLookup(name string) (string, bool) { return os.LookupEnv(name) }

// Mapping paths a proposed document would delete by omitting them from the existing one
func omittedConfigPaths(existing, proposed map[string]any, prefix []string) []string {
	var omitted []string
	for _, key := range MappingKeys(existing) {
		old := existing[key]
		path := append(append([]string(nil), prefix...), key)
		next, has := proposed[key]
		if !has {
			omitted = append(omitted, strings.Join(path, "."))
			continue
		}
		if om, ok := old.(map[string]any); ok {
			if nm, ok := next.(map[string]any); ok {
				omitted = append(omitted, omittedConfigPaths(om, nm, path)...)
			} else if len(om) > 0 {
				omitted = append(omitted, strings.Join(path, "."))
			}
		}
	}
	return omitted
}

// Comment preserving config write, refusing a failed read and, unless omissions are allowed, any dropped path
func writeConfigState(ctx context.Context, h, configPath string, data map[string]any, allowOmissions bool, extraContentOnCreate string) error {
	if err := RefuseFailedRead(h, configPath, data); err != nil {
		return err
	}
	if !allowOmissions {
		existing, err := KeepingReadBase(ctx, func() (map[string]any, error) {
			return RequireReadableConfigBeforeWrite(ctx, configPath)
		})
		if err != nil {
			return err
		}
		if omitted := omittedConfigPaths(existing, data, nil); len(omitted) > 0 {
			shown := strings.Join(omitted[:min(12, len(omitted))], ", ")
			if len(omitted) > 12 {
				shown += fmt.Sprintf(", +%d more", len(omitted)-12)
			}
			return RefuseOverwrite(configPath, "would lose settings omitted by this write", fmt.Errorf("omitted config paths: %s", shown),
				"Pass the complete current config, or use atomic_config_replace() only when deletion by omission is deliberate.")
		}
	}
	return WriteConfigDocument(ctx, configPath, data, extraContentOnCreate)
}

// Persists a config without letting an incomplete mapping delete existing settings
func AtomicConfigWrite(ctx context.Context, h, configPath string, data map[string]any, extraContentOnCreate string) error {
	return writeConfigState(ctx, h, configPath, data, false, extraContentOnCreate)
}

// Persists the complete desired config state, omitted mapping keys deliberately deleted
func AtomicConfigReplace(ctx context.Context, h, configPath string, data map[string]any, extraContentOnCreate string) error {
	return writeConfigState(ctx, h, configPath, data, true, extraContentOnCreate)
}

// Saves a config to the home config.yaml, defaults the user never set stripped and managed leaves dropped
func SaveConfig(ctx context.Context, h string, cfg map[string]any, opts SaveOptions) error {
	ctx, unlock := LockConfig(ctx)
	raw, current, saved, err := func() (map[string]any, map[string]any, bool, error) {
		defer unlock()
		return saveConfigLocked(ctx, h, cfg, opts)
	}()
	if err != nil || !saved {
		return err
	}
	facadeMu.RLock()
	observe := savedObserver
	facadeMu.RUnlock()
	if observe != nil && ctx.Value(appliedWriteKey{}) == nil {
		observe(h, raw, current)
	}
	return nil
}

func saveConfigLocked(ctx context.Context, h string, cfg map[string]any, opts SaveOptions) (map[string]any, map[string]any, bool, error) {
	if ManagedSystem(ctx, h) != "" {
		fmt.Fprintln(FacadeNoticeOut, FormatManagedMessage(ctx, h, "save configuration"))
		return nil, nil, false, nil
	}
	path := home.ConfigPath(h)
	if err := RefuseFailedRead(h, path, cfg); err != nil {
		return nil, nil, false, err
	}
	cfg = stripManagedKeysForSave(cfg)
	if err := EnsureIcebathHome(ctx, h); err != nil {
		return nil, nil, false, err
	}
	raw, err := KeepingReadBase(ctx, func() (map[string]any, error) { return RequireReadableConfigBeforeWrite(ctx, path) })
	if err != nil {
		return nil, nil, false, err
	}
	var loadErr error
	cfg = RebaseForSave(cfg, raw, opts.MergeExisting, func() map[string]any {
		t, lerr := LoadContext(ctx, h, EnvRefLookupContext(ctx))
		if lerr != nil && loadErr == nil {
			loadErr = lerr
		}
		return t
	})
	if loadErr != nil {
		return nil, nil, false, loadErr
	}
	current, err := Canonicalize(cfg)
	if err != nil {
		return nil, nil, false, err
	}
	normalized := current
	if len(raw) > 0 {
		rawCanon, cerr := Canonicalize(raw)
		if cerr != nil {
			return nil, nil, false, cerr
		}
		loadCacheMu.Lock()
		loaded := lastExpanded[path]
		loadCacheMu.Unlock()
		var loadedAny any
		if loaded != nil {
			loadedAny = loaded
		}
		normalized = preserveEnvRefTemplates(normalized, rawCanon, loadedAny, EnvRefLookupContext(ctx)).(map[string]any)
	}
	if !opts.KeepDefaults {
		preserve := ExplicitPaths(raw)
		for _, parts := range opts.PreserveKeys {
			preserve[joinPath(parts)] = true
		}
		normalized = StripDefaults(normalized, map[string]any(Defaults()), preserve)
	}
	var stamp StampHooks
	if fn := hooks().Stamp; fn != nil {
		stamp = fn()
	}
	if _, err := noteFirstStamp(h, raw, normalized, stamp); err != nil {
		return nil, nil, false, err
	}
	if err := AtomicConfigReplace(ctx, h, path, normalized, commentedSectionsForSave(normalized)); err != nil {
		return nil, nil, false, err
	}
	SecureFile(ctx, h, path)
	loadCacheMu.Lock()
	delete(rawCache, path)
	lastExpanded[path] = Clone(current).(map[string]any)
	loadCacheMu.Unlock()
	return raw, current, true, nil
}
