package gateway

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Marker of a request option the body did not carry
type apiRequestOptionMissingValue struct{}

var apiRequestOptionMissing any = &apiRequestOptionMissingValue{}

var (
	apiTrueRequestBoolStrings  = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
	apiFalseRequestBoolStrings = map[string]bool{"0": true, "false": true, "no": true, "off": true}
	apiReasoningEfforts        = map[string]bool{"none": true, "minimal": true, "low": true, "medium": true, "high": true,
		"xhigh": true, "max": true, "ultra": true}
	apiRuntimeAgentOverrideKeys = []string{"api_key", "base_url", "provider", "api_mode", "command", "args", "credential_pool"}
)

// One SSE frame with optional id and event lines followed by the JSON data line
func apiSSEFrame(data any, event string, ensureASCII bool, id *int) ([]byte, error) {
	prefix := ""
	if id != nil {
		prefix = fmt.Sprintf("id: %d\n", *id)
	}
	if event != "" {
		prefix += "event: " + event + "\n"
	}
	encoded, err := pyjson.Dumps(data, pyjson.EnsureASCII(ensureASCII))
	if err != nil {
		return nil, err
	}
	return []byte(prefix + "data: " + encoded + "\n\n"), nil
}

// Boolean of a bool like request value, only explicit bool ish scalars counting and anything else the default
func apiCoerceRequestBool(value any, def bool) bool {
	switch v := value.(type) {
	case bool:
		return v
	case nil:
		return def
	case string:
		normalized := strings.ToLower(pystr.Strip(v))
		if apiTrueRequestBoolStrings[normalized] {
			return true
		}
		if apiFalseRequestBoolStrings[normalized] {
			return false
		}
		return def
	case float64:
		return v != 0
	}
	if n, ok := pyrepr.Int(value); ok {
		return n.Sign() != 0
	}
	return def
}

// Stripped request string, nil for an absent, blank or non string value
func apiCleanRequestString(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	stripped := pystr.Strip(text)
	if stripped == "" {
		return nil
	}
	return &stripped
}

// Merges the resolved provider runtime fields into the agent keyword arguments in place
func apiApplyRuntimeAgentOverrides(runtimeKwargs map[string]any, overrides any) map[string]any {
	obj, ok := pyjson.AsObject(overrides)
	if !ok {
		return runtimeKwargs
	}
	for _, key := range apiRuntimeAgentOverrideKeys {
		value := obj.GetOr(key, nil)
		if value == nil {
			continue
		}
		if list, isList := value.([]any); isList && key == "args" {
			value = append([]any(nil), list...)
		}
		runtimeKwargs[key] = value
	}
	return runtimeKwargs
}
