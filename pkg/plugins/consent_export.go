package plugins

import "context"

// Python dependency consent gate for callers outside the package, the refusal text empty when consented
func ConsentPythonDeps(ctx context.Context, pluginName string, deps []string, console *Console, assumeYes bool) (bool, string) {
	ok, refusal := consentPythonDeps(ctx, pluginName, deps, console, assumeYes)
	if refusal != nil {
		return ok, refusal.Text
	}
	return ok, ""
}

// Replaces the terminal check and the line reader every prompt goes through, restore puts the previous pair back
func SwapTerminalIO(tty func() bool, input func(prompt string) (string, error)) (restore func()) {
	savedTTY, savedInput := isTTY, inputLine
	isTTY, inputLine = tty, input
	return func() { isTTY, inputLine = savedTTY, savedInput }
}
