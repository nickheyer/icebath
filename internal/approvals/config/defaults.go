package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config, transports func() []config.Option) {
	unattended := config.Options(
		config.Option{Value: Refuse, Title: "Deny", About: "Block every command that needs approval"},
		config.Option{Value: Allow, Title: "Approve", About: "Run those commands with nobody watching"},
	)
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Approvals",
		About: "When the agent must ask before it runs a command",
		Group: config.GroupSafety,
		Order: 10,
		Settings: []config.Setting{
			{Key: Mode, Title: "When to ask", About: "Whether the agent asks you before it runs a risky command.", Default: Smart, Slash: "approvals",
				Options: config.Options(
					config.Option{Value: Manual, Title: "Always ask", About: "Ask before every risky command"},
					config.Option{Value: Smart, Title: "Smart", About: "A helper model lets clearly safe commands through"},
					config.Option{Value: Off, Title: "Never ask", About: "Run every command without asking"},
				)},
			{Key: Allowlist, Title: "Allowed without asking", About: "Commands and kinds of action that run without asking.",
				Help: "An exact command, a pattern such as git push * where * stands for anything, or a kind of risky action.", Default: []string{},
				Custom: true, Entry: &config.Entry{Noun: "command or pattern"}},
			{Key: Deny, Title: "Blocked commands", About: "Commands that never run, even when asking is off.",
				Help: "A pattern such as rm -rf * is matched against the whole command.", Default: []string{},
				Entry: &config.Entry{Noun: "command or pattern"}},
			{Key: DestructiveConfirm, Title: "Confirm before deleting", About: "Ask before commands that delete or reset things, such as /undo.",
				Help: "The variable ICEBATH_TUI_NO_CONFIRM set to true turns asking off whatever this says.", Default: true},
			{Key: Timeout, Title: "Time to answer", About: "How long icebath waits for your answer before it denies the command.",
				Default: 5 * time.Minute, Zero: "Denied at once", Min: config.Bound(0)},
			{Key: SmartPolicy, Title: "Smart check rules", About: "Rules of your own for the helper model that checks risky commands.",
				Help: "Free text added to the instructions of that model.", Default: "", Advanced: true},
			{Key: DenialBreaker, Title: "Denials before stopping", About: "How many smart denials in a row make the agent stop trying.", Default: 3,
				Zero: "No limit", Counts: "denials", Advanced: true},
			{Key: CronMode, Title: "In scheduled jobs", About: "What happens to risky commands in scheduled jobs, where nobody can answer.",
				Default: Refuse, Options: unattended, Advanced: true},
			{Key: SingleQueryMode, Title: "In single-question runs", About: "What happens to risky commands in a single-question run of icebath chat -q.",
				Default: Refuse, Options: unattended, Advanced: true},
			{Key: UnattendedMode, Title: "Where nobody answers", About: "What happens to risky commands in places nobody answers, such as webhooks.",
				Default: Refuse, Options: unattended, Advanced: true},
			{Key: Transport, Title: "Where you are asked", About: "Where icebath asks you to approve a command.", Default: Builtin, Advanced: true,
				Options: func() []config.Option {
					return append([]config.Option{{Value: Builtin, Title: "In the chat", About: "You are asked in the chat"}}, transports()...)
				}},
			{Key: TransportFallback, Title: "If asking fails", About: "What happens when asking in the chosen place fails.", Default: Refuse,
				Advanced: true, Options: config.Options(
					config.Option{Value: Refuse, Title: "Deny", About: "Block the command"},
					config.Option{Value: Builtin, Title: "Ask in the chat", About: "Ask in the chat instead"},
				)},
		},
	}})
}
