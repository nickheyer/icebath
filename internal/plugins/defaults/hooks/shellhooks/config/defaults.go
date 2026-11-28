package config

import (
	"github.com/nickheyer/icebath/internal/config"
	hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: hooksconfig.Key,
		Into:   hooksconfig.Key,
		Title:  "Shell hooks",
		About:  "Your commands and web addresses that hear of agent events",
		Order:  10,
		Settings: []config.Setting{
			{Key: Events, Title: "Hooks", About: "Commands that run when the agent acts, listed by event.", Slash: "hooks", Default: map[string]any{},
				Custom: true, Entry: &config.Entry{Noun: "event", Name: "Event", Names: pointOptions, Fields: []config.Setting{
					{Title: "Hooks", About: "The hooks that run on this event.", Default: []any{}, Entry: &config.Entry{Noun: "hook", Fields: hookFields()}},
				}}},
			{Key: Outbound, Title: "Webhooks", About: "Web addresses that agent events are posted to.", Slash: "hooks", Default: map[string]any{},
				Custom: true, Entry: &config.Entry{Noun: "webhook", Fields: webhookFields()}},
			{Key: AutoAccept, Title: "Run without asking", About: "Run every hook without asking you first.",
				Help: "Hooks run commands on your computer, so turn this on only for settings you trust.", Default: false,
				Env: []string{"ICEBATH_ACCEPT_HOOKS"}},
		},
	}})
}

// Fields of one hook, as an event's list of hooks holds them
func hookFields() []config.Setting {
	return []config.Setting{
		{Key: Command, Title: "Command", About: "The command that runs.", Help: "It reads what happened as JSON on its input.", Default: "", Need: true},
		{Key: Matcher, Title: "Tools", About: "The tools the command runs for, as a pattern of their names.",
			Help: "Such as terminal|write_file. Only the events before and after a tool runs use it.", Default: "", Zero: "Every tool", Picks: PickTools},
		{Key: Timeout, Title: "Time limit", About: "How long the command may run.", Default: 60, Zero: "1 minute", Counts: "seconds",
			Least: config.Num(1), Most: config.Num(300)},
		{Key: Block, Title: "Block when it fails", About: "Block the tool call when the command fails or runs out of time.",
			Help: "Only the event before a tool runs can block.", Default: false},
	}
}

// Fields of one webhook, each a row of its page
func webhookFields() []config.Setting {
	return []config.Setting{
		{Key: URL, Title: "Address", About: "The web address that events are posted to.", Help: "Such as https://example.com/hooks.", Default: "",
			Need: true, Starts: []string{"https://", "http://"}},
		{Key: Posted, Title: "Events", About: "The events that are posted to the address.", Default: []string{}, Options: pointOptions, Need: true},
		{Key: Name, Title: "Name", About: "The name the webhook is listed by.", Default: "", Zero: "Its address"},
		{Key: Matcher, Title: "Tools", About: "The tools it posts for, as a pattern of their names.",
			Help: "Such as terminal|write_file. Only the events before and after a tool runs use it.", Default: "", Zero: "Every tool", Picks: PickTools},
		{Key: Timeout, Title: "Time limit", About: "How long one post may take.", Default: 10, Zero: "10 seconds", Counts: "seconds",
			Least: config.Num(1), Most: config.Num(60)},
		{Key: Secret, Title: "Secret", About: "The secret that posts are signed with.",
			Help: "The address can check each post with it. A secret in Secret variable wins over this one.", Default: "", Secret: true},
		{Key: SecretEnv, Title: "Secret variable", About: "The environment variable that holds the secret that posts are signed with.",
			Help: "Such as WEBHOOK_SECRET.", Default: "", Zero: "None"},
	}
}
