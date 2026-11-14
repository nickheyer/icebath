package process

const nonInteractiveAdvice = "Pass input non-interactively instead (arguments, a here-doc, or `printf ... | cmd`)."

func noPTYReason(s *Session) string {
	s.mu.Lock()
	fallback, log := s.ptyFallback, s.outputLog
	s.mu.Unlock()
	if fallback != "" {
		return "pty=true was requested but " + fallback + ", so it runs without stdin"
	}
	if log != "" {
		return "background processes here run in an isolated worker that cannot give them a pseudo-terminal"
	}
	if !ptyAvailable() {
		return ptyUnavailableReason
	}
	return ""
}

func alreadyExitedError(s *Session) string {
	if s.process == nil || s.process.Stdin() != nil {
		return "Process has already finished"
	}
	if reason := noPTYReason(s); reason != "" {
		return "Process has already finished. It ran without stdin (" + reason + "), so if it read stdin, it " +
			"got end-of-file at once. " + nonInteractiveAdvice
	}
	return "Process has already finished. It was started without stdin (terminal background=true without " +
		"pty=true), so if it read stdin, it got end-of-file at once. Relaunch it with " +
		"terminal(background=true, pty=true) to send input."
}

func stdinUnavailableError(s *Session) string {
	if s.process == nil {
		return "Process stdin not available: it runs on a non-local terminal backend (or was recovered " +
			"after a restart), which has no live stdin. " + nonInteractiveAdvice
	}
	if reason := noPTYReason(s); reason != "" {
		return "This process has no stdin: " + reason + ". Relaunching with pty=true would not help. " + nonInteractiveAdvice
	}
	return "This process was started without stdin (terminal background=true without pty=true). " +
		"Relaunch it with terminal(background=true, pty=true) to send input."
}
