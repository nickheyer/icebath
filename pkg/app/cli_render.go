package app

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Clamps the persistent output line limit to at least 10, 200 when the value does not parse
func coerceOutputHistoryLimit(value any) int {
	n, ok := pyIntOf(value)
	if !ok {
		return 200
	}
	return max(10, n)
}

// Prints Rich markup through the prompt safe console once the TUI is live, else the plain console
func chatConsolePrint(markup string) {
	rendered := pyrich.Render(markup, console().ShouldUseColor())
	if tuiApp() != nil {
		paintThroughTUIApp(rendered)
		return
	}
	fmt.Fprintln(console().Out, rendered)
}

// Prints ANSI text through the live TUI renderer, else straight to the console with history recording
func cprint(text string) {
	if tuiApp() != nil {
		paintThroughTUIApp(text)
		return
	}
	releasePaints()
	if outputHistoryRecording() {
		recordOutputHistory(text, true)
	}
	fmt.Fprintln(console().Out, text)
}
