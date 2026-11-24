package tuigateway

// One allowed commands entry the approval history proposes, a command pattern or a whole class of risky command
type ApprovalProposal struct {
	Pattern string `json:"pattern"`
	// glob for a command pattern, class for every command of one risky class
	Kind string `json:"kind"`
	// How often a person approved a command the entry covers
	Count    int64    `json:"count"`
	Classes  []string `json:"classes,omitzero"`
	Examples []string `json:"examples,omitzero"`
}

// How far back the approval history is read and how much it proposes
type ApprovalsSuggestParams struct {
	Profile *string `json:"profile,omitzero"`
	// Days of history read, 90 when absent and every day for zero
	Days *int64 `json:"days,omitzero"`
	// Approvals an entry needs before it is proposed, 2 when absent
	MinCount int64 `json:"min_count,omitzero"`
	// Most entries proposed, 20 when absent
	Limit int64 `json:"limit,omitzero"`
}

// Entries the approval history proposes, the most approved first
type ApprovalsSuggestResult struct {
	Days      int64              `json:"days"`
	Proposals []ApprovalProposal `json:"proposals"`
	// Entries the allowed commands hold now
	Allowed int64 `json:"allowed"`
}

// Patterns to add to the allowed commands
type ApprovalsAllowParams struct {
	Profile  *string  `json:"profile,omitzero"`
	Patterns []string `json:"patterns"`
}

// Patterns added and the size of the allowed commands after them
type ApprovalsAllowResult struct {
	Added   []string `json:"added"`
	Allowed int64    `json:"allowed"`
}

// Command whose approval verdict is asked for, never run
type ApprovalsTestParams struct {
	Profile *string `json:"profile,omitzero"`
	Command string  `json:"command"`
	// Where the command would run, the profile's own place when empty
	EnvType string `json:"env_type,omitzero"`
}

type ApprovalVerdict string

func (ApprovalVerdict) Values() []string {
	return []string{"allow", "ask-approval", "hardline-deny", "user-deny"}
}

// What the approval rules do with the command, the rule that decided and every spelling of it the rules read
type ApprovalsTestResult struct {
	Command  string          `json:"command"`
	EnvType  string          `json:"env_type"`
	Verdict  ApprovalVerdict `json:"verdict"`
	Rule     string          `json:"rule,omitzero"`
	Detail   string          `json:"detail,omitzero"`
	Variants []string        `json:"variants,omitzero"`
}

var (
	// Allowed commands entries proposed from the approvals people gave
	ApprovalsSuggest = NewMethod[ApprovalsSuggestParams, ApprovalsSuggestResult]("approvals.suggest")
	// Adds patterns to the allowed commands
	ApprovalsAllow = NewMethod[ApprovalsAllowParams, ApprovalsAllowResult]("approvals.allow")
	// Verdict the approval rules give a command, without running it
	ApprovalsTest = NewMethod[ApprovalsTestParams, ApprovalsTestResult]("approvals.test")
)
