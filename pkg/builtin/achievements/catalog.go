package achievements

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

var tierNames = []string{"Copper", "Silver", "Gold", "Diamond", "Olympian"}

type achReq struct {
	metric string
	gte    int64
}

type achSpec struct {
	metric   string
	tiers    []int64
	requires []achReq
	secret   bool
}

// Catalog entry with its kind derived from requires, a max_ metric or a lifetime accumulation
func ach(id, name, description, category, icon string, spec achSpec) *pyjson.Object {
	kind := "lifetime"
	if spec.requires != nil {
		kind = "multi_condition"
	} else if len(spec.metric) >= 4 && spec.metric[:4] == "max_" {
		kind = "best_session"
	}
	item := obj("id", id, "name", name, "description", description, "category", category, "kind", kind, "icon", icon)
	if spec.secret {
		item.Set("secret", true)
	}
	if spec.requires != nil {
		reqs := make([]any, 0, len(spec.requires))
		for _, r := range spec.requires {
			reqs = append(reqs, obj("metric", r.metric, "gte", r.gte))
		}
		item.Set("requirements", reqs)
		return item
	}
	item.Set("threshold_metric", spec.metric)
	tiers := []any{}
	for i := 0; i < len(tierNames) && i < len(spec.tiers); i++ {
		tiers = append(tiers, obj("name", tierNames[i], "threshold", spec.tiers[i]))
	}
	item.Set("tiers", tiers)
	return item
}

