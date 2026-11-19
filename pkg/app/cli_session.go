package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

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

// Active profile lookup of the exit resume hint, the profile resolver in production
var exitSummaryActiveProfileName = profiles.GetActiveProfileName

// Active profile for the resume hint, default when the lookup fails
func exitSummaryProfileName(ctx context.Context) (name string) {
	defer func() {
		if r := recover(); r != nil {
			name = "default"
		}
	}()
	return exitSummaryActiveProfileName(ctx)
}

// Session clock reading the wall time as datetime.now() does, swapped by tests
var cliNow = pydatetime.Now

// Prints the session resume commands, title, duration and message counts on exit
func (c *CLI) printExitSummary() {
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
	total := int(cliNow().Sub(c.sessionStart) / time.Second)
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
		msg = c.t("cli.session.cwd_enter_failed", map[string]any{"path": recorded, "error": errchain.OSText(cerr)})
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
