package provider

import (
	"os"
	"reflect"
	"strings"
)

var (
	ResponsesBypassFields       = []string{"input", "tools"}
	ChatCompletionsBypassFields = []string{"messages", "tools"}
)

const sdkTransformEscapeHatchEnv = "ICEBATH_CODEX_SDK_TRANSFORM"

// True when the value holds only JSON wire types, so typed values keep the typed request path
func sdkIsPlainJSONData(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return true
		}
		return rv.Kind() == reflect.Interface && sdkIsPlainJSONData(rv.Elem().Interface())
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return false
		}
		for it := rv.MapRange(); it.Next(); {
			if !sdkIsPlainJSONData(it.Value().Interface()) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		for i := 0; i < rv.Len(); i++ {
			if !sdkIsPlainJSONData(rv.Index(i).Interface()) {
				return false
			}
		}
		return true
	}
	return false
}

// True for a dict or list shaped request field
func sdkIsContainer(value any) bool {
	if value == nil {
		return false
	}
	switch reflect.TypeOf(value).Kind() {
	case reflect.Map:
		return true
	case reflect.Slice, reflect.Array:
		return reflect.TypeOf(value).Elem().Kind() != reflect.Uint8
	}
	return false
}

// Moves wire format bulk fields into extra_body, leaving empty list slots when asked, the request itself when nothing moves
func BypassSDKRequestTransform(requestKwargs map[string]any, fields []string, keepSlots bool) map[string]any {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(sdkTransformEscapeHatchEnv))) {
	case "1", "true", "yes", "on":
		return requestKwargs
	}
	if fields == nil {
		fields = ResponsesBypassFields
	}
	moved := map[string]any{}
	var order []string
	for _, f := range fields {
		if v, ok := requestKwargs[f]; ok && sdkIsContainer(v) && sdkIsPlainJSONData(v) {
			if _, seen := moved[f]; !seen {
				order = append(order, f)
			}
			moved[f] = v
		}
	}
	if len(moved) == 0 {
		return requestKwargs
	}
	bypassed := map[string]any{}
	for key, value := range requestKwargs {
		if _, isMoved := moved[key]; isMoved {
			if keepSlots {
				bypassed[key] = []any{}
			}
			continue
		}
		bypassed[key] = value
	}
	merged := map[string]any{}
	if extra, ok := bypassed["extra_body"].(map[string]any); ok {
		for k, v := range extra {
			merged[k] = v
		}
	}
	for _, f := range order {
		if _, exists := merged[f]; !exists {
			merged[f] = moved[f]
		}
	}
	bypassed["extra_body"] = merged
	return bypassed
}

// Chat completions bypass applied only for the real SDK client, which merges extra_body after building the body
func BypassChatSDKRequestTransform(requestKwargs map[string]any, client any) map[string]any {
	if _, ok := client.(*auxOpenAIClient); !ok {
		return requestKwargs
	}
	return BypassSDKRequestTransform(requestKwargs, ChatCompletionsBypassFields, true)
}
