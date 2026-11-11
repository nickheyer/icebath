package dashboard

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	chatNeedsNode = "%s could not start: icebath needs Node.js to run the terminal chat. " +
		"Install Node 18+ (for example from nodejs.org) and reopen this tab."
	chatTooManyTerminals = "%s could not start: too many chat terminals are open in other tabs. " +
		"Close one and click Start new session."
	chatProfileUnknown = "%s could not start: %s Pick another profile from the switcher and reopen this tab."
	chatStartFailed    = "%s could not start: %s Check the server log (`icebath dashboard` terminal) and click Start new session."
)

// Whether a start failure is a SystemExit or names a missing node, npm or Node.js
func nodeMissing(exc error) bool {
	text := exc.Error()
	return pyrepr.TypeName(exc) == "SystemExit" || strings.Contains(text, "'node'") || strings.Contains(text, "'npm'") ||
		strings.Contains(text, "Node.js")
}

// One plain sentence for the terminal pane describing a chat start failure, never the bare exception
func chatStartFailureMessage(exc error, surface string) string {
	var full *tools.RegistryFullError
	if errors.As(exc, &full) {
		return fmt.Sprintf(chatTooManyTerminals, surface)
	}
	if h, ok := asHTTPException(exc); ok {
		return fmt.Sprintf(chatProfileUnknown, surface, chatSentence(pyrepr.Str(h.Detail)))
	}
	if nodeMissing(exc) {
		return fmt.Sprintf(chatNeedsNode, surface)
	}
	text := exc.Error()
	if text == "" {
		text = pyrepr.TypeName(exc)
	}
	return fmt.Sprintf(chatStartFailed, surface, chatSentence(text))
}

// Stripped text ending in sentence punctuation, a launch failure sentence when empty
func chatSentence(text string) string {
	text = pystr.Strip(text)
	if text == "" {
		return "the terminal process could not be launched."
	}
	if strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") || strings.HasSuffix(text, "?") {
		return text
	}
	return text + "."
}
