package browser

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const browserCommandDescription = "Helpers for real-profile browsing (browser.use_real_profile). " +
	"close-profile terminates the browser process tree holding your " +
	"default profile so Icebath can copy it — DESTRUCTIVE (unsaved tabs " +
	"in that browser are lost). The agent runs this only after you " +
	"approve closing the browser."

const browserCloseProfileHelp = "Close the browser locking your real profile (asks nothing — " +
	"run only with the user's explicit OK; loses unsaved tabs)"

// Error stream of the browser command
var browserCommandStderr io.Writer = os.Stderr

// argparse style help of the browser command with its one subcommand
func printBrowserCommandHelp(w io.Writer) {
	fmt.Fprintf(w, "usage: icebath browser [-h] {close-profile} ...\n\n%s\n\npositional arguments:\n  {close-profile}\n    %-20s%s\n"+
		"\noptions:\n  -h, --help            show this help message and exit\n", browserCommandDescription, "close-profile", browserCloseProfileHelp)
}

// BrowserCommand runs icebath browser, close-profile ending the browser that holds the real profile
func BrowserCommand(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	if action, _ := ns["browser_action"].(string); action != "close-profile" {
		printBrowserCommandHelp(w)
		return 2, nil
	}
	browser := ""
	if pyrepr.Truthy(ns["browser"]) {
		browser = pyrepr.Str(ns["browser"])
	}
	if browser == "" {
		browser = connectDetectDefaultChromium("")
	}
	if browser == "" || browser == UnsupportedChannel {
		fmt.Fprintln(browserCommandStderr, "✗ No supported Chromium default browser detected.")
		return 1, nil
	}
	src := connectRealProfileDataDir(browser, "")
	if src == "" {
		fmt.Fprintf(browserCommandStderr, "✗ Could not resolve the %s profile directory.\n", browser)
		return 1, nil
	}
	closed, msg := connectCloseBrowserHolding(env, src, 15.0)
	if closed {
		fmt.Fprintf(w, "✓ %s\n", msg)
		return 0, nil
	}
	fmt.Fprintf(browserCommandStderr, "✗ %s\n", msg)
	return 1, nil
}
