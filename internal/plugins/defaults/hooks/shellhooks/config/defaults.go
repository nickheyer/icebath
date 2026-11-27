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
		About:  "Your commands and web addresses that hear of agent events",
		Order:  10,
		Settings: []config.Setting{
			{Key: Events, Title: "Commands", About: "Commands that run when the agent acts, listed by event.",
				Help: "The Hooks screen adds, tests and allows them.", Slash: "hooks", Default: map[string]any{},
				Entry: &config.Entry{Noun: "event", Name: "Event", Names: pointOptions, Fields: []config.Setting{
					{Title: "Commands", About: "The commands that run on this event.", Default: []any{}, Entry: &config.Entry{Noun: "command", Fields: []config.Setting{
						{Key: "command", Title: "Command", About: "The command that runs.", Help: "It reads what happened as JSON on its input.", Default: "", Need: true},
						{Key: "matcher", Title: "Tools", About: "The tools the command runs for, as a pattern of their names.",
							Help: "Such as terminal|write_file. Only the events before and after a tool runs use it.", Default: "", Zero: "Every tool"},
						{Key: "timeout", Title: "Time limit", About: "How long the command may run.", Default: 60, Counts: "seconds",
							Least: config.Num(1), Most: config.Num(300)},
						{Key: "fail_closed", Title: "Block when it fails", About: "Block what the event guards when the command fails or runs out of time.",
							Help: "Only the events that can block use it.", Default: false},
					}}},
				}}},
			{Key: Outbound, Title: "Webhooks", About: "Web addresses that agent events are posted to.", Default: []any{},
				Entry: &config.Entry{Noun: "webhook", Fields: []config.Setting{
					{Key: "url", Title: "Address", About: "The web address that events are posted to.", Default: "", Need: true, Starts: []string{"https://", "http://"}},
					{Key: "events", Title: "Events", About: "The events that are posted to the address.", Default: []string{}, Options: pointOptions, Need: true},
					{Key: "name", Title: "Name", About: "A name for the webhook, shown where it is listed.", Default: ""},
					{Key: "matcher", Title: "Tools", About: "The tools it posts for, as a pattern of their names.",
						Help: "Such as terminal|write_file. Only the events before and after a tool runs use it.", Default: "", Zero: "Every tool"},
					{Key: "timeout", Title: "Time limit", About: "How long one post may take.", Default: 10, Counts: "seconds", Least: config.Num(1),
						Most: config.Num(60)},
					{Key: "secret_env", Title: "Secret variable", About: "The environment variable that holds the secret posts are signed with.",
						Default: ""},
					{Key: "secret", Title: "Secret", About: "The secret posts are signed with, when no variable holds it.", Default: "", Secret: true},
				}}},
			{Key: AutoAccept, Title: "Run without asking", About: "Run shell hooks without asking you first.",
				Help: "Hooks run commands on your computer, so turn this on only for settings you trust.", Default: false,
				Env: []string{"ICEBATH_ACCEPT_HOOKS"}},
		},
	}})
}
