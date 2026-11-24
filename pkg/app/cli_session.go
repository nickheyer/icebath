package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	consoleutil "github.com/nickheyer/icebath/pkg/utils/console"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
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
	text = strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " ")
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
var cliNow = time.Now

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
	c.consolePrint("[dim]" + consoleutil.Escape(msg) + "[/dim]")
}

// Moves a resumed session back into the directory it was started from, a missing directory only noted
func (c *CLI) restoreSessionCwd(sessionMeta map[string]any, quiet bool) {
	if !cliTruthy(sessionMeta["cwd"]) {
		return
	}
	recorded := pathutil.ExpandUser(anyutil.String(sessionMeta["cwd"]))
	current, err := os.Getwd()
	if err != nil {
		current = ""
	}
	if current != "" {
		a, aerr := pathutil.Resolve(recorded)
		b, berr := pathutil.Resolve(current)
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
		moveCLIFolder(recorded)
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
