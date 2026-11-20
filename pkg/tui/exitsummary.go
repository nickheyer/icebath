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

	"github.com/nickheyer/icebath/pkg/tuigateway"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/tty"
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
func (a *app) exitReasonOf(err error, panicked bool) string {
	switch {
	case errors.Is(err, tea.ErrProgramPanic):
		// A panic in the model was recorded as it passed, one in a command is known only by how the program ended
		if !panicked {
			recordParentLifecycle("uncaughtException: panic in a command of the UI, its report is on stderr")
		}
		return "uncaughtException"
	case a.exitReason != "":
		return a.exitReason
	case errors.Is(err, tty.ErrTerminalGone):
		recordParentLifecycle(fmt.Sprintf("dead output stream (%v) → exiting", err))
		return "dead-output-stream"
	case errors.Is(err, context.Canceled):
		return "context-cancelled"
	}
	return "terminal UI exit"
}

// Exit status of the UI, printing what it leaves on the terminal it gave back, the session summary and the lines of a failure
func (a *app) exitStatus(err error, signal syscall.Signal) int {
	gone := a.exitReason == "dead-output-stream" || errors.Is(err, tty.ErrTerminalGone)
	if !gone {
		err = errors.Join(err, a.printExitSummary(os.Stdout))
	}
	code := a.exitCode
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
	if a.lastWords != nil {
		a.lastWords()
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

// Summary of the session left on screen, its title, id, turns, duration and resume command
func (a *app) exitSummary() string {
	ui := a.store.ui
	if ui.StoredSid == "" {
		return "\n" + tr("exitSummary.noSession") + "\n"
	}
	title := strings.TrimSpace(ui.SessionTitle)
	if title == "" {
		title = tr("exitSummary.untitled")
	}
	rows := [][2]string{
		{tr("exitSummary.title"), title},
		{tr("exitSummary.id"), ui.StoredSid},
		{tr("exitSummary.turns"), strconv.Itoa(a.sessionTurns())},
		{tr("exitSummary.duration"), fmtDuration(a.store.nowMillis() - a.sessionStart)},
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
	fmt.Fprintf(&sb, "\n%s\n  %s\n", tr("exitSummary.resume"), resumeCommand(ui.StoredSid, ui.Info))
	return sb.String()
}

// Writes the summary of the session left after the terminal is given back
func (a *app) printExitSummary(w io.Writer) error {
	_, err := io.WriteString(w, a.exitSummary())
	return err
}
