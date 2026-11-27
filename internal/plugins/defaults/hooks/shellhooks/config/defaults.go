package config

import (
	"github.com/nickheyer/icebath/internal/config"
	hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: hooksconfig.Key,
		Title:  "Shell hooks",
		About:  "Commands of yours that run on the agent's events, and addresses the events are posted to",
		Settings: []config.Setting{
			{Key: Events, Title: "Shell hooks", About: "Commands to run on agent events, grouped by event name. Each event holds a list of hook definitions: " +
				"command; matcher, a pattern of tool names for pre_tool_call and post_tool_call; timeout, seconds at most 300 (60 when unset); fail_closed, " +
				"for pre_tool_call only. Unknown event names are ignored with a warning", Default: map[string]any{}},
			{Key: AutoAccept, Title: "Auto-accept hooks", About: "Approve shell hooks without asking first. Hooks run commands on your machine, so only " +
				"enable this for trusted configs", Default: false, Env: []string{"ICEBATH_ACCEPT_HOOKS"}},
			{Key: Outbound, Title: "Outbound webhooks", About: "Addresses agent events are posted to, a list of targets: url (http or https), events, " +
				"matcher of tool names for pre_tool_call and post_tool_call, timeout in seconds at most 60 (10 when unset), name, and secret_env or " +
				"secret to sign the posts with", Default: []any{}},
		},
	}})
}
