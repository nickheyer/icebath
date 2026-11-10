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

// Draft text and cursor saved while a modal prompt owns the input buffer
type modalInputSnapshot struct {
	Text   string
	Cursor int
}

// Sudo panel state, shared by the vault unlock, save login and verification code prompts
type sudoModalState struct {
	VaultBackend string
	VaultSave    *vaultSavePrompt
	VaultCode    *vaultCodePrompt
	response     chan string
}

// Site, origin and step of a two step save login prompt
type vaultSavePrompt struct{ Site, Origin, Step string }

// Site and hint of a one time code prompt
type vaultCodePrompt struct{ Site, Hint string }

// Dangerous command approval panel state
type approvalModalState struct {
	Command, Description string
	Choices              []string
	Selected             int
	ShowFull             bool
	response             chan string
}

// Secret capture panel state
type secretModalState struct {
	VarName, Prompt string
	Metadata        map[string]any
	response        chan string
}

// Clarify panel state, the batch of questions with locked answers and the flat view of the active one
type clarifyModalState struct {
	Questions       []clarifyQuestion
	Answers         map[string]*string
	AnswerMeta      map[string]clarifyAnswerMeta
	Active          int
	Question        string
	Choices         []string
	Selected        int
	MultiSelect     bool
	SelectedIndices map[int]bool
	response        chan map[string]any
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

// Blocks until an answer lands or the deadline passes, repainting every refresh seconds, a cancelled context answering onCancel
func pollModalQueue[T any](c *CLI, ctx context.Context, response <-chan T, deadline func() (float64, bool), refresh float64, paint func(), onCancel T) (T, bool) {
	var zero T
	if paint == nil {
		paint = c.paintNow
	}
	last := monotonic()
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
		if now := monotonic(); now-last >= refresh {
			last = now
			paint()
		}
		timer.Reset(modalPollInterval)
	}
}

// Current sudo panel state
func (c *CLI) sudoModal() *sudoModalState {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.sudoState
}

// Current approval panel state
func (c *CLI) approvalModal() *approvalModalState {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.approvalState
}

