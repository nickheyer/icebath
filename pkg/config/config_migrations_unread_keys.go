package config

import "strings"

// Path of each setting nothing in this product reads, a path ending on a mapping standing for the whole mapping
var unreadKeys = [][]string{
	{"display", "compact"}, {"display", "resume_display"}, {"display", "resume_exchanges"}, {"display", "resume_max_user_chars"},
	{"display", "resume_max_assistant_chars"}, {"display", "resume_max_assistant_lines"}, {"display", "resume_skip_tool_only"},
	{"display", "persist_prompts"}, {"display", "credits_notices"}, {"display", "show_cost"}, {"display", "user_message_preview"},
	{"display", "copy_shortcut"},
	{"voice", "voice_chat_mode"}, {"voice", "gpt_live"}, {"voice", "client_direct"},
	{"auxiliary", "skills_hub"},
	{"mcp", "auto_reload_on_config_change"},
	{"bedrock", "discovery"},
	{"discord", "allow_any_attachment"},
	{"bot_mode", "envelope_ttl_seconds"},
	{"tools", "connectors"},
	{"paste_collapse_threshold_fallback"},
}

// Drops the settings nothing reads and keeps the rest of their sections
func migrateTo68(r *migrationRun) error {
	cfg := r.read()
	var removed []string
	for _, path := range unreadKeys {
		holder := cfg
		for _, section := range path[:len(path)-1] {
			holder, _ = holder[section].(map[string]any)
		}
		key := path[len(path)-1]
		if _, has := holder[key]; has {
			delete(holder, key)
			removed = append(removed, strings.Join(path, "."))
		}
	}
	if len(removed) == 0 {
		return nil
	}
	list := strings.Join(removed, ", ")
	return r.commit(cfg, "removed "+list, "  ✓ Removed "+list+" — nothing in icebath reads these settings.")
}
