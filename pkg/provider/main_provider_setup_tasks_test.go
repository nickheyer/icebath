package provider

import (
	"strings"
	"testing"
)

func TestBuiltinAuxTaskKeysFollowHermesOrder(t *testing.T) {
	want := "vision,compression,approval,mcp,title_generation,review,memory_query_rewrite,tts_audio_tags,voice_chat,skills_hub,triage_specifier,kanban_decomposer,profile_describer,curator"
	if got := strings.Join(BuiltinAuxTaskKeys(), ","); got != want {
		t.Fatalf("keys %s", got)
	}
	if auxTasks[3].Name != "MCP" || auxTasks[13].Desc != "skill-usage review pass" {
		t.Fatalf("rows %+v", auxTasks)
	}
	if delegationTaskKey != "delegation" || delegationTaskName != "Delegation" || delegationTaskDesc != "subagent model (delegate_task)" {
		t.Fatal("delegation row")
	}
	keys := BuiltinAuxTaskKeys()
	keys[0] = "mutated"
	if auxTasks[0].Key != "vision" {
		t.Fatal("caller mutation reached the table")
	}
}
