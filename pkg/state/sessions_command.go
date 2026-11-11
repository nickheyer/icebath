package state

import (
	"context"
	"io"
)

// Runs icebath sessions through the store command handler with the help of the sessions parser bound
func SessionsCommand(ctx context.Context, h string, ns map[string]any, w io.Writer, printHelp func() error) (int, error) {
	return CmdSessions(ctx, h, ns, w, printHelp)
}
