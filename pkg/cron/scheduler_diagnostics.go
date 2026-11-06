package cron

import (
	"errors"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const workerStderrTailChars = 1500

// Run document error section with chained causes and URL credentials redacted
func formatRunError(s *Store, err error) string {
	chain := []error{}
	for e := err; e != nil; e = errors.Unwrap(e) {
		chain = append(chain, e)
	}
	var b strings.Builder
	for i := len(chain) - 1; i >= 0; i-- {
		b.WriteString(cronExceptionName(chain[i]) + ": " + chain[i].Error() + "\n")
		if i > 0 {
			b.WriteString("\nThe above exception was the direct cause of the following exception:\n\n")
		}
	}
	text := tools.RedactSensitiveTextWith(&tools.Env{Home: s.home}, b.String(), tools.RedactOptions{Force: true, URLCredentials: true})
	return "## Error\n\n```\n" + text + "\n```\n"
}

// Redacted tail of an external worker's captured stderr as a message suffix, empty when nothing was captured
func externalWorkerStderrTail(s *Store, path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := pystr.Strip(strings.TrimPrefix(strings.ToValidUTF8(string(raw), "�"), "\ufeff"))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	tail := text
	if len(runes) > workerStderrTailChars {
		tail = "…" + string(runes[len(runes)-workerStderrTailChars:])
	}
	tail = tools.RedactSensitiveTextWith(&tools.Env{Home: s.home}, tail, tools.RedactOptions{Force: true, URLCredentials: true})
	return "; worker stderr: " + tail
}