// Catalog definitions in upstream order, rebuilt per call so callers may mutate entries freely
func catalog() []*pyjson.Object {
	return []*pyjson.Object{
		ach("let_him_cook", "Let Him Cook", "Let Hermes run a serious autonomous tool chain in one session.", "Agent Autonomy", "flame", achSpec{metric: "max_tool_calls_in_session", tiers: []int64{200, 500, 1200, 3000, 8000}}),
		ach("autonomous_avalanche", "Autonomous Avalanche", "Accumulate a lifetime avalanche of Hermes tool calls across sessions.", "Agent Autonomy", "avalanche", achSpec{metric: "total_tool_calls", tiers: []int64{1000, 3000, 8000, 20000, 50000}}),
		ach("toolchain_maxxer", "Toolchain Maxxer", "Use a wide spread of distinct Hermes tools in one session.", "Agent Autonomy", "nodes", achSpec{metric: "max_distinct_tools_in_session", tiers: []int64{18, 28, 45, 70, 100}}),
		ach("full_send", "Full Send", "Terminal, files, and web/browser all get involved in one real run.", "Agent Autonomy", "rocket", achSpec{requires: []achReq{{"max_terminal_calls_in_session", 180}, {"max_file_tool_calls_in_session", 120}, {"max_web_browser_calls_in_session", 60}}}),
		ach("subagent_commander", "Subagent Commander", "Coordinate delegated agent work.", "Agent Autonomy", "branch", achSpec{metric: "total_delegate_calls", tiers: []int64{5, 40, 100, 1000, 5000}}),
		ach("background_process_enjoyer", "Background Process Enjoyer", "Start or control enough long-running processes to deserve the title.", "Agent Autonomy", "daemon", achSpec{metric: "total_process_calls", tiers: []int64{300, 800, 2000, 6000, 15000}}),
		ach("cron_necromancer", "Cron Necromancer", "Raise scheduled autonomous jobs from the dead.", "Agent Autonomy", "clock", achSpec{metric: "total_cron_calls", tiers: []int64{1000, 3000, 8000, 20000, 50000}}),
		ach("red_text_connoisseur", "Red Text Connoisseur", "Encounter enough errors to develop a palate for red text.", "Debugging Chaos", "warning", achSpec{metric: "total_errors", tiers: []int64{1500, 4000, 10000, 25000, 75000}}),
		ach("stack_trace_sommelier", "Stack Trace Sommelier", "Taste tracebacks by the flight, not by the sip.", "Debugging Chaos", "wine", achSpec{metric: "traceback_events", tiers: []int64{300, 1000, 3000, 8000, 20000}}),
		ach("actually_read_the_logs", "Actually Read The Logs", "Inspect logs repeatedly instead of guessing.", "Debugging Chaos", "scroll", achSpec{metric: "log_read_events", tiers: []int64{1000, 3000, 8000, 20000, 50000}}),
		ach("port_3000_taken", "Port 3000 Is Taken", "Discover dev-server port conflict patterns enough times to become numb.", "Debugging Chaos", "plug", achSpec{metric: "port_conflict_events", tiers: []int64{15, 40, 100, 300, 1000}, secret: true}),
		ach("permission_denied_any_percent", "Permission Denied Any%", "Speedrun into permission walls.", "Debugging Chaos", "lock", achSpec{metric: "permission_denied_events", tiers: []int64{25, 75, 200, 600, 1500}, secret: true}),
		ach("dependency_hell_tourist", "Dependency Hell Tourist", "Package installs fail, then somehow life continues.", "Debugging Chaos", "package_skull", achSpec{requires: []achReq{{"install_error_events", 25}, {"install_success_events", 10}}}),
		ach("the_fix_was_restarting", "The Fix Was Restarting It", "Restart after enough error clusters to call it a technique.", "Debugging Chaos", "restart", achSpec{requires: []achReq{{"restart_after_error_events", 50}, {"total_errors", 4000}}}),
		ach("forgot_the_env_var", "Forgot The Env Var", "Auth or configuration failed because an environment variable was missing.", "Debugging Chaos", "key", achSpec{metric: "env_var_error_events", tiers: []int64{5000, 15000, 40000, 100000, 250000}, secret: true}),
		ach("yaml_colon_incident", "YAML Colon Incident", "Configuration syntax bites back.", "Debugging Chaos", "colon", achSpec{metric: "yaml_error_events", tiers: []int64{1000, 3000, 8000, 20000, 50000}, secret: true}),
		ach("docker_name_collision", "Docker Name Collision", "A container name already exists. Of course it does.", "Debugging Chaos", "container", achSpec{metric: "docker_conflict_events", tiers: []int64{75, 200, 600, 1500, 4000}, secret: true}),
		ach("supposed_to_be_quick", "This Was Supposed To Be Quick", "A tiny ask becomes an entire expedition.", "Vibe Coding", "melting_clock", achSpec{metric: "max_messages_in_session", tiers: []int64{300, 600, 1200, 2500, 6000}}),
		ach("one_more_small_change", "One More Small Change", "Make enough file edits in one session to invalidate the phrase small change.", "Vibe Coding", "pencil", achSpec{metric: "max_file_tool_calls_in_session", tiers: []int64{150, 400, 1000, 3000, 8000}}),
		ach("vibe_architect", "Vibe Architect", "Touch a broad surface area in one project session.", "Vibe Coding", "blueprint", achSpec{metric: "max_files_touched_in_session", tiers: []int64{300, 700, 1500, 4000, 10000}}),
		ach("pixel_goblin", "Pixel Goblin", "Do sustained frontend, CSS, SVG, or visual tuning.", "Vibe Coding", "pixel", achSpec{metric: "frontend_activity_events", tiers: []int64{20000, 50000, 120000, 300000, 800000}}),
		ach("ship_first_ask_later", "Ship First, Ask Later", "Git activity after a serious tool chain.", "Vibe Coding", "ship", achSpec{requires: []achReq{{"git_events", 50}, {"max_tool_calls_in_session", 500}}}),
		ach("css_exorcist", "CSS Exorcist", "Cast repeated styling demons out of the interface.", "Vibe Coding", "spark_cursor", achSpec{metric: "css_activity_events", tiers: []int64{10000, 30000, 80000, 200000, 500000}}),
		ach("one_character_fix", "One Character Fix", "A tiny edit after a pile of errors. Painful. Beautiful.", "Vibe Coding", "needle", achSpec{requires: []achReq{{"tiny_patch_after_errors_events", 5}, {"total_errors", 4000}}, secret: true}),
		ach("skillsmith", "Skillsmith", "Work with Hermes skills enough to leave fingerprints.", "Hermes Native", "hammer_scroll", achSpec{metric: "skill_events", tiers: []int64{5000, 15000, 40000, 100000, 250000}}),
		ach("skill_issue_skill_created", "Skill Issue? Skill Created.", "Create or patch durable procedures instead of repeating yourself.", "Hermes Native", "anvil", achSpec{metric: "skill_manage_events", tiers: []int64{25, 75, 200, 600, 1500}}),
		ach("memory_keeper", "Memory Keeper", "Persist durable knowledge with memory or Mnemosyne.", "Hermes Native", "crystal", achSpec{metric: "memory_events", tiers: []int64{100, 300, 1000, 3000, 8000}}),
		ach("memory_palace", "Memory Palace", "Build a serious durable-memory trail.", "Hermes Native", "palace", achSpec{metric: "memory_write_events", tiers: []int64{100, 300, 1000, 3000, 8000}}),
		ach("context_dragon", "Context Dragon", "Brush against compression, huge context, or token pressure repeatedly.", "Hermes Native", "dragon", achSpec{metric: "context_events", tiers: []int64{5000, 15000, 40000, 100000, 250000}}),
		ach("gateway_dweller", "Gateway Dweller", "Live through gateway-connected Hermes workflows.", "Hermes Native", "antenna", achSpec{metric: "gateway_events", tiers: []int64{5000, 15000, 40000, 100000, 250000}}),
		ach("plugin_goblin", "Plugin Goblin", "Use or develop plugins enough that the dashboard notices.", "Hermes Native", "puzzle", achSpec{metric: "plugin_events", tiers: []int64{1000, 3000, 8000, 20000, 50000}}),
		ach("rollback_wizard", "Rollback Wizard", "Invoke rollback/checkpoint recovery magic.", "Hermes Native", "rewind", achSpec{metric: "rollback_events", tiers: []int64{500, 1500, 4000, 10000, 25000}, secret: true}),
		ach("rabbit_hole_certified", "Rabbit Hole Certified", "Search or extract enough web content to qualify as a research spiral.", "Research/Web", "spiral", achSpec{metric: "total_web_calls", tiers: []int64{400, 1200, 3000, 8000, 20000}}),
		ach("citation_goblin", "Citation Goblin", "Extract enough web pages to become a tiny librarian.", "Research/Web", "quote", achSpec{metric: "total_web_extract_calls", tiers: []int64{100, 300, 1000, 3000, 8000}}),
		ach("docs_archaeologist", "Docs Archaeologist", "Dig through documentation sources over and over.", "Research/Web", "compass", achSpec{metric: "docs_activity_events", tiers: []int64{5000, 15000, 40000, 100000, 250000}}),
		ach("browser_possession", "Browser Possession", "Possess a browser through automation repeatedly.", "Research/Web", "browser", achSpec{metric: "browser_calls", tiers: []int64{75, 200, 600, 1500, 4000}}),
		ach("terminal_goblin", "Terminal Goblin", "Spend serious time in shell-land.", "Tool Mastery", "terminal", achSpec{metric: "total_terminal_calls", tiers: []int64{750, 2000, 6000, 15000, 50000}}),
		ach("patch_wizard", "Patch Wizard", "Bend files to your will with targeted patches.", "Tool Mastery", "wand", achSpec{metric: "total_patch_calls", tiers: []int64{250, 750, 2000, 6000, 15000}}),
		ach("file_archaeologist", "File Archaeologist", "Dig through the filesystem with reads and searches.", "Tool Mastery", "folder", achSpec{metric: "total_file_reads_searches", tiers: []int64{750, 2000, 6000, 15000, 50000}}),
		ach("image_whisperer", "Image Whisperer", "Use image generation or vision tools enough for visual work.", "Tool Mastery", "eye", achSpec{metric: "image_vision_calls", tiers: []int64{100, 300, 1000, 3000, 8000}}),
		ach("voice_of_the_machine", "Voice Of The Machine", "Use text-to-speech or voice tooling repeatedly.", "Tool Mastery", "wave", achSpec{metric: "tts_calls", tiers: []int64{10, 30, 100, 300, 800}}),
		ach("model_hopper", "Model Hopper", "Switch or inspect providers/models enough to count as a habit.", "Model Lore", "swap", achSpec{metric: "model_events", tiers: []int64{10000, 30000, 80000, 200000, 500000}}),
		ach("openrouter_enjoyer", "OpenRouter Enjoyer", "Route model work through OpenRouter repeatedly.", "Model Lore", "router", achSpec{metric: "openrouter_events", tiers: []int64{250, 750, 2000, 6000, 15000}}),
		ach("codex_conjurer", "Codex Conjurer", "Summon Codex-flavored assistance often enough for a ritual.", "Model Lore", "codex", achSpec{metric: "codex_events", tiers: []int64{500, 1500, 4000, 10000, 25000}}),
		ach("multi_model_mage", "Multi-Model Mage", "Use a real spread of distinct model names across Hermes history.", "Model Lore", "prism", achSpec{metric: "distinct_model_count", tiers: []int64{10, 20, 40, 80, 160}}),
		ach("five_model_flight", "Five-Model Flight", "Try at least five distinct LLMs instead of marrying the first model that answers.", "Model Lore", "prism", achSpec{metric: "distinct_model_count", tiers: []int64{5, 10, 20, 40, 80}}),
		ach("provider_polyglot", "Provider Polyglot", "Use models from multiple providers across Hermes history.", "Model Lore", "swap", achSpec{metric: "distinct_provider_count", tiers: []int64{2, 3, 5, 8, 12}}),
		ach("model_sommelier", "Model Sommelier", "Taste enough model/provider conversations to develop preferences.", "Model Lore", "wine", achSpec{metric: "model_events", tiers: []int64{250, 750, 2000, 6000, 15000}}),
		ach("claude_confidant", "Claude Confidant", "Bring Claude-flavored reasoning into the workflow repeatedly.", "Model Lore", "quote", achSpec{metric: "claude_events", tiers: []int64{50, 150, 500, 1500, 4000}}),
		ach("gemini_cartographer", "Gemini Cartographer", "Map enough Gemini-related workflows to know the terrain.", "Model Lore", "compass", achSpec{metric: "gemini_events", tiers: []int64{50, 150, 500, 1500, 4000}}),
		ach("open_weights_pilgrim", "Open Weights Pilgrim", "Actually chat with local/open-weight models through Hermes session metadata.", "Model Lore", "terminal", achSpec{metric: "local_model_chat_sessions", tiers: []int64{1, 3, 10, 30, 100}}),
		ach("toolset_cartographer", "Toolset Cartographer", "Navigate Hermes toolsets deliberately instead of treating tools as a blur.", "Hermes Native", "compass", achSpec{metric: "toolset_events", tiers: []int64{20, 60, 200, 600, 1500}}),
		ach("config_surgeon", "Config Surgeon", "Operate on real config files, manifests, env files, and dashboard settings without flinching.", "Hermes Native", "key", achSpec{metric: "config_events", tiers: []int64{100, 300, 1000, 3000, 10000}}),
		ach("rebase_acrobat", "Rebase Acrobat", "Handle real git history surgery: rebase, conflict, merge, fetch, push.", "Vibe Coding", "branch", achSpec{metric: "git_history_events", tiers: []int64{10, 30, 100, 300, 800}}),
		ach("test_suite_tamer", "Test Suite Tamer", "Run enough verification commands that green text becomes part of the ritual.", "Tool Mastery", "daemon", achSpec{metric: "test_events", tiers: []int64{100, 300, 800, 2400, 6000}}),
		ach("screenshot_hunter", "Screenshot Hunter", "Capture, inspect, and polish visual proof instead of just claiming it works.", "Tool Mastery", "eye", achSpec{metric: "screenshot_events", tiers: []int64{50, 150, 500, 1500, 5000}}),
		ach("marathon_operator", "Marathon Operator", "Accumulate a serious number of Hermes sessions.", "Lifestyle", "marathon", achSpec{metric: "session_count", tiers: []int64{75, 200, 500, 1500, 5000}}),
		ach("weekend_warrior", "Weekend Warrior", "Run Hermes on weekends enough times to make it a lifestyle.", "Lifestyle", "calendar", achSpec{metric: "weekend_sessions", tiers: []int64{25, 75, 200, 600, 1500}}),
		ach("night_shift_operator", "Night Shift Operator", "Run sessions during gremlin hours repeatedly.", "Lifestyle", "moon", achSpec{metric: "night_sessions", tiers: []int64{25, 75, 200, 600, 1500}}),
		ach("cache_hit_appreciator", "Cache Hit Appreciator", "Notice or benefit from prompt/cache behavior.", "Lifestyle", "cache", achSpec{metric: "cache_events", tiers: []int64{100, 300, 1000, 3000, 8000}, secret: true}),
	}
}

