package agent

import (
	"context"
	"fmt"
	"reflect"
)

const heldTransformFailedReply = "This reply was withheld: the plugin that rewrites replies in this session failed."

// Outcome of the transform_llm_output hook recorded for one turn id
type llmOutputTransformRecord struct {
	turnID       any
	transformed  bool
	preTransform any
}

// Fires a lifecycle plugin hook and returns every callback result
func invokeHookSafely(ctx context.Context, name string, kwargs map[string]any) []any {
	return lifecycleInvokeHook(ctx, name, kwargs)
}

// Reports whether two turn ids are equal the way Python compares them, unhashable ids never matching
func sameTurnID(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if !reflect.TypeOf(a).Comparable() || !reflect.TypeOf(b).Comparable() {
		return false
	}
	return a == b
}

// Fires transform_llm_output once per turn id and returns the response, whether it changed and the original
func applyLLMOutputTransform(ctx context.Context, a *AIAgent, finalResponse string, turnID any, platform *string) (string, bool, any) {
	if recorded := a.llmOutputTransform; recorded != nil && sameTurnID(recorded.turnID, turnID) {
		return finalResponse, recorded.transformed, recorded.preTransform
	}
	if finalResponse == "" {
		return finalResponse, false, nil
	}
	plat := a.platform
	if platform != nil {
		plat = *platform
	}
	transformed := false
	var preTransform any
	payload := map[string]any{"response_text": finalResponse, "session_id": a.sessionID, "model": a.model, "platform": plat, "turn_id": turnID}
	var held func(context.Context, map[string]any) (string, bool)
	if a.holdStream {
		held = a.heldTransforms
	}
	rewrite := ""
	if held != nil {
		rewrite = runHeldTransforms(ctx, held, payload)
	} else {
		for _, r := range invokeHookSafely(ctx, "transform_llm_output", payload) {
			if s, ok := r.(string); ok && s != "" {
				rewrite = s
				break
			}
		}
	}
	if rewrite != "" {
		preTransform, finalResponse, transformed = finalResponse, rewrite, true
	}
	a.llmOutputTransform = &llmOutputTransformRecord{turnID: turnID, transformed: transformed, preTransform: preTransform}
	return finalResponse, transformed, preTransform
}

// Runs the transforms captured at the held turn start, the withheld notice replacing the text when one failed
func runHeldTransforms(ctx context.Context, held func(context.Context, map[string]any) (string, bool), payload map[string]any) string {
	rewrite, failed := func() (rewrite string, failed bool) {
		defer func() {
			if r := recover(); r != nil {
				conversationLoopLog().Warn("held transform_llm_output dispatch failed", "error", fmt.Sprint(r))
				rewrite, failed = "", true
			}
		}()
		return held(ctx, payload)
	}()
	if failed {
		return heldTransformFailedReply
	}
	return rewrite
}

// ApplyLLMOutputTransform is the codex runtime hook over the per turn transform
func (a *AIAgent) ApplyLLMOutputTransform(finalResponse string, turnID any) (string, bool, any) {
	return applyLLMOutputTransform(a.homeContext(), a, finalResponse, turnID, nil)
}
