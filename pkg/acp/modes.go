package acp

const (
	DefaultMode                = "default"
	DefaultEditApprovalPolicy  = "ask"
	EditApprovalPolicyConfigID = "edit_approval_policy"
)

// Edit approval policy, display name and description of one ACP mode
type ModeSpec struct {
	Policy      string
	Name        string
	Description string
}

// Mode ids in table order
var ModeOrder = []string{"default", "accept_edits", "dont_ask"}

// Mode id to its edit approval policy, display name and description
var Modes = map[string]ModeSpec{
	"default":      {"ask", "Default", "Ask before edits."},
	"accept_edits": {"workspace_session", "Accept Edits", "Auto-allow workspace and temp-dir edits; still asks for sensitive paths."},
	"dont_ask":     {"session", "Don't Ask", "Auto-allow file edits for this session except sensitive paths."},
}

// Mode id to edit approval policy
var ModeToEditApprovalPolicy = map[string]string{
	"default":      "ask",
	"accept_edits": "workspace_session",
	"dont_ask":     "session",
}

// Edit approval policy to mode id
var EditApprovalPolicyToMode = map[string]string{
	"ask":               "default",
	"workspace_session": "accept_edits",
	"session":           "dont_ask",
}