var metricLabels = map[string]string{
	"max_tool_calls_in_session":        "tool calls in one session",
	"max_distinct_tools_in_session":    "distinct Hermes tools used in one session",
	"max_terminal_calls_in_session":    "terminal calls in one session",
	"max_file_tool_calls_in_session":   "file/search/patch calls in one session",
	"max_web_browser_calls_in_session": "web search/extract or browser calls in one session",
	"max_messages_in_session":          "messages in one session",
	"max_files_touched_in_session":     "files touched in one session",
	"total_delegate_calls":             "lifetime delegate_task calls",
	"total_process_calls":              "lifetime background process operations",
	"total_cron_calls":                 "lifetime scheduled-job operations",
	"total_errors":                     "error/failed/traceback messages observed",
	"traceback_events":                 "traceback or exception mentions",
	"log_read_events":                  "log inspections",
	"port_conflict_events":             "dev-server port conflict detections",
	"permission_denied_events":         "permission-denied errors",
	"install_error_events":             "package-install failures",
	"install_success_events":           "successful package installs after package work",
	"restart_after_error_events":       "restart/reload actions after error clusters",
	"env_var_error_events":             "missing auth/config/environment-variable events",
	"yaml_error_events":                "YAML/config parse incidents",
	"docker_conflict_events":           "Docker/container-name conflicts",
	"frontend_activity_events":         "frontend/CSS/SVG/React activity mentions",
	"css_activity_events":              "CSS, styling, Tailwind, or className activity",
	"git_events":                       "git workflow commands",
	"tiny_patch_after_errors_events":   "tiny typo-style fixes after error clusters",
	"skill_events":                     "Hermes skill mentions or tool use",
	"skill_manage_events":              "skill_manage create/patch/delete operations",
	"memory_events":                    "memory or Mnemosyne tool events",
	"memory_write_events":              "durable memory writes",
	"context_events":                   "context, compression, token, or cache-pressure mentions",
	"gateway_events":                   "gateway/API/chat-platform activity",
	"plugin_events":                    "dashboard plugin development or usage signals",
	"rollback_events":                  "rollback/checkpoint recovery mentions",
	"docs_activity_events":             "documentation/README/docs activity",
	"model_events":                     "model/provider-related activity",
	"openrouter_events":                "OpenRouter mentions",
	"codex_events":                     "Codex mentions",
	"cache_events":                     "prompt-cache/cache-hit mentions",
	"total_web_calls":                  "lifetime web_search/web_extract calls",
	"total_web_extract_calls":          "lifetime web_extract calls",
	"browser_calls":                    "lifetime browser automation calls",
	"total_tool_calls":                 "lifetime Hermes tool calls",
	"total_terminal_calls":             "lifetime terminal calls",
	"total_patch_calls":                "lifetime targeted patch edits",
	"total_file_reads_searches":        "lifetime read_file/search_files calls",
	"image_vision_calls":               "image generation or vision tool calls",
	"tts_calls":                        "text-to-speech or voice tool calls",
	"distinct_model_count":             "distinct model names seen in session metadata",
	"distinct_provider_count":          "distinct model providers inferred from session metadata",
	"claude_events":                    "Claude/Anthropic model mentions",
	"gemini_events":                    "Gemini/Google model mentions",
	"local_model_events":               "local/open-weight model mentions",
	"local_model_chat_sessions":        "Hermes sessions whose model metadata is local/open-weight",
	"toolset_events":                   "toolset or tool-family mentions",
	"config_events":                    "configuration/environment/manifest activity",
	"git_history_events":               "git history operations such as rebase, merge, fetch, push, or tag",
	"test_events":                      "test/check/verification command mentions",
	"screenshot_events":                "screenshot, Playwright, PNG, or vision-inspection activity",
	"release_events":                   "release, version, publish, or git tag events",
	"session_count":                    "Hermes sessions",
	"weekend_sessions":                 "sessions started on weekends",
	"night_sessions":                   "sessions started late night or before dawn",
}
