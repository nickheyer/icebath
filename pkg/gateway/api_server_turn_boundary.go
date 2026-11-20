package gateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

var apiTranscriptIdentityKeys = []string{"role", "content", "tool_calls", "tool_call_id"}

// Whether the agent transcript starts with the prefix by what each message says, ignoring agent stamps
func apiSameTranscriptPrefix(agentMessages, prefix []any) bool {
	if len(agentMessages) < len(prefix) {
		return false
	}
	for i, want := range prefix {
		got := agentMessages[i]
		gotObj, gotDict := pyjson.AsObject(got)
		wantObj, wantDict := pyjson.AsObject(want)
		if !gotDict || !wantDict {
			if !pyrepr.Equal(got, want) {
				return false
			}
			continue
		}
		for _, key := range apiTranscriptIdentityKeys {
			if !pyrepr.Equal(gotObj.GetOr(key, nil), wantObj.GetOr(key, nil)) {
				return false
			}
		}
	}
	return true
}

// Index in the result messages where this turn's assistant and tool rows begin, zero meaning all
func APIResponseTurnStartIndex(conversationHistory []any, userMessage any, result any) int {
	obj, isDict := pyjson.AsObject(result)
	if !isDict {
		return 0
	}
	agentMessages, ok := obj.GetOr("messages", nil).([]any)
	if !ok || len(agentMessages) == 0 {
		return 0
	}
	if pyrepr.Truthy(obj.GetOr("_messages_are_turn_suffix", nil)) {
		return 0
	}
	if userIdx := state.ReanchorCurrentTurnUserIdx(agentMessages, userMessage); userIdx >= 0 {
		return userIdx + 1
	}
	prior := append([]any(nil), conversationHistory...)
	expectedPrefix := append(append([]any(nil), prior...), pyjson.ObjectOf("role", "user", "content", userMessage))
	if apiSameTranscriptPrefix(agentMessages, expectedPrefix) {
		return len(expectedPrefix)
	}
	if len(prior) > 0 && apiSameTranscriptPrefix(agentMessages, prior) {
		return len(prior)
	}
	return 0
}
