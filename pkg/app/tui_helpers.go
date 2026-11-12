package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Runs the TUI gateway bootstrap verb, the grant JSON on stdout or the verdict on stderr with exit 1
func RunTUIGatewayBootstrap(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	savedStdout := os.Stdout
	os.Stdout = os.Stderr
	result, err := tui.GatewayBootstrap(ctx, slices.Contains(argv, "--start"), slices.Contains(argv, "--recover"))
	os.Stdout = savedStdout
	if err != nil {
		fmt.Fprintln(stderr, tui.BootstrapVerdict(err))
		return 1
	}
	text, err := pyjson.Dumps(result)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	fmt.Fprintln(stdout, text)
	return 0
}

// Saves the host clipboard image to the destination argument and prints the outcome as JSON
func RunTUIClipboardImage(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "IndexError: list index out of range")
		return 1
	}
	saved := false
	if err := pm.ActivateDependencies(ctx, tui.HelperRoot()); err != nil {
		fmt.Fprintf(stderr, "clipboard helper: dependency environment unavailable: %s\n", err)
	} else {
		saved = SaveClipboardImage(argv[0])
	}
	if saved {
		fmt.Fprintln(stdout, "true")
	} else {
		fmt.Fprintln(stdout, "false")
	}
	return 0
}
