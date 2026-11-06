package tools

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// NousManagedProvider is the stored selection naming the Nous Tool Gateway
const NousManagedProvider = "nous"

const (
	defaultBrowserProvider = "local"
	defaultModalMode       = "auto"
)

var validModalModes = map[string]bool{"auto": true, "direct": true, "managed": true}

var extraSelectionKeys = map[string][]string{"web": {"search_backend", "extract_backend"}}
var defaultNameKeys = []string{"provider", "backend", "cloud_provider"}
var selectionNameKeys = map[string][]string{
	"browser": {"cloud_provider"}, "web": {"backend"},
	"image_gen": defaultNameKeys, "video_gen": defaultNameKeys, "tts": defaultNameKeys, "stt": defaultNameKeys,
}

// NousAccountSource answers Nous Portal entitlement and token questions, installed by the provider wiring
type NousAccountSource interface {
	ManagedToolsEnabled(env *Env, forceFresh bool) bool
	UnavailableMessage(env *Env, capability string, forceFresh bool) string
	PeekAccessToken(env *Env) string
	ReadAccessToken(env *Env) string
	SourceSuppressed(env *Env, providerID, source string) bool
	PoolAPIKey(env *Env, providerID string) string
	// Whether a Nous identity exists for the profile and whether it is the anonymous guest tier
	IdentityState(env *Env) (hasIdentity bool, isGuest bool)
	GuestAllowed(env *Env) bool
	LastMintFailure(env *Env) (MintFailure, bool)
	AnonFailureCopy(env *Env, code string, retryAfter float64) string
	EnsurePortalIdentity(env *Env) error
	ManagedToolsRolledOut(env *Env) bool
}

// NousAccount is the installed account source, nil meaning no Portal login is available
var NousAccount NousAccountSource

// Coarse gate for the Nous Tool Gateway, false on any failure
func managedNousToolsEnabled(env *Env, forceFresh bool) bool {
	if NousAccount == nil {
		return false
	}
	return NousAccount.ManagedToolsEnabled(env, forceFresh)
}

// Account aware guidance for an unavailable Nous Tool Gateway path
func nousToolGatewayUnavailableMessage(env *Env, capability string, forceFresh bool) string {
	if capability == "" {
		capability = "the Nous Tool Gateway"
	}
	if NousAccount != nil {
		if msg := NousAccount.UnavailableMessage(env, capability, forceFresh); msg != "" {
			return msg
		}
	}
	return capability + " is unavailable. Run `icebath model` to refresh your Nous Portal login and billing status."
}

// NormalizeBrowserCloudProvider returns the normalized browser provider key
func NormalizeBrowserCloudProvider(value string) string {
	provider := pystr.Lower(pystr.Strip(value))
	if provider == "" {
		return defaultBrowserProvider
	}
	return provider
}

// CoerceModalMode returns the requested modal mode when valid else the default
func CoerceModalMode(value string) string {
	mode := pystr.Lower(pystr.Strip(value))
	if validModalModes[mode] {
		return mode
	}
	return defaultModalMode
}

// NormalizeModalMode returns the requested modal mode when valid else the default
func NormalizeModalMode(value string) string { return CoerceModalMode(value) }

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

// ModalBackendState is the resolved direct versus managed Modal decision
type ModalBackendState struct {
	RequestedMode      string
	Mode               string
	HasDirect          bool
	ManagedReady       bool
	ManagedModeBlocked bool
	SelectedBackend    string
}

// ResolveModalBackendState picks direct or managed Modal, auto preferring managed when available
func ResolveModalBackendState(env *Env, modalMode string, hasDirect, managedReady bool, managedEnabled *bool) ModalBackendState {
	requested := CoerceModalMode(modalMode)
	enabled := false
	if managedEnabled != nil {
		enabled = *managedEnabled
	} else {
		enabled = managedNousToolsEnabled(env, false)
	}
	managedOK := enabled && managedReady
	selected := ""
	switch requested {
	case "managed":
		if managedOK {
			selected = "managed"
		}
	case "direct":
		if hasDirect {
			selected = "direct"
		}
	default:
		if managedOK {
			selected = "managed"
		} else if hasDirect {
			selected = "direct"
		}
	}
	return ModalBackendState{RequestedMode: requested, Mode: requested, HasDirect: hasDirect, ManagedReady: managedReady,
		ManagedModeBlocked: requested == "managed" && !enabled, SelectedBackend: selected}
}

func envSourceSuppressed(env *Env, providerID, envVar string) bool {
	if providerID == "" || NousAccount == nil {
		return false
	}
	return NousAccount.SourceSuppressed(env, providerID, "env:"+envVar)
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
	if NousAccount != nil {
		for _, poolKey := range []string{providerID, "custom:" + providerID} {
			if k := pystr.Strip(NousAccount.PoolAPIKey(env, poolKey)); k != "" {
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

// PrefersGateway reports whether a config section sets use_gateway
func PrefersGateway(env *Env, section string) bool {
	if env == nil {
		return false
	}
	sec, ok := env.Config.Get(section).(map[string]any)
	return ok && isTruthyValue(sec["use_gateway"], false)
}

// SelectionConfigKeys lists the dotted config keys the selection readers honour
func SelectionConfigKeys() map[string]bool {
	out := map[string]bool{}
	for section, nameKeys := range selectionNameKeys {
		keys := append([]string{"use_gateway"}, nameKeys...)
		keys = append(keys, extraSelectionKeys[section]...)
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

// ReadSelection returns the persisted tools selection for a section, nous, a vendor, or empty
func ReadSelection(env *Env, section string) string {
	raw := rawSection(env, section)
	if raw == nil {
		return ""
	}
	if isTruthyValue(raw["use_gateway"], false) {
		return NousManagedProvider
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
