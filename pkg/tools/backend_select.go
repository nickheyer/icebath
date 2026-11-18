package tools

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const defaultBrowserProvider = "local"

var extraSelectionKeys = map[string][]string{"web": {"search_backend", "extract_backend"}}
var defaultNameKeys = []string{"provider", "backend", "cloud_provider"}
var selectionNameKeys = map[string][]string{
	"browser": {"cloud_provider"}, "web": {"backend"},
	"image_gen": defaultNameKeys, "video_gen": defaultNameKeys, "tts": defaultNameKeys, "stt": defaultNameKeys,
}

// PoolKeySource answers the credential pool reads of the provider key resolver, installed by the provider wiring
type PoolKeySource interface {
	// Whether auth remove recorded the source as suppressed for the provider
	SourceSuppressed(env *Env, providerID, source string) bool
	// Runtime key else access token of the first pooled credential of the pool key, empty when none
	PoolAPIKey(env *Env, providerID string) string
}

// PoolKeys is the installed credential pool reader, nil meaning no pool is consulted
var PoolKeys PoolKeySource

// NormalizeBrowserCloudProvider returns the normalized browser provider key
func NormalizeBrowserCloudProvider(value string) string {
	provider := pystr.Lower(pystr.Strip(value))
	if provider == "" {
		return defaultBrowserProvider
	}
	return provider
}

// Process home directory seam, the Path.home lookup
var modalHomeDir = os.UserHomeDir

// Credential through the profile secret scope, a multiplexed miss never borrowing the process environment
func scopedCredential(env *Env, name string) string {
	if isMultiplexActive(env) && !IsGlobalEnv(name) {
		if env == nil || env.Secrets == nil {
			return ""
		}
		return pystr.Strip(env.Secrets.File()[name])
	}
	if env != nil && env.Secrets != nil {
		return pystr.Strip(env.Secrets.String(name))
	}
	return pystr.Strip(os.Getenv(name))
}

// Credential through the profile secret scope for availability checks outside the package
func ScopedCredential(env *Env, name string) string { return scopedCredential(env, name) }

// Scoped environment value when present, else the profile .env value, the get_env_value order
func dotenvValue(env *Env, name string) string {
	if isMultiplexActive(env) && !IsGlobalEnv(name) {
		if env != nil && env.Secrets != nil {
			if v, ok := env.Secrets.File()[name]; ok {
				return pystr.Strip(v)
			}
		}
		return ""
	}
	if v, ok := os.LookupEnv(name); ok {
		return pystr.Strip(v)
	}
	if env == nil || env.Secrets == nil {
		return ""
	}
	return pystr.Strip(env.Secrets.File()[name])
}

// Bool coercion over the shared truthy string set, default for a missing value
func isTruthyValue(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case string:
		switch pystr.Lower(pystr.Strip(x)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(v)
}

// HasDirectModalCredentials reports whether a Modal token pair or ~/.modal.toml exists
func HasDirectModalCredentials(env *Env) bool {
	if scopedCredential(env, "MODAL_TOKEN_ID") != "" && scopedCredential(env, "MODAL_TOKEN_SECRET") != "" {
		return true
	}
	home, err := modalHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".modal.toml"))
	return err == nil
}

func envSourceSuppressed(env *Env, providerID, envVar string) bool {
	if providerID == "" || PoolKeys == nil {
		return false
	}
	return PoolKeys.SourceSuppressed(env, providerID, "env:"+envVar)
}

// ResolveProviderSecret resolves a voice provider key through config, scope, .env, then the credential pool
func ResolveProviderSecret(env *Env, envVar, providerID, configValue string, envGetter func(string) string) string {
	if key := pystr.Strip(configValue); key != "" {
		return key
	}
	suppressed := envSourceSuppressed(env, providerID, envVar)
	if !suppressed {
		if key := scopedCredential(env, envVar); key != "" {
			return key
		}
	}
	if isMultiplexActive(env) {
		return ""
	}
	key := ""
	if !suppressed {
		if envGetter != nil {
			key = pystr.Strip(envGetter(envVar))
		} else {
			key = dotenvValue(env, envVar)
		}
	}
	if key != "" || providerID == "" {
		return key
	}
	if PoolKeys != nil {
		for _, poolKey := range []string{providerID, "custom:" + providerID} {
			if k := pystr.Strip(PoolKeys.PoolAPIKey(env, poolKey)); k != "" {
				return k
			}
		}
	}
	return ""
}

