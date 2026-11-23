package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/txt"
)

const (
	gatewayRecoveryLimit    = 3
	gatewayRecoveryWindowMs = 60_000
	stderrLineCap           = 120
	stderrLinesMax          = 4
)

// Decision after a gateway death, the pruned attempt window, whether to recover and which session
type recoveryPlan struct {
	Attempts []float64
	Recover  bool
	Sid      string
}

// Plans a respawn and resume of the live session or the carried recovery target within the sliding budget
func planGatewayRecovery(liveSid, recoverSid string, attempts []float64, now float64) recoveryPlan {
	sid := liveSid
	if sid == "" {
		sid = recoverSid
	}
	recent := []float64{}
	for _, at := range attempts {
		if now-at < gatewayRecoveryWindowMs {
			recent = append(recent, at)
		}
	}
	recover := sid != "" && len(recent) < gatewayRecoveryLimit
	if recover {
		recent = append(recent, now)
	}
	return recoveryPlan{Attempts: recent, Recover: recover, Sid: sid}
}

// Reports gateway.start_timeout, still waiting with failure looking stderr lines or the bootstrap's terminal verdict
func (c *turnController) reportGatewayStartTimeout(stderrTail string, setStatus func(string)) {
	terminal := isTerminalBootstrapFailure(stderrTail)
	if terminal {
		setStatus(tr(txt.UserMessagesBackendGaveUpActivity))
	} else {
		setStatus(tr(txt.UserMessagesBackendSlowStartStatus))
		c.pushActivity(tr(txt.UserMessagesBackendSlowStart), "warn", "")
	}
	var lines []string
	for line := range strings.SplitSeq(stderrTail, "\n") {
		if line = strings.TrimFunc(line, isJSSpace); line != "" && stderrLooksLikeProblem(line) {
			lines = append(lines, line)
		}
	}
	for _, line := range lines[max(0, len(lines)-stderrLinesMax):] {
		c.pushActivity(jsSlice(line, 0, stderrLineCap), "error", "")
	}
}

// Offers to restart a background service of another build, once for the window, or names it where the launcher cannot restart it
func (a *app) offerOwnerRestart() {
	switch {
	case a.host.ReplaceOwner == nil:
		a.sys(tr(txt.GatewayMsgOtherBuildLine))
	case a.primary():
		a.store.overlay.Confirm = &confirmReq{Title: tr(txt.GatewayMsgOtherBuildTitle), Detail: tr(txt.GatewayMsgOtherBuildDetail),
			ConfirmLabel: tr(txt.GatewayMsgOtherBuildRestart), CancelLabel: tr(txt.GatewayMsgOtherBuildKeep), OnConfirm: a.restartOwner}
	}
}

// Stops the background service of another build off the update loop; losing the connection then brings up one of this build
func (a *app) restartOwner() tea.Cmd {
	a.sys(tr(txt.GatewayMsgOtherBuildRestarting))
	replace, ctx := a.host.ReplaceOwner, a.ctx
	return func() tea.Msg {
		err := replace(ctx)
		return asyncDoneMsg{run: func() tea.Cmd {
			if err != nil {
				a.sys(tr(txt.GatewayMsgOtherBuildRestartFailed, err.Error()))
			}
			return nil
		}}
	}
}
