package tuihost

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Answers and outcome of a batch clarify, the tools/clarify_batch.py settle_batch rule
func SettleBatch(locked *jsonutil.Object, frameResult any, ended string) *jsonutil.Object {
	return settleBatch(locked, frameResult, ended)
}
