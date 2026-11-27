package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config, transports func() []config.Option) {
	unattended := config.Options(
		config.Option{Value: Refuse, Title: "Deny", About: "Block commands that would need approval"},
		config.Option{Value: Allow, Title: "Approve", About: "Allow them without asking"},
	)
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Approvals",
		About: "When the agent asks before risky commands, and what it may run without asking",
		Settings: []config.Setting{
			{Key: Mode, Title: "Approvals", About: "Whether the agent asks before running risky commands; /yolo skips asking for one session without changing this",
				Default: Smart, Options: config.Options(
					config.Option{Value: Manual, Title: "Always ask", About: "Ask before every risky command"},
					config.Option{Value: Smart, Title: "Smart", About: "A quick check lets clearly safe ones through"},
					config.Option{Value: Off, Title: "Never ask", About: "Run everything without asking"},
				)},
			{Key: Allowlist, Title: "Allowed commands", About: "Commands you chose to always allow, which run without asking: exact commands, globs such as `git push *`, or kinds of risky action", Default: []string{}},
			{Key: DestructiveConfirm, Title: "Confirm destructive", About: "Ask before slash commands that delete or reset things, such as /undo", Default: true},
			{Key: Timeout, Title: "Approval wait", About: "Seconds an approval prompt waits before it is denied", Default: 5 * time.Minute},
			{Key: Deny, Title: "Always-deny patterns", About: "Command patterns always blocked, even in yolo mode; matched as globs against the command text", Default: []string{}},
			{Key: DenialBreaker, Title: "Denial limit", About: "Tell the agent to stop after this many smart approval denials in a row; zero or less turns the limit off", Default: 3},
			{Key: SmartPolicy, Title: "Smart approval rules", About: "Extra rules the approval reviewer model follows, free text added to its instructions", Default: ""},
			{Key: CronMode, Title: "Scheduled job approvals", About: "How risky commands are treated in scheduled jobs; approve runs them with nobody watching", Default: Refuse, Options: unattended},
			{Key: SingleQueryMode, Title: "One-shot approvals", About: "How risky commands are treated in one-shot runs; approve runs them with nobody watching", Default: Refuse, Options: unattended},
			{Key: UnattendedMode, Title: "Unattended approvals", About: "How risky commands are treated on surfaces nobody answers, such as webhooks; approve runs them with nobody watching", Default: Refuse, Options: unattended},
			{Key: Transport, Title: "Approval channel", About: "Which channel delivers approval prompts: built-in asks in the chat, a plugin's sends them through it", Default: Builtin,
				Options: func() []config.Option {
					return append([]config.Option{{Value: Builtin, Title: "Built-in", About: "The prompt in the chat"}}, transports()...)
				}},
			{Key: TransportFallback, Title: "Approval fallback", About: "What happens when the chosen approval channel fails", Default: Refuse, Options: config.Options(
				config.Option{Value: Refuse, Title: "Deny", About: "Block the command"},
				config.Option{Value: Builtin, Title: "Built-in", About: "Fall back to the prompt in the chat"},
			)},
		},
	}})
}
