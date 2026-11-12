package main

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/app"
)

// Exit status of the Ink TUI helper verb named by argv, or false when argv names none
func runTUIHelperVerb(argv []string) (int, bool) {
	if len(argv) == 0 {
		return 0, false
	}
	switch argv[0] {
	case "tui-gateway-bootstrap":
		return app.RunTUIGatewayBootstrap(context.Background(), argv[1:], os.Stdout, os.Stderr), true
	case "tui-clipboard-image":
		return app.RunTUIClipboardImage(context.Background(), argv[1:], os.Stdout, os.Stderr), true
	}
	return 0, false
}
