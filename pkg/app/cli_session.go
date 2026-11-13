package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent that flushes its live transcript under its persist lock before the session ends
type closePersister interface {
	PersistActiveSessionBeforeClose(ctx context.Context, readCLIHistory func() []map[string]any) bool
}

var _ closePersister = (*agent.AIAgent)(nil)

// Flushes the agent's in memory turn before the session row ends, adopting the agent's session id afterwards
func (c *CLI) persistActiveSessionBeforeClose() {
	persister, ok := c.agentRef().(closePersister)
	if !ok {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("Could not persist active CLI session before close: %v", r))
		}
	}()
	if !persister.PersistActiveSessionBeforeClose(c.ctx, func() []map[string]any { return c.conversationHistory }) {
		return
	}
	if sid := c.agentRef().SessionID(); sid != "" {
		c.uiMu.Lock()
		c.sessionID = sid
		c.uiMu.Unlock()
		c.writeTerminalBreadcrumb()
	}
}

// Records this terminal's live session so a later bare -c in this terminal resumes it
func (c *CLI) writeTerminalBreadcrumb() {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	WriteBreadcrumb(c.ctx, c.sessionID, cwd)
}

// Collapses whitespace and caps the text at limit cells with an ellipsis
func squash(text string, limit int) string {
	text = strings.Join(strings.FieldsFunc(text, pystr.IsSpace), " ")
	if utf8.RuneCountInString(text) > limit {
		return string([]rune(text)[:limit-1]) + "…"
	}
	return text
}

// Prints a one line scrollback summary of a resolved modal prompt when display.persist_prompts is on
func (c *CLI) persistPromptSummary(icon, label, detail, outcome string) {
	if !cliTruthy(mapValueOr(cliMap(CLIConfig(c.ctx), "display"), "persist_prompts", true)) {
		return
	}
	detail, outcome = squash(detail, 120), squash(outcome, 120)
	cprint("\n" + dimANSI + i18n.T(c.ctx, "cli.session.persist_prompt_summary", "", map[string]any{
		"icon": icon, "label": label, "detail": detail, "outcome": outcome}) + resetANSI)
}

var (
	// Exit screen clear the summary runs first, the real console wipe in production
	clearTerminalOnExit = (*CLI).wipeConsoleOnExit
	// Executable lookup of the exit clear fallback, the PATH search in production
	clearCommandWhich = func(name string) (string, bool) { return pyshutil.Which(name, nil) }
	// Runs the exit clear fallback child, the real process in production
	runClearCommand = (*exec.Cmd).Run
	// Active profile lookup of the exit resume hint, the profile resolver in production
	exitSummaryActiveProfileName = profiles.GetActiveProfileName
)

// Active profile for the resume hint, default when the lookup fails
func exitSummaryProfileName(ctx context.Context) (name string) {
	defer func() {
		if r := recover(); r != nil {
			name = "default"
		}
	}()
	return exitSummaryActiveProfileName(ctx)
}

