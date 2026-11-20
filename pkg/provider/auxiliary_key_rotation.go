package provider

import (
	"context"
	"reflect"
)

// Swaps a revoked main key for its replacement inside the published runtime map in place
func RotateRuntimeMainAPIKey(ctx context.Context, old, new any) {
	runtime := auxRuntimeMain(ctx)
	if runtime == nil {
		return
	}
	cur, has := runtime["api_key"]
	if !has {
		cur = nil
	}
	if auxSameValue(cur, old) {
		runtime["api_key"] = auxNormalizeAPIKey(new)
	}
}

// Python equality for two loosely typed values, false for uncomparable kinds
func auxSameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if !reflect.TypeOf(a).Comparable() || !reflect.TypeOf(b).Comparable() {
		return reflect.DeepEqual(a, b)
	}
	return a == b
}
