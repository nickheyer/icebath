package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Answers and outcome of a batch clarify, the tools/clarify_batch.py settle_batch rule
func SettleBatch(locked *pyjson.Object, frameResult any, ended string) *pyjson.Object {
	return settleBatch(locked, frameResult, ended)
}
