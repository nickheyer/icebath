package tui

import (
	tea "charm.land/bubbletea/v2"
)

// Message that belongs to one session card: the result of a command a session issued, taken by that session's update. A new message type a
// session handles gets a line here, or it reaches no session
type sessionScoped interface{ sessionScoped() }

func (asyncDoneMsg) sessionScoped()             {}
func (bootBackgroundRecheckMsg) sessionScoped() {}
func (clipboardImageMsg) sessionScoped()        {}
func (clipboardTextMsg) sessionScoped()         {}
func (clipboardWaitedMsg) sessionScoped()       {}
func (completionDueMsg) sessionScoped()         {}
func (configPollMsg) sessionScoped()            {}
func (cutDoneMsg) sessionScoped()               {}
func (dieMsg) sessionScoped()                   {}
func (editorDoneMsg) sessionScoped()            {}
func (gitBranchResultMsg) sessionScoped()       {}
func (gitBranchTickMsg) sessionScoped()         {}
func (interruptReplyMsg) sessionScoped()        {}
func (loopTickMsg) sessionScoped()              {}
func (macAppearanceProbeMsg) sessionScoped()    {}
func (refreshTickMsg) sessionScoped()           {}
func (resumeScrollMsg) sessionScoped()          {}
func (terminalHintsMsg) sessionScoped()         {}
func (thinkingStatusMsg) sessionScoped()        {}
func (turnTimerMsg) sessionScoped()             {}
func (typingIdleMsg) sessionScoped()            {}
func (widgetTickedMsg) sessionScoped()          {}
func (widgetsLoadedMsg) sessionScoped()         {}

// Message of a session's command on its way back to that session
type sessionMsg struct {
	s *app
	m tea.Msg
}

// Command whose messages come back to the session that issued it
func (sh *shell) tag(s *app, cmd tea.Cmd) tea.Cmd {
	if cmd == nil || s == nil {
		return cmd
	}
	return func() tea.Msg { return sh.tagged(s, cmd()) }
}

// Message addressed to the session when it is one a session takes, each command of a batch tagged in turn; anything else, the runtime's and the
// window manager's own messages, passes as it is
func (sh *shell) tagged(s *app, m tea.Msg) tea.Msg {
	switch v := m.(type) {
	case tea.BatchMsg:
		out := make(tea.BatchMsg, len(v))
		for i, c := range v {
			out[i] = sh.tag(s, c)
		}
		return out
	case sessionScoped:
		return sessionMsg{s: s, m: m}
	}
	return m
}

// Commands run one after the other, each tagged with the session here because the runtime's sequence cannot be opened to tag them later
func (a *app) sequence(cmds ...tea.Cmd) tea.Cmd {
	tagged := make([]tea.Cmd, len(cmds))
	for i, c := range cmds {
		tagged[i] = a.tag(a, c)
	}
	return tea.Sequence(tagged...)
}

// Whether a program runs to post messages to, none in a session built without one
func (a *app) posts() bool { return a.shell.post != nil }

// Posts a message from outside the update loop, one of this session's coming back to it
func (a *app) post(m tea.Msg) {
	if a.posts() {
		a.shell.post(a.shell.tagged(a, m))
	}
}
