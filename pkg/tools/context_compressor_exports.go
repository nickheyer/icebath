package tools

import "strings"

// Longest elision marker any count can render, in code points
func ElisionMarkerMaxLen() int { return elisionMarkerMaxLen }

// Command gate cli denied, transport denied and cli timeout texts with every placeholder empty
func CommandGateRefusalTexts() (cliDenied, transportDenied, cliTimeout string) {
	blank := strings.NewReplacer("{description}", "", "{breaker}", "")
	return blank.Replace(commandGate.cliDenied), blank.Replace(commandGate.transportDenied), blank.Replace(commandGate.cliTimeout)
}

// Terminal error envelope with exit code minus one and the given status
func TerminalErrorJSON(errText, status string) string { return errorJSON(errText, -1, status, nil) }

// Workdir guard verdict, empty when the workdir is allowed
func ValidateWorkdir(workdir string) string { return validateWorkdir(workdir) }
