package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Exit status of a program the operating system ended with the signal, 128 plus its number
func signalExitCode(s syscall.Signal) int { return 128 + int(s) }

// Name of a stop signal as the crash log spells it
func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return "signal " + strconv.Itoa(int(s))
}

// Ends the UI for a stop request of the operating system, the signal recorded in the crash log first, which is what tells a hangup or a kill
// from a quit once the gateway logged the detach that follows
func (a *app) terminated(s syscall.Signal) tea.Cmd {
	name := signalName(s)
	recordParentLifecycle("graceful-exit received signal=" + name + " → detaching from the gateway")
	fmt.Fprintln(os.Stderr, "icebath lifecycle: received "+name)
	return a.dieFor(signalExitCode(s), "graceful-exit-cleanup")
}

// Ends the UI whose terminal went away without a signal, recorded in the crash log since nothing can be shown any more
func (a *app) outputGone(err error) tea.Cmd {
	recordParentLifecycle(fmt.Sprintf("dead output stream (%v) → exiting", err))
	return a.dieFor(1, "dead-output-stream")
}

// Reason the gateway client is told the UI ends for, the one the quit named or what the end of the program shows
func (sh *shell) exitReasonOf(err error, panicked bool) string {
	switch {
	case errors.Is(err, tea.ErrProgramPanic):
		// A panic in the model was recorded as it passed, one in a command is known only by how the program ended
		if !panicked {
			recordParentLifecycle("uncaughtException: panic in a command of the UI, its report is on stderr")
		}
		return "uncaughtException"
	case sh.exitReason != "":
		return sh.exitReason
	case errors.Is(err, tty.ErrTerminalGone):
		recordParentLifecycle(fmt.Sprintf("dead output stream (%v) → exiting", err))
		return "dead-output-stream"
	case errors.Is(err, context.Canceled):
		return "context-cancelled"
	}
	return "terminal UI exit"
}

// Exit status of the UI, printing what it leaves on the terminal it gave back, the summary of its sessions and the lines of a failure
func (sh *shell) exitStatus(err error, signal syscall.Signal) int {
	gone := sh.exitReason == "dead-output-stream" || errors.Is(err, tty.ErrTerminalGone)
	if !gone {
		err = errors.Join(err, sh.printExitSummary(os.Stdout))
	}
	code := sh.exitCode
	switch {
	case errors.Is(err, tea.ErrProgramPanic):
		code = 2
	case gone:
		code = 1
	case err != nil && !errors.Is(err, tea.ErrProgramKilled) && !errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "icebath: "+err.Error())
		code = 1
	}
	if signal != 0 {
		code = signalExitCode(signal)
	}
	if sh.lastWords != nil {
		sh.lastWords()
		code = memoryExitStatus
	}
	return code
}

// User turns among the rows
func userRows(items []msg) int {
	n := 0
	for _, m := range items {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

// Turns of the session in view, those the history cap dropped included
func (a *app) sessionTurns() int { return a.droppedTurns + userRows(a.history) }

// Command that resumes the session, the profile flag added for a named profile
func resumeCommand(storedSid string, info *tuigateway.SessionInfo) string {
	cmd := "icebath --resume " + storedSid
	switch profile := strings.TrimSpace(val(infoOf(info).ProfileName)); profile {
	case "", "default", "custom":
	default:
		cmd += " -p " + profile
	}
	return cmd
}

// Title of the session for the summary, a word for one that has none
func (a *app) summaryTitle() string {
	if title := strings.TrimSpace(a.store.ui.SessionTitle); title != "" {
		return title
	}
	return tr(txt.ExitSummaryUntitled)
}

// Summary of the sessions left open: the one in view with its title, id, turns, duration and resume command, then the resume command of each
// other session card
func (sh *shell) exitSummary() string {
	cur := sh.active()
	if cur == nil || cur.store.ui.StoredSid == "" {
		cur = nil
		for _, s := range sh.sessions {
			if s.store.ui.StoredSid != "" {
				cur = s
				break
			}
		}
	}
	if cur == nil {
		return "\n" + tr(txt.ExitSummaryNoSession) + "\n"
	}
	var sb strings.Builder
	sb.WriteString(cur.exitSummary())
	others := false
	for _, s := range sh.sessions {
		if s == cur || s.store.ui.StoredSid == "" {
			continue
		}
		if !others {
			others = true
			fmt.Fprintf(&sb, "\n%s\n", tr(txt.ExitSummaryAlsoOpen))
		}
		fmt.Fprintf(&sb, "  %s  %s\n", resumeCommand(s.store.ui.StoredSid, s.store.ui.Info), s.summaryTitle())
	}
	return sb.String()
}

// Summary of one session, its title, id, turns, duration and resume command
func (a *app) exitSummary() string {
	ui := a.store.ui
	title := a.summaryTitle()
	rows := [][2]string{
		{tr(txt.ExitSummaryTitle), title},
		{tr(txt.ExitSummaryId), ui.StoredSid},
		{tr(txt.ExitSummaryTurns), strconv.Itoa(a.sessionTurns())},
		{tr(txt.ExitSummaryDuration), fmtDuration(a.store.nowMillis() - a.sessionStart)},
	}
	width := 0
	for _, r := range rows {
		width = max(width, ansi.StringWidth(r[0]))
	}
	var sb strings.Builder
	sb.WriteString("\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%s%s  %s\n", r[0], strings.Repeat(" ", width-ansi.StringWidth(r[0])), r[1])
	}
	fmt.Fprintf(&sb, "\n%s\n  %s\n", tr(txt.ExitSummaryResume), resumeCommand(ui.StoredSid, ui.Info))
	return sb.String()
}

// Writes the summary of the sessions left open after the terminal is given back
func (sh *shell) printExitSummary(w io.Writer) error {
	_, err := io.WriteString(w, sh.exitSummary())
	return err
}
