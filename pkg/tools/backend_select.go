package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/settings"
)

const defaultBrowserProvider = "local"

// Setting holding the provider choice of each section the tools picker writes
var selectionKeys = map[string]string{
	"browser": settings.BrowserCloudProvider.Key, "web": settings.WebSearch.Key, "image_gen": settings.ImageGenProvider.Key,
	"video_gen": settings.VideoGenProvider.Key, "tts": settings.SpeechVoice.Key, "stt": settings.RecognitionService.Key,
}

// Settings pinning one web capability to its own backend
var webCapabilityKeys = map[string]string{"search": settings.WebSearchBackend.Key, "extract": settings.WebExtractBackend.Key}

// PoolKeySource answers the credential pool reads of the provider key resolver, installed by the provider wiring
type PoolKeySource interface {
	// Whether auth remove recorded the source as suppressed for the provider
	SourceSuppressed(env *Env, providerID, source string) bool
	// Runtime key else access token of the profile default login of the pool key, empty when none
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

// ResolveProviderSecret resolves a voice provider key through config, scope, .env, then the profile default login of the credential pool
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

// Value of a setting the user stored, stripped and spelled as written, empty when it holds only its default
func storedSetting(env *Env, key string) string {
	v, err := settings.Resolve(envHomeCtx(context.Background(), env), key)
	if err != nil || v.Source == settings.SourceDefault {
		return ""
	}
	return pystr.Strip(pyOrStr(v.Value))
}

// Lowercased value of a setting the user stored, empty when it holds only its default
func storedChoice(env *Env, key string) string { return pystr.Lower(storedSetting(env, key)) }

// StoredProvider returns the provider name stored for a section spelled as the user wrote it, empty when none was chosen
func StoredProvider(env *Env, section string) string {
	key, ok := selectionKeys[section]
	if !ok {
		return ""
	}
	return storedSetting(env, key)
}

// ReadSelection returns the provider choice stored for a section in lower case, empty when none was chosen
func ReadSelection(env *Env, section string) string { return pystr.Lower(StoredProvider(env, section)) }

// ReadWebCapabilitySelection returns the selection deciding one web capability
func ReadWebCapabilitySelection(env *Env, capability string) string {
	if key, ok := webCapabilityKeys[capability]; ok {
		if pin := storedChoice(env, key); pin != "" {
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
	if section != "web" {
		return false
	}
	for _, key := range webCapabilityKeys {
		if storedChoice(env, key) != "" {
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

// Settings screen group holding the provider choice of each section
var selectionGroups = map[string]settings.GroupID{
	"browser": settings.GroupTools, "web": settings.GroupTools, "image_gen": settings.GroupMedia,
	"video_gen": settings.GroupMedia, "tts": settings.GroupVoice, "stt": settings.GroupVoice,
}

// SelectionError is the uniform message for a selected but broken provider, naming where the choice is changed
func SelectionError(section, selectionName, failure string) string {
	if note := RemovedBackendNote(section, selectionName); note != "" {
		failure = note
	}
	where := "/settings"
	if group, ok := settings.GroupOf(selectionGroups[section]); ok {
		where += " → " + group.Label
	}
	return section + " is configured to use " + selectionName + ", but " + failure + ". Pick another one in " + where + "."
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
