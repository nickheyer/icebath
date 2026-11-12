package tui

import "strings"

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
		setStatus(tr("userMessages.backend.gaveUpActivity"))
	} else {
		setStatus(tr("userMessages.backend.slowStartStatus"))
		c.pushActivity(tr("userMessages.backend.slowStart"), "warn", "")
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
