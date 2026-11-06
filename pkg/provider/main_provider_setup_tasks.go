package provider

// Built in auxiliary task row with its config key, picker display name and short description
type auxTaskRow struct {
	Key, Name, Desc string
}

// Built in auxiliary tasks in the order the aux config picker lists them
var auxTasks = []auxTaskRow{
	{"vision", "Vision", "image/screenshot analysis"},
	{"compression", "Compression", "context summarization"},
	{"approval", "Approval", "smart command approval"},
	{"mcp", "MCP", "MCP tool reasoning"},
	{"title_generation", "Title generation", "session titles"},
	{"review", "Review", "/review reviewer subagent"},
	{"memory_query_rewrite", "Memory query rewrite", "memory retrieval queries"},
	{"tts_audio_tags", "TTS audio tags", "Gemini TTS tag insertion"},
	{"voice_chat", "Voice chat", "spoken voice-mode replies"},
	{"skills_hub", "Skills hub", "skills search/install"},
	{"triage_specifier", "Triage specifier", "kanban spec fleshing"},
	{"kanban_decomposer", "Kanban decomposer", "task decomposition"},
	{"profile_describer", "Profile describer", "auto profile descriptions"},
	{"curator", "Curator", "skill-usage review pass"},
}

// Subagent delegation row the picker shows beside the aux tasks, routed under the top level delegation section
const (
	delegationTaskKey  = "delegation"
	delegationTaskName = "Delegation"
	delegationTaskDesc = "subagent model (delegate_task)"
)

// Keys of the built in auxiliary tasks in picker order
func builtinAuxTaskKeys() []string {
	keys := make([]string, 0, len(auxTasks))
	for _, row := range auxTasks {
		keys = append(keys, row.Key)
	}
	return keys
}

// BuiltinAuxTaskKeys lists the built in auxiliary task keys in picker order
func BuiltinAuxTaskKeys() []string { return builtinAuxTaskKeys() }