// ResolveOpenAIAudioAPIKey prefers the voice tools key then the scoped OpenAI key with pool fallback
func ResolveOpenAIAudioAPIKey(env *Env) string {
	if key := ResolveProviderSecret(env, "VOICE_TOOLS_OPENAI_KEY", "", "", nil); key != "" {
		return key
	}
	return ResolveProviderSecret(env, "OPENAI_API_KEY", "openai-api", "", nil)
}

// SelectionConfigKeys lists the dotted config keys the selection readers honour
func SelectionConfigKeys() map[string]bool {
	out := map[string]bool{}
	for section, nameKeys := range selectionNameKeys {
		keys := append(append([]string(nil), nameKeys...), extraSelectionKeys[section]...)
		for _, k := range keys {
			out[section+"."+k] = true
		}
	}
	return out
}

// Reads the raw unmerged user config document as a tree
func readRawConfig(env *Env) (config.Tree, error) {
	if env == nil || env.Home == "" {
		return nil, os.ErrNotExist
	}
	doc, err := config.ReadUser(env.Home)
	if err != nil {
		return nil, err
	}
	return config.Tree(doc), nil
}

func rawSection(env *Env, section string) map[string]any {
	raw, err := readRawConfig(env)
	if err != nil {
		return nil
	}
	m, ok := raw[section].(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// ReadSelection returns the persisted tools selection for a section, a vendor, or empty
func ReadSelection(env *Env, section string) string {
	raw := rawSection(env, section)
	if raw == nil {
		return ""
	}
	keys, ok := selectionNameKeys[section]
	if !ok {
		keys = defaultNameKeys
	}
	for _, key := range keys {
		if raw[key] == nil {
			continue
		}
		if text := pystr.Lower(pystr.Strip(pyrepr.Str(raw[key]))); text != "" {
			return text
		}
	}
	return ""
}

// ReadWebCapabilitySelection returns the selection deciding one web capability
func ReadWebCapabilitySelection(env *Env, capability string) string {
	if capability != "" {
		raw := rawSection(env, "web")
		if pin := pystr.Lower(pystr.Strip(pyOrStr(raw[capability+"_backend"]))); pin != "" {
			return pin
		}
	}
	return ReadSelection(env, "web")
}

// SelectionExists reports whether any selection signal was ever written for a section
func SelectionExists(env *Env, section string) bool {
	if ReadSelection(env, section) != "" {
		return true
	}
	extra := extraSelectionKeys[section]
	if len(extra) == 0 {
		return false
	}
	raw := rawSection(env, section)
	if raw == nil {
		return false
	}
	for _, key := range extra {
		if pystr.Strip(pyOrStr(raw[key])) != "" {
			return true
		}
	}
	return false
}

// RemovedBackends explains backends that used to ship in tree
var RemovedBackends = map[string]map[string]string{}

// RemovedBackendNote explains a removed backend or returns empty
func RemovedBackendNote(section, name string) string {
	key := pystr.Lower(strings.Trim(pystr.Strip(name), "'\""))
	return RemovedBackends[section][key]
}

// SelectionError is the uniform message for a selected but broken provider
func SelectionError(section, selectionName, failure string) string {
	if note := RemovedBackendNote(section, selectionName); note != "" {
		failure = note
	}
	return section + " is configured to use " + selectionName + " (set via icebath tools), but " + failure + ". Run 'icebath tools' to change it."
}

// FalKeyIsConfigured reports whether FAL_KEY holds a non blank value in scope or .env
func FalKeyIsConfigured(env *Env) bool {
	return scopedCredential(env, "FAL_KEY") != "" || dotenvValue(env, "FAL_KEY") != ""
}

// Text of value or the empty string when the value is falsy, the str(value or "") idiom
func pyOrStr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}
