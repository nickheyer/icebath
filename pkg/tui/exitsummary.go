package tui

import (
	"fmt"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"io"
	"strconv"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/ansi"
)

// Exit status of a program the operating system ended with the signal, 128 plus its number
func signalExitCode(s syscall.Signal) int { return 128 + int(s) }

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