// Clears the screen and scrollback of a real console, falling back to the platform clear command without a shell
func (c *CLI) wipeConsoleOnExit() {
	con := console()
	if !con.StdoutIsTTY() {
		return
	}
	_, err := io.WriteString(con.Out, "\033[3J\033[2J\033[H")
	if flusher, ok := con.Out.(interface{ Flush() error }); ok && err == nil {
		err = flusher.Flush()
	}
	if err == nil {
		return
	}
	var argv []string
	if IsWindows() {
		argv = []string{"cmd", "/c", "cls"}
	} else if clearBin, ok := clearCommandWhich("clear"); ok && clearBin != "" {
		argv = []string{clearBin}
	}
	if len(argv) == 0 {
		return
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = tools.WindowsHideAttr()
	_ = runClearCommand(cmd)
}

// Prints the session resume commands, title, duration and message counts on exit
func (c *CLI) printExitSummary(clearScreen bool) {
	if clearScreen {
		clearTerminalOnExit(c)
	}
	out := console().Out
	fmt.Fprintln(out)
	for _, message := range takeSessionEndMessages() {
		fmt.Fprintln(out, message)
	}
	msgCount := len(c.conversationHistory)
	if msgCount == 0 {
		fmt.Fprintln(out, config.GetActiveGoodbye(c.ctx, c.t("cli.session.goodbye", nil)))
		return
	}
	userMsgs, toolCalls := 0, 0
	for _, m := range c.conversationHistory {
		if m["role"] == "user" {
			userMsgs++
		}
		if m["role"] == "tool" || cliTruthy(m["tool_calls"]) {
			toolCalls++
		}
	}
	total := int(time.Since(c.sessionStart).Seconds())
	hours, remainder := floorDivMod(total, 3600)
	minutes, seconds := floorDivMod(remainder, 60)
	duration := c.t("cli.shared.duration_s", map[string]any{"seconds": seconds})
	if hours > 0 {
		duration = c.t("cli.shared.duration_hm_rest", map[string]any{"hours": hours, "minutes": minutes, "rest": duration})
	} else if minutes > 0 {
		duration = c.t("cli.shared.duration_m_rest", map[string]any{"minutes": minutes, "rest": duration})
	}
	sessionTitle := ""
	if c.sessionDB != nil {
		if title, _, err := c.sessionDB.GetSessionTitle(c.ctx, c.sessionID); err == nil {
			sessionTitle = title
		}
	}
	fmt.Fprintln(out, c.t("cli.session.exit_resume_hint", nil))
	profileFlag := ""
	if active := exitSummaryProfileName(c.ctx); active != "default" && active != "custom" {
		profileFlag = " -p " + active
	}
	fmt.Fprintf(out, "  icebath --resume %s%s\n", c.sessionID, profileFlag)
	if sessionTitle != "" {
		fmt.Fprintf(out, "  icebath -c \"%s\"%s\n", sessionTitle, profileFlag)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, c.t("cli.session.exit_label_session", map[string]any{"session_id": c.sessionID}))
	if sessionTitle != "" {
		fmt.Fprintln(out, c.t("cli.session.exit_label_title", map[string]any{"title": sessionTitle}))
	}
	fmt.Fprintln(out, c.t("cli.session.exit_label_duration", map[string]any{"duration": duration}))
	fmt.Fprintln(out, c.t("cli.session.exit_label_messages", map[string]any{"count": msgCount, "user": userMsgs, "tool_calls": toolCalls}))
}

// Python divmod of two ints, the quotient floored and the remainder taking the divisor's sign
func floorDivMod(a, b int) (int, int) {
	q, r := a/b, a%b
	if r != 0 && (r < 0) != (b < 0) {
		q, r = q-1, r+b
	}
	return q, r
}

// Prints a dim notice, raw on stderr for the quiet paths that run before the TUI
func (c *CLI) dimNotice(msg string, quiet bool) {
	if quiet {
		fmt.Fprintln(stderrWriter(), msg)
		return
	}
	c.consolePrint("[dim]" + pyrich.Escape(msg) + "[/dim]")
}

// Moves a resumed session back into the directory it was started from, a missing directory only noted
func (c *CLI) restoreSessionCwd(sessionMeta map[string]any, quiet bool) {
	if !cliTruthy(sessionMeta["cwd"]) {
		return
	}
	recorded := pypath.Expanduser(pyrepr.Str(sessionMeta["cwd"]))
	current, err := os.Getwd()
	if err != nil {
		current = ""
	}
	if current != "" {
		a, aerr := pypath.Realpath(recorded, pypath.Lax)
		b, berr := pypath.Realpath(current, pypath.Lax)
		if aerr == nil && berr == nil && a == b {
			return
		}
	}
	var msg string
	if info, serr := os.Stat(recorded); serr != nil || !info.IsDir() {
		shown := current
		if shown == "" {
			shown = "."
		}
		msg = c.t("cli.session.cwd_gone", map[string]any{"recorded": recorded, "current": shown})
	} else if cerr := os.Chdir(recorded); cerr != nil {
		msg = c.t("cli.session.cwd_enter_failed", map[string]any{"path": recorded, "error": pypath.OSErrorText(cerr)})
	} else {
		_ = os.Setenv("TERMINAL_CWD", recorded)
		msg = c.t("cli.session.cwd_restored", map[string]any{"path": recorded})
	}
	c.dimNotice(msg, quiet)
}

// Arms the approval bypass again when the resumed session row persisted it
func (c *CLI) restoreSessionYolo(sessionMeta map[string]any, quiet bool) {
	sessionKey := c.sessionID
	if sessionKey == "" {
		sessionKey = "default"
	}
	if tools.RestoreSessionYolo(c.ctx, sessionKey, state.SessionYoloEnabled(sessionMeta)) {
		c.dimNotice(c.t("cli.session.yolo_restored", nil), quiet)
	}
}

// Drops a just ended session row that never gained content so it does not clutter the resume list
func (c *CLI) discardSessionIfEmpty(sessionID string) bool {
	if c.sessionDB == nil || sessionID == "" || len(c.conversationHistory) > 0 {
		return false
	}
	removed, err := c.sessionDB.DeleteSessionIfEmpty(c.ctx, sessionID)
	if err != nil {
		cliLogger.Debug(fmt.Sprintf("Could not prune empty session %s", sessionID), "error", err)
		return false
	}
	return removed
}

// Resolves a bare number typed right after a bare resume prompt, true when the input was consumed
func (c *CLI) consumePendingResumeSelection(text string) bool {
	pending := c.pendingResumeSessions
	if len(pending) == 0 {
		return false
	}
	c.pendingResumeSessions = nil
	stripped := pystr.Strip(text)
	if !pystr.StrIsDigit(stripped) {
		return false
	}
	index, ok := pyint.Parse(stripped)
	if !ok || !index.IsInt64() || index.Int64() < 1 || index.Int64() > int64(len(pending)) {
		cprint("  " + c.t("cli.session.resume_index_out_of_range", map[string]any{"index": index.String()}))
		cprint("  " + c.t("cli.session.resume_no_args_hint", nil))
		return true
	}
	c.handleResumeCommand(fmt.Sprintf("/resume %d", index.Int64()))
	return true
}
