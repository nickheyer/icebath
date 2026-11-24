package gateway

import (
	"fmt"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	opts := []jsonutil.Option{jsonutil.Spaced()}
	if ensureASCII {
		opts = append(opts, jsonutil.ASCII())
	}
	encoded, err := jsonutil.Encode(data, opts...)
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
		normalized := strings.ToLower(strings.TrimSpace(v))
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
	if n, ok := anyutil.Int(value); ok {
		return n != 0
	}
	return def
}

// Stripped request string, nil for an absent, blank or non string value
func apiCleanRequestString(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return nil
	}
	return &stripped
}

// Merges the resolved provider runtime fields into the agent keyword arguments in place
func apiApplyRuntimeAgentOverrides(runtimeKwargs map[string]any, overrides any) map[string]any {
	obj, ok := jsonutil.AsObject(overrides)
	if !ok {
		return runtimeKwargs
	}
	for _, key := range apiRuntimeAgentOverrideKeys {
		value := obj.ValueOr(key, nil)
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
