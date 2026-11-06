package tools

import "strings"

// Longest elision marker any count can render, in code points
func ElisionMarkerMaxLen() int { return elisionMarkerMaxLen }

// Command gate cli denied, transport denied and cli timeout texts with the description and breaker fields emptied
func CommandGateRefusalTexts() (cliDenied, transportDenied, cliTimeout string) {
	blank := strings.NewReplacer("{description}", "", "{breaker}", "")
	return blank.Replace(commandGate.cliDenied), blank.Replace(commandGate.transportDenied), blank.Replace(commandGate.cliTimeout)
}

// Terminal error envelope with exit code minus one and the given status
func TerminalErrorJSON(errText, status string) string { return errorJSON(errText, -1, status, nil) }

// Workdir guard verdict, empty when the workdir is allowed
func ValidateWorkdir(workdir string) string { return validateWorkdir(workdir) }

// Intra record truncation marker naming the omitted and total character counts
func ElisionMarker(omitted, total int) string { return elisionMarker(omitted, total) }

// Whether the text carries a minted compression marker first sentence
func HasCompressionMarker(text string) bool { return compressionMarkerRe.MatchString(text) }
