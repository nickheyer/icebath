package app

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Monotonic seconds the modal deadlines count against
var monotonic = pythread.Monotonic

// Wait between response checks of a blocking modal prompt
var modalPollInterval = time.Second

// Approval result to the catalog key of the outcome label in the persisted prompt summary
var approvalOutcomeKeys = map[string]string{
	"once": "cli.approval.outcome_once", "session": "cli.approval.outcome_session",
	"always": "cli.approval.outcome_always", "deny": "cli.approval.outcome_deny"}

// Localized outcome label of an approval result, the result itself when unknown
func approvalOutcomeLabel(ctx context.Context, result string) string {
	if key, ok := approvalOutcomeKeys[result]; ok {
		return i18n.T(ctx, key, "", nil)
	}
	return result
}

// Sudo panel state, shared by the vault unlock, save login and verification code prompts
type sudoModalState struct {
	VaultCode *vaultCodePrompt
	response  chan string
}

// Site and hint of a one time code prompt
type vaultCodePrompt struct{ Site, Hint string }

// Dangerous command approval panel state
type approvalModalState struct {
	Command, Description string
	Choices              []string
	Selected             int
	response             chan string
}

// Clarify panel state, the batch of questions with locked answers and the flat view of the active one
type clarifyModalState struct {
	Questions   []clarifyQuestion
	Answers     map[string]*string
	Active      int
	Question    string
	Choices     []string
	Selected    int
	MultiSelect bool
	response    chan map[string]any
}

// Delivers a modal answer without blocking, a missing or full channel dropping it like a dead queue
func offer[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

// Monotonic deadline from the context, else seconds from now
func deadlineAfter(ctx context.Context, seconds float64) float64 {
	if dl, ok := ctx.Deadline(); ok {
		return monotonic() + time.Until(dl).Seconds()
	}
	return monotonic() + seconds
}

// Blocks until an answer lands or the deadline passes, a cancelled context answering onCancel
func pollModalQueue[T any](ctx context.Context, response <-chan T, deadline func() (float64, bool), onCancel T) (T, bool) {
	var zero T
	timer := time.NewTimer(modalPollInterval)
	defer timer.Stop()
	for {
		select {
		case v := <-response:
			return v, false
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return zero, true
			}
			return onCancel, false
		case <-timer.C:
		}
		if d, ok := deadline(); ok && d-monotonic() <= 0 {
			return zero, true
		}
		timer.Reset(modalPollInterval)
	}
}

// Installs or clears the sudo panel with its deadline
func (c *CLI) setSudoModal(state *sudoModalState, deadline float64) {
	c.uiMu.Lock()
	c.sudoState, c.sudoDeadline = state, deadline
	c.uiMu.Unlock()
}

// Installs or clears the approval panel with its deadline
func (c *CLI) setApprovalModal(state *approvalModalState, deadline float64) {
	c.uiMu.Lock()
	c.approvalState, c.approvalDeadline = state, deadline
	c.uiMu.Unlock()
}

// Sudo panel deadline, always set while the panel polls
func (c *CLI) sudoDeadlineAt() (float64, bool) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.sudoDeadline, true
}

// Approval panel deadline, always set while the panel polls
func (c *CLI) approvalDeadlineAt() (float64, bool) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.approvalDeadline, true
}

// Prints a dim indented localized line through the chat printer
func (c *CLI) dimNote(key string, kwargs map[string]any) {
	cprint("\n" + dimANSI + "  " + i18n.T(c.ctx, key, "", kwargs) + resetANSI)
}

// Prompts for a sudo password on the sudo panel, empty on skip or timeout
func (c *CLI) sudoPasswordCallback(ctx context.Context) string {
	response := make(chan string, 1)
	c.setSudoModal(&sudoModalState{response: response}, deadlineAfter(ctx, 45))
	c.ringBell(true, i18n.T(c.ctx, "cli.secret.bell_sudo", "", nil), "")
	result, timedOut := pollModalQueue(ctx, response, c.sudoDeadlineAt, "")
	c.setSudoModal(nil, 0)
	switch {
	case timedOut:
		c.dimNote("cli.secret.sudo_timeout", nil)
		return ""
	case result != "":
		c.dimNote("cli.secret.sudo_received", nil)
	default:
		c.dimNote("cli.secret.sudo_skipped", nil)
	}
	return result
}

// Dangerous command approval through the approval panel, once, session, always, deny or timeout
func (c *CLI) approvalCallback(ctx context.Context, command, description string, allowPermanent, allowSession, smartDenied bool) (string, error) {
	c.approvalLock.Lock()
	defer c.approvalLock.Unlock()
	raw := mapValueOr(cliMap(CLIConfig(c.ctx), "approvals"), "timeout", 300)
	timeout, ok := pyIntOf(raw)
	if !ok {
		return "", fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(raw))
	}
	response := make(chan string, 1)
	c.setApprovalModal(&approvalModalState{Command: command, Description: description,
		Choices: c.approvalChoices(command, allowPermanent, allowSession, smartDenied), response: response}, deadlineAfter(ctx, float64(timeout)))
	c.ringBell(true, i18n.T(c.ctx, "cli.approval.bell_context", "", nil), command)
	result, timedOut := pollModalQueue(ctx, response, c.approvalDeadlineAt, "deny")
	c.setApprovalModal(nil, 0)
	label := i18n.T(c.ctx, "cli.approval.label", "", nil)
	if timedOut {
		c.dimNote("cli.approval.timeout_denying", nil)
		c.persistPromptSummary("⚠", label, command, i18n.T(c.ctx, "cli.approval.timed_out_no_response", "", nil))
		return "timeout", nil
	}
	c.persistPromptSummary("⚠", label, command, approvalOutcomeLabel(c.ctx, result))
	return result, nil
}

