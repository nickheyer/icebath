package tuigateway

// One shell hook as the hooks screen shows it: the event it runs on, its script and whether it may run
type HookRow struct {
	Event   string `json:"event"`
	Command string `json:"command"`
	// Pattern of the tools it is for, empty for every tool or an event that is not about tools
	Matcher string `json:"matcher,omitzero"`
	// Seconds its script may take
	Timeout int64 `json:"timeout"`
	// A failure of the script blocks the action it was asked about
	FailClosed bool `json:"fail_closed,omitzero"`
	Allowed    bool `json:"allowed,omitzero"`
	// When it was allowed, empty when not known
	ApprovedAt string `json:"approved_at,omitzero"`
	// The script changed since it was allowed, with its time then and now
	Changed    bool   `json:"changed,omitzero"`
	ChangedWas string `json:"changed_was,omitzero"`
	ChangedNow string `json:"changed_now,omitzero"`
}

// One address events are posted to
type WebhookRow struct {
	Label   string   `json:"label"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Matcher string   `json:"matcher,omitzero"`
	Timeout int64    `json:"timeout"`
	Signed  bool     `json:"signed,omitzero"`
}

type HooksListParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Every shell hook and outbound webhook of the profile, with the events a new hook may be set on
type HooksListResult struct {
	Hooks    []HookRow    `json:"hooks"`
	Webhooks []WebhookRow `json:"webhooks"`
	// Events a hook may run on
	Events []string `json:"events"`
}

// One change to the hooks: add, remove, allow or revoke
type HooksChangeParams struct {
	Profile *string `json:"profile,omitzero"`
	Verb    string  `json:"verb"`
	Event   string  `json:"event"`
	Command string  `json:"command"`
	// Tools a new hook is for and the seconds it may take, both left to their defaults when empty
	Matcher string `json:"matcher,omitzero"`
	Timeout int64  `json:"timeout,omitzero"`
}

// One hook to run on a sample event, naming the tool when one is given
type HooksTestParams struct {
	Profile *string `json:"profile,omitzero"`
	Event   string  `json:"event"`
	Command string  `json:"command"`
	Tool    string  `json:"tool,omitzero"`
}

// How one run of a hook's script ended
type HookRun struct {
	// Why the script could not run, empty when it ran
	Error    string `json:"error,omitzero"`
	TimedOut bool   `json:"timed_out,omitzero"`
	// Exit code of a script that ran to its end
	Exited   bool    `json:"exited,omitzero"`
	ExitCode int64   `json:"exit_code,omitzero"`
	Seconds  float64 `json:"seconds"`
	Stdout   string  `json:"stdout,omitzero"`
	Stderr   string  `json:"stderr,omitzero"`
	// What the script answered in the shape icebath acts on, empty when it answered nothing
	Directive string `json:"directive,omitzero"`
}

// One hook to check
type HooksCheckParams struct {
	Profile *string `json:"profile,omitzero"`
	Event   string  `json:"event"`
	Command string  `json:"command"`
}

// What a check of one hook found
type HooksCheckResult struct {
	Executable bool `json:"executable,omitzero"`
	Allowed    bool `json:"allowed,omitzero"`
	// When it was allowed, a question mark when not known
	ApprovedAt string `json:"approved_at,omitzero"`
	// Whether the script could be compared with the one that was allowed, and whether it changed
	Compared   bool   `json:"compared,omitzero"`
	Changed    bool   `json:"changed,omitzero"`
	ChangedWas string `json:"changed_was,omitzero"`
	ChangedNow string `json:"changed_now,omitzero"`
	// Run on a sample event, absent for a hook that is not allowed or cannot run
	Run *HookRun `json:"run,omitzero"`
	// What a finished run printed: empty, json or invalid
	Output   string `json:"output,omitzero"`
	Problems int64  `json:"problems"`
}

// Shell hooks and outbound webhooks of the profile for the hooks screen
var HooksList = NewMethod[HooksListParams, HooksListResult]("hooks.list")

// One change to the hooks, answered with the hooks after it
var HooksChange = NewMethod[HooksChangeParams, HooksListResult]("hooks.change")

// Runs one hook on a sample event
var HooksTest = NewMethod[HooksTestParams, HookRun]("hooks.test")

// Checks one hook: whether its script can run, is allowed, is unchanged and answers well
var HooksCheck = NewMethod[HooksCheckParams, HooksCheckResult]("hooks.check")
