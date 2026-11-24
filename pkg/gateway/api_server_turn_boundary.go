package gateway

import (
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var apiTranscriptIdentityKeys = []string{"role", "content", "tool_calls", "tool_call_id"}

// Whether the agent transcript starts with the prefix by what each message says, ignoring agent stamps
func apiSameTranscriptPrefix(agentMessages, prefix []any) bool {
	if len(agentMessages) < len(prefix) {
		return false
	}
	for i, want := range prefix {
		got := agentMessages[i]
		gotObj, gotDict := jsonutil.AsObject(got)
		wantObj, wantDict := jsonutil.AsObject(want)
		if !gotDict || !wantDict {
			if !anyutil.Equal(got, want) {
				return false
			}
			continue
		}
		for _, key := range apiTranscriptIdentityKeys {
			if !anyutil.Equal(gotObj.ValueOr(key, nil), wantObj.ValueOr(key, nil)) {
				return false
			}
		}
	}
	return true
}

// Index in the result messages where this turn's assistant and tool rows begin, zero meaning all
func APIResponseTurnStartIndex(conversationHistory []any, userMessage any, result any) int {
	obj, isDict := jsonutil.AsObject(result)
	if !isDict {
		return 0
	}
	agentMessages, ok := obj.ValueOr("messages", nil).([]any)
	if !ok || len(agentMessages) == 0 {
		return 0
	}
	if anyutil.IsTrue(obj.ValueOr("_messages_are_turn_suffix", nil)) {
		return 0
	}
	if userIdx := state.ReanchorCurrentTurnUserIdx(agentMessages, userMessage); userIdx >= 0 {
		return userIdx + 1
	}
	prior := append([]any(nil), conversationHistory...)
	expectedPrefix := append(append([]any(nil), prior...), jsonutil.ObjectOf("role", "user", "content", userMessage))
	if apiSameTranscriptPrefix(agentMessages, expectedPrefix) {
		return len(expectedPrefix)
	}
	if len(prior) > 0 && apiSameTranscriptPrefix(agentMessages, prior) {
		return len(prior)
	}
	return 0
}
