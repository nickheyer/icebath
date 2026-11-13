package tui

import tea "charm.land/bubbletea/v2"

// Dangerous command waiting for the user's verdict
type approvalReq struct {
	Shared         *sharedControl
	AllowPermanent bool
	Choices        []string
	Command        string
	Description    string
	RequestID      string
	SmartDenied    bool
}

// Yes or no question with its labels and the action a yes runs
type confirmReq struct {
	CancelLabel  string
	ConfirmLabel string
	Danger       bool
	Detail       string
	OnConfirm    func() tea.Cmd
	Title        string
}

// One clarify question, nil choices for free text
type clarifyQuestion struct {
	Choices     []string
	MultiSelect bool
	QID         string
	Question    string
}

// Clarify prompt with the answers the owner already locked
type clarifyReq struct {
	Shared    *sharedControl
	RequestID string
	Questions []clarifyQuestion
	Answers   map[string]string
}

// Masked sudo password prompt
type sudoReq struct{ RequestID string }

// Masked secret prompt for one environment variable
type secretReq struct {
	EnvVar    string
	Prompt    string
	RequestID string
}

// Master password prompt of an external password manager
type vaultUnlockReq struct {
	Backend     string
	DisplayName string
	RequestID   string
}

// Website login capture for the encrypted vault
type vaultSaveLoginReq struct {
	Origin    string
	RequestID string
	Site      string
}

// One time sign in code the user reads from a device
type vaultCodeReq struct {
	Hint      string
	RequestID string
	Site      string
}

// Connection card of the live connection operation
type connectionOverlay struct{ OpID string }

// Overlays and prompt cards open over the composer
type overlayState struct {
	Agents         bool
	Approval       *approvalReq
	Clarify        *clarifyReq
	Confirm        *confirmReq
	Connection     *connectionOverlay
	Secret         *secretReq
	Sessions       bool
	Sudo           *sudoReq
	VaultCode      *vaultCodeReq
	VaultSaveLogin *vaultSaveLoginReq
	VaultUnlock    *vaultUnlockReq
}

// Drops the flow prompts of a finished turn and keeps the panels the user opened
func (s *tuiStore) resetFlowOverlays() {
	s.overlay = overlayState{Agents: s.overlay.Agents, Connection: s.overlay.Connection, Sessions: s.overlay.Sessions}
}

// Request id of an open prompt card, empty when the card is closed
func (o *overlayState) promptRequestID(key string) string {
	switch key {
	case "approval":
		if o.Approval != nil {
			return o.Approval.RequestID
		}
	case "clarify":
		if o.Clarify != nil {
			return o.Clarify.RequestID
		}
	case "sudo":
		if o.Sudo != nil {
			return o.Sudo.RequestID
		}
	case "secret":
		if o.Secret != nil {
			return o.Secret.RequestID
		}
	case "vaultUnlock":
		if o.VaultUnlock != nil {
			return o.VaultUnlock.RequestID
		}
	case "vaultSaveLogin":
		if o.VaultSaveLogin != nil {
			return o.VaultSaveLogin.RequestID
		}
	case "vaultCode":
		if o.VaultCode != nil {
			return o.VaultCode.RequestID
		}
	}
	return ""
}

// Closes the prompt card a key names
func (o *overlayState) closePrompt(key string) {
	switch key {
	case "approval":
		o.Approval = nil
	case "clarify":
		o.Clarify = nil
	case "sudo":
		o.Sudo = nil
	case "secret":
		o.Secret = nil
	case "vaultUnlock":
		o.VaultUnlock = nil
	case "vaultSaveLogin":
		o.VaultSaveLogin = nil
	case "vaultCode":
		o.VaultCode = nil
	}
}

// Credential prompts that Esc and Ctrl+C decline
var sensitivePrompts = []string{"sudo", "secret", "vaultUnlock", "vaultSaveLogin", "vaultCode"}
