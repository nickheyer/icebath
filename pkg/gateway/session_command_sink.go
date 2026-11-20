package gateway

import "context"

// What a messaging command handler would have sent, captured for a local slash exec
type CommandSink struct {
	Notices []string
	Prompt  *string
	Display *string
}

type commandSinkKey struct{}

// Sink capturing command output on the context, nil when no local exec is capturing
func ActiveSink(ctx context.Context) *CommandSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(commandSinkKey{}).(*CommandSink)
	return sink
}

// Context whose command acks and next turn land in a fresh sink instead of the transport
func CaptureCommandSink(ctx context.Context) (context.Context, *CommandSink) {
	sink := &CommandSink{Notices: []string{}}
	return context.WithValue(ctx, commandSinkKey{}, sink), sink
}
