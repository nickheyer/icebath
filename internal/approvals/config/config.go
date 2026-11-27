package config

const (
	Key = "approvals"
	// Whether the agent asks before risky commands: manual, smart or off
	Mode = Key + ".mode"
	// Commands, command globs and kinds of risky action that run without asking
	Allowlist = Key + ".allowlist"
	// Command globs blocked always, even in yolo or with approvals off
	Deny = Key + ".deny"
	// Seconds a prompt waits for an answer before it is denied
	Timeout = Key + ".timeout"
	// Denials of the smart reviewer in a row after which the agent is told to stop trying
	DenialBreaker = Key + ".denial_breaker_threshold"
	// Rules the smart reviewer follows besides its own
	SmartPolicy = Key + ".smart_policy"
	// What risky commands do where nobody watches: in scheduled jobs, one-shot runs and unattended surfaces
	CronMode        = Key + ".cron_mode"
	SingleQueryMode = Key + ".single_query_mode"
	UnattendedMode  = Key + ".unattended_mode"
	// Ask before slash commands that delete or reset things
	DestructiveConfirm = Key + ".destructive_slash_confirm"
	// Channel prompts go through, and what a failed one falls back to
	Transport         = Key + ".transport"
	TransportFallback = Key + ".transport_fallback"
)

// Values of the approval mode
const (
	Manual = "manual"
	Smart  = "smart"
	Off    = "off"
)

// Values of the modes where nobody watches
const (
	Refuse = "deny"
	Allow  = "approve"
)

// Transport of the prompt in the chat itself, and the fallback to it
const Builtin = "builtin"

// Helper task smart approvals run on
const Task = "approval"

// Variable that turns the confirmation of destructive slash commands off when it is true
const NoConfirmEnv = "ICEBATH_TUI_NO_CONFIRM"
