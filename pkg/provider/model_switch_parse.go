package provider

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// ModelSwitchResult is the outcome of one model switch attempt
type ModelSwitchResult struct {
	Success              bool
	NewModel             string
	TargetProvider       string
	ProviderChanged      bool
	APIKey               string
	BaseURL              string
	APIMode              string
	RequestOverrides     map[string]any
	ErrorMessage         string
	WarningMessage       string
	UsageNotice          string
	ProviderLabel        string
	ResolvedViaAlias     string
	Capabilities         *ModelCapabilities
	RuntimeCapabilities  map[string]bool
	ModelInfo            *ModelsDevEntry
	IsGlobal             bool
	AliasEndpointApplied bool
}

// ModelFlagParseResult holds the model text and flags of one model command
type ModelFlagParseResult struct {
	ModelInput       string
	ExplicitProvider string
	ReasoningEffort  string
	IsGlobal         bool
	ForceRefresh     bool
	IsSession        bool
	IsOnce           bool
}

var modelBoolFlags = map[string]string{"--global": "is_global", "--session": "is_session", "--refresh": "force_refresh", "--once": "is_once"}

var modelValueFlags = map[string]string{"--provider": "explicit_provider", "--reasoning": "reasoning_effort"}

var modelFlagDashRE = regexp.MustCompile("[‒–—―](provider|reasoning|global|session|refresh|once)")

// Whitespace as Python str.split sees it
func isPySpace(r rune) bool { return (r >= 0x1c && r <= 0x1f) || unicode.IsSpace(r) }

// ParseModelFlagsDetailed splits provider, reasoning, global, session, refresh and once flags from the model text
func ParseModelFlagsDetailed(rawArgs string) ModelFlagParseResult {
	rawArgs = modelFlagDashRE.ReplaceAllString(rawArgs, "--$1")
	var out ModelFlagParseResult
	var filtered []string
	tokens := strings.FieldsFunc(rawArgs, isPySpace)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if field, ok := modelBoolFlags[tok]; ok {
			switch field {
			case "is_global":
				out.IsGlobal = true
			case "is_session":
				out.IsSession = true
			case "force_refresh":
				out.ForceRefresh = true
			case "is_once":
				out.IsOnce = true
			}
			continue
		}
		if field, ok := modelValueFlags[tok]; ok && i+1 < len(tokens) {
			i++
			switch field {
			case "explicit_provider":
				out.ExplicitProvider = tokens[i]
			case "reasoning_effort":
				out.ReasoningEffort = tokens[i]
			}
			continue
		}
		filtered = append(filtered, tok)
	}
	out.ModelInput = pyStripSpace(strings.Join(filtered, " "))
	return out
}

// ResolvePersistBehavior decides whether a model switch persists to config.yaml
func ResolvePersistBehavior(ctx context.Context, isGlobal, isSession, isOnce bool, explicitProvider string) bool {
	if isOnce || isSession {
		return false
	}
	if isGlobal {
		return true
	}
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return false
	}
	modelCfg := tree["model"]
	if m, ok := modelCfg.(map[string]any); ok {
		if !(pyTruthy(m["default"]) || pyTruthy(m["provider"])) {
			return true
		}
		if pyTruthy(m["persist_switch_by_default"]) {
			return true
		}
		if explicitProvider != "" {
			return false
		}
		return false
	}
	return !pyTruthy(modelCfg)
}

// Error codes a model command request can carry
const (
	ModelSwitchErrOnceWithGlobal     = "once_with_global"
	ModelSwitchErrOnceRequiresTarget = "once_requires_target"
	ModelSwitchErrBadReasoning       = "bad_reasoning"
	ModelSwitchErrResetWithFlags     = "reset_with_flags"
)