// Current secret panel state
func (c *CLI) secretModal() *secretModalState {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.secretState
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
	c.captureModalInputSnapshot()
	c.setSudoModal(&sudoModalState{response: response}, deadlineAfter(ctx, 45))
	c.ringBell(true, i18n.T(c.ctx, "cli.secret.bell_sudo", "", nil), "")
	c.paintNow()
	result, timedOut := pollModalQueue(c, ctx, response, c.sudoDeadlineAt, 0, nil, "")
	c.setSudoModal(nil, 0)
	c.restoreModalInputSnapshot()
	c.paintNow()
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
	c.paintNow()
	result, timedOut := pollModalQueue(c, ctx, response, c.approvalDeadlineAt, 1.0, nil, "deny")
	c.setApprovalModal(nil, 0)
	c.paintNow()
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

// Processes the selected approval choice, view expanding the command in place
func (c *CLI) handleApprovalSelection() {
	c.uiMu.Lock()
	state := c.approvalState
	if state == nil || state.Selected < 0 || state.Selected >= len(state.Choices) {
		c.uiMu.Unlock()
		return
	}
	chosen := state.Choices[state.Selected]
	if chosen == "view" {
		state.ShowFull = true
		kept := make([]string, 0, len(state.Choices))
		for _, choice := range state.Choices {
			if choice != "view" {
				kept = append(kept, choice)
			}
		}
		state.Choices = kept
		if state.Selected >= len(kept) {
			state.Selected = max(0, len(kept)-1)
		}
		c.uiMu.Unlock()
		_ = c.invalidate()
		return
	}
	offer(state.response, chosen)
	c.approvalState = nil
	c.uiMu.Unlock()
	_ = c.invalidate()
}

// Masked master password prompt for an external password manager on the sudo panel, empty when it stays locked
func (c *CLI) vaultUnlockCallback(backendName, displayName string) string {
	response := make(chan string, 1)
	c.captureModalInputSnapshot()
	c.setSudoModal(&sudoModalState{VaultBackend: displayName, response: response}, monotonic()+120)
	c.ringBell(true, i18n.T(c.ctx, "cli.secret.bell_unlock", "", map[string]any{"name": displayName}), "")
	c.paintNow()
	result, timedOut := pollModalQueue(c, c.ctx, response, c.sudoDeadlineAt, 0, nil, "")
	c.setSudoModal(nil, 0)
	c.restoreModalInputSnapshot()
	c.paintNow()
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
		c.captureModalInputSnapshot()
		c.setSudoModal(&sudoModalState{VaultSave: &vaultSavePrompt{Site: site, Origin: origin, Step: step}, response: response}, monotonic()+180)
		if step == "identifier" {
			c.ringBell(true, i18n.T(c.ctx, "cli.secret.bell_save_login", "", map[string]any{"site": site}), "")
		}
		c.paintNow()
		result, timedOut := pollModalQueue(c, c.ctx, response, c.sudoDeadlineAt, 0, nil, "")
		c.setSudoModal(nil, 0)
		c.restoreModalInputSnapshot()
		c.paintNow()
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
	c.captureModalInputSnapshot()
	c.setSudoModal(&sudoModalState{VaultCode: &vaultCodePrompt{Site: site, Hint: hint}, response: response}, monotonic()+180)
	c.ringBell(true, i18n.T(c.ctx, "cli.secret.bell_verification_code", "", map[string]any{"site": site}), "")
	c.paintNow()
	result, timedOut := pollModalQueue(c, c.ctx, response, c.sudoDeadlineAt, 0, nil, "")
	c.setSudoModal(nil, 0)
	c.restoreModalInputSnapshot()
	c.paintNow()
	if timedOut || result == "" {
		c.dimNote("cli.secret.code_not_entered", map[string]any{"site": site})
		return ""
	}
	c.dimNote("cli.secret.code_entered", map[string]any{"site": site})
	return result
}

// Captures a secret for a skill through the secret panel, restoring the draft afterwards
func (c *CLI) secretCaptureCallback(varName, prompt string, metadata map[string]any) (map[string]any, error) {
	c.captureModalInputSnapshot()
	defer func() {
		c.restoreModalInputSnapshot()
		c.paintNow()
	}()
	return promptForSecret(c, varName, prompt, metadata)
}

// Clears the input buffer and saves the user's in progress draft before a modal prompt opens
func (c *CLI) captureModalInputSnapshot() {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	if c.modalInputSnapshot != nil || c.app == nil {
		return
	}
	c.modalInputSnapshot = &modalInputSnapshot{Text: c.app.BufferText(), Cursor: c.app.BufferCursor()}
	c.app.ResetBuffer()
}

// Restores the draft text present before a modal prompt opened
func (c *CLI) restoreModalInputSnapshot() {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	snapshot := c.modalInputSnapshot
	c.modalInputSnapshot = nil
	if snapshot == nil || c.app == nil {
		return
	}
	c.app.SetBuffer(snapshot.Text, min(snapshot.Cursor, utf8.RuneCountInString(snapshot.Text)))
}

// Rings the terminal bell and sends the desktop notification when the display flag allows
func (c *CLI) ringBell(prompt bool, text, detail string) {
	c.uiMu.Lock()
	flag := c.bellOnComplete
	if prompt {
		flag = c.bellOnPrompt
	}
	broken := c.terminalIOBroken
	app := c.app
	c.uiMu.Unlock()
	if !flag || broken {
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
	seq := "\a" + notify
	if app == nil || !app.IsRunning() {
		writeTTY(seq)
		return
	}
	runOnAppLoop(app, func() { _ = writeTerminalSequence(app, seq) })
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
		c.clarifyMultiBase = nil
	}
	restore := false
	if st := c.sudoState; st != nil {
		offer(st.response, "")
		c.sudoState = nil
		c.sudoDeadline = 0
		restore = true
	}
	secret := c.secretState != nil
	c.uiMu.Unlock()
	if restore {
		c.restoreModalInputSnapshot()
	}
	if secret {
		c.cancelSecretCapture()
	}
}

// Answers the secret panel and tears it down with a direct paint
func (c *CLI) submitSecretResponse(value string) {
	c.uiMu.Lock()
	state := c.secretState
	if state == nil {
		c.uiMu.Unlock()
		return
	}
	offer(state.response, value)
	c.secretState, c.secretDeadline = nil, 0
	c.uiMu.Unlock()
	c.paintNow()
}

// Cancels an open secret capture, reporting it skipped
func (c *CLI) cancelSecretCapture() { c.submitSecretResponse("") }

// Resets the composer buffer while the secret panel owns input
func (c *CLI) clearSecretInputBuffer() {
	if app := c.app; app != nil {
		app.ResetBuffer()
	}
}

// Clears the clarify panel state and repaints
func (c *CLI) clarifyTeardown() {
	c.uiMu.Lock()
	c.clarifyState, c.clarifyFreetext, c.clarifyDeadline, c.clarifyMultiBase = nil, false, nil, nil
	c.uiMu.Unlock()
	c.paintNow()
}