// Choices offered for a command, once and deny only under smart deny or a re ask every time gate
func (c *CLI) approvalChoices(command string, allowPermanent, allowSession, smartDenied bool) []string {
	var choices []string
	switch {
	case smartDenied || !allowSession:
		choices = []string{"once", "deny"}
	case allowPermanent:
		choices = []string{"once", "session", "always", "deny"}
	default:
		choices = []string{"once", "session", "deny"}
	}
	if utf8.RuneCountInString(command) > 70 {
		choices = append(choices, "view")
	}
	return choices
}

// Masked master password prompt for an external password manager on the sudo panel, empty when it stays locked
func (c *CLI) vaultUnlockCallback(backendName, displayName string) string {
	response := make(chan string, 1)
	c.setSudoModal(&sudoModalState{response: response}, monotonic()+120)
	c.ringBell(true, i18n.T(c.ctx, "cli.secret.bell_unlock", "", map[string]any{"name": displayName}), "")
	result, timedOut := pollModalQueue(c.ctx, response, c.sudoDeadlineAt, "")
	c.setSudoModal(nil, 0)
	if timedOut || result == "" {
		c.dimNote("cli.secret.stays_locked", map[string]any{"name": displayName})
		return ""
	}
	c.dimNote("cli.secret.unlocking_for_session", map[string]any{"name": displayName})
	return result
}

// Two step save this login prompt on the sudo panel, identifier shown then password masked, nil when declined
func (c *CLI) vaultSaveLoginCallback(origin, site string) map[string]string {
	answer := map[string]string{}
	for _, step := range []string{"identifier", "password"} {
		response := make(chan string, 1)
		c.setSudoModal(&sudoModalState{response: response}, monotonic()+180)
		if step == "identifier" {
			c.ringBell(true, i18n.T(c.ctx, "cli.secret.bell_save_login", "", map[string]any{"site": site}), "")
		}
		result, timedOut := pollModalQueue(c.ctx, response, c.sudoDeadlineAt, "")
		c.setSudoModal(nil, 0)
		if timedOut || result == "" {
			c.dimNote("cli.secret.login_not_saved", map[string]any{"site": site})
			return nil
		}
		answer[step] = result
	}
	c.dimNote("cli.secret.login_saved", map[string]any{"site": site})
	return answer
}

// One time code prompt shown as typed on the sudo panel, empty when declined or timed out
func (c *CLI) vaultCodeCallback(site, hint string) string {
	response := make(chan string, 1)
	c.setSudoModal(&sudoModalState{VaultCode: &vaultCodePrompt{Site: site, Hint: hint}, response: response}, monotonic()+180)
	c.ringBell(true, i18n.T(c.ctx, "cli.secret.bell_verification_code", "", map[string]any{"site": site}), "")
	result, timedOut := pollModalQueue(c.ctx, response, c.sudoDeadlineAt, "")
	c.setSudoModal(nil, 0)
	if timedOut || result == "" {
		c.dimNote("cli.secret.code_not_entered", map[string]any{"site": site})
		return ""
	}
	c.dimNote("cli.secret.code_entered", map[string]any{"site": site})
	return result
}

// Captures a secret for a skill through the secret panel, restoring the draft afterwards
func (c *CLI) secretCaptureCallback(varName, prompt string, metadata map[string]any) (map[string]any, error) {
	return promptForSecret(c, varName, prompt, metadata)
}

// Rings the terminal bell and sends the OS notification when the display flag allows
func (c *CLI) ringBell(prompt bool, text, detail string) {
	c.uiMu.Lock()
	flag := c.bellOnComplete
	if prompt {
		flag = c.bellOnPrompt
	}
	c.uiMu.Unlock()
	if !flag {
		return
	}
	body := text
	if body == "" {
		key := "cli.modal.bell_turn_complete"
		if prompt {
			key = "cli.modal.bell_input_needed"
		}
		body = i18n.T(c.ctx, key, "", nil)
	}
	notify, err := notificationSequence(body, prompt, c.sessionID, detail)
	if err != nil {
		return
	}
	writeTTY("\a" + notify)
}

// Drains and clears every input blocking overlay an interrupted agent left behind
func (c *CLI) clearActiveOverlaysForInterrupt() {
	c.uiMu.Lock()
	if st := c.approvalState; st != nil {
		offer(st.response, "deny")
		c.approvalState = nil
	}
	connection := c.connectionState
	c.uiMu.Unlock()
	if connection != nil {
		c.connectionInterrupt()
	}
	c.uiMu.Lock()
	if st := c.clarifyState; st != nil {
		offer[map[string]any](st.response, nil)
		c.clarifyState = nil
		c.clarifyFreetext = false
	}
	if st := c.sudoState; st != nil {
		offer(st.response, "")
		c.sudoState = nil
		c.sudoDeadline = 0
	}
	c.uiMu.Unlock()
}

// Clears the clarify panel state and repaints
func (c *CLI) clarifyTeardown() {
	c.uiMu.Lock()
	c.clarifyState, c.clarifyFreetext, c.clarifyDeadline = nil, false, nil
	c.uiMu.Unlock()
}