// ModelSwitchErrorText is the surface neutral copy for each model command error code
var ModelSwitchErrorText = map[string]string{
	ModelSwitchErrOnceWithGlobal:     "/model --once cannot be combined with --global",
	ModelSwitchErrOnceRequiresTarget: "/model --once requires a model or provider.",
	ModelSwitchErrBadReasoning:       "/model --reasoning takes none, minimal, low, medium, high, xhigh, max or ultra.",
	ModelSwitchErrResetWithFlags: "/model reset takes no flags — it clears this session's model override so the " +
		"configured default applies again.",
}

// ModelSwitchResetToken clears the session model override instead of naming a model
const ModelSwitchResetToken = "reset"

// ModelSwitchRequest is a fully parsed model command with its requested scope and error codes
type ModelSwitchRequest struct {
	Raw              string
	Target           string
	ExplicitProvider string
	ReasoningEffort  string
	IsGlobal         bool
	IsSession        bool
	IsOnce           bool
	ForceRefresh     bool
	Scope            string
	Errors           []string
}

// ModelInput is the target text, matching the flag parse result field
func (r ModelSwitchRequest) ModelInput() string { return r.Target }

// ErrorMessages renders each error code through the canonical copy
func (r ModelSwitchRequest) ErrorMessages() []string {
	out := make([]string, 0, len(r.Errors))
	for _, code := range r.Errors {
		out = append(out, ModelSwitchErrorText[code])
	}
	return out
}

// IsReset reports a bare reset request that clears the session override
func (r ModelSwitchRequest) IsReset() bool {
	return strings.ToLower(pyStripSpace(r.Target)) == ModelSwitchResetToken && len(r.Errors) == 0
}

// ParseModelSwitchArgs tokenizes a model command and validates flag conflicts and the reasoning level
func ParseModelSwitchArgs(raw string) ModelSwitchRequest {
	parsed := ParseModelFlagsDetailed(raw)
	errs := []string{}
	if parsed.IsOnce && parsed.IsGlobal {
		errs = append(errs, ModelSwitchErrOnceWithGlobal)
	}
	if parsed.IsOnce && parsed.ModelInput == "" && parsed.ExplicitProvider == "" {
		errs = append(errs, ModelSwitchErrOnceRequiresTarget)
	}
	if parsed.ReasoningEffort != "" && ParseReasoningEffort(parsed.ReasoningEffort) == nil {
		errs = append(errs, ModelSwitchErrBadReasoning)
	}
	if strings.ToLower(pyStripSpace(parsed.ModelInput)) == ModelSwitchResetToken && (parsed.ExplicitProvider != "" ||
		parsed.ReasoningEffort != "" || parsed.IsGlobal || parsed.IsSession || parsed.IsOnce) {
		errs = append(errs, ModelSwitchErrResetWithFlags)
	}
	scope := "default"
	switch {
	case parsed.IsOnce:
		scope = "once"
	case parsed.IsSession:
		scope = "session"
	case parsed.IsGlobal:
		scope = "global"
	}
	return ModelSwitchRequest{
		Raw: raw, Target: parsed.ModelInput, Scope: scope, Errors: errs,
		ExplicitProvider: parsed.ExplicitProvider, ReasoningEffort: parsed.ReasoningEffort, IsGlobal: parsed.IsGlobal,
		IsSession: parsed.IsSession, IsOnce: parsed.IsOnce, ForceRefresh: parsed.ForceRefresh,
	}
}

// Model name candidate from a string, a mapping with a model key, or a value with a Model field
func effectiveModelCandidate(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return pyStripSpace(v)
	case map[string]any:
		return msClean(v["model"])
	}
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}
	field := rv.FieldByName("Model")
	if !field.IsValid() || !field.CanInterface() {
		return ""
	}
	return msClean(field.Interface())
}

// ResolveEffectiveModel picks the session override, then the channel, then the global model
func ResolveEffectiveModel(sessionOverrides, channelConfig, globalConfig any) string {
	for _, tier := range []any{sessionOverrides, channelConfig, globalConfig} {
		if candidate := effectiveModelCandidate(tier); candidate != "" {
			return candidate
		}
	}
	return ""
}
