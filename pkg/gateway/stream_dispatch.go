package gateway

import (
	"context"
	"fmt"
	"log/slog"
)

// Adapter surface that renders message events onto a sink and formats tool chrome
type StreamEventRenderer interface {
	RenderMessageEvent(event StreamEvent, sink StreamSink)
	FormatToolEvent(ctx context.Context, event StreamEvent, mode string, previewMaxLen int) (string, bool)
}

// Tool progress queue, tool mode, preview cap and the gateway owned long tool and notice hooks
type DispatcherOptions struct {
	EnqueueToolLine func(line string)
	ToolMode        string
	PreviewMaxLen   *int
	OnLongTool      func(hint LongToolHint)
	OnNotice        func(notice GatewayNotice)
}

// Routes typed stream events through an adapter onto the stream consumer and the tool progress queue
type GatewayEventDispatcher struct {
	Adapter         StreamEventRenderer
	Sink            StreamSink
	ToolMode        string
	PreviewMaxLen   int
	enqueueToolLine func(line string)
	onLongTool      func(hint LongToolHint)
	onNotice        func(notice GatewayNotice)
	lastTool        *string
}

// Dispatcher over adapter and sink, a nil sink dropping message events and tool mode defaulting to all
func NewGatewayEventDispatcher(adapter StreamEventRenderer, sink StreamSink, opts DispatcherOptions) *GatewayEventDispatcher {
	mode := opts.ToolMode
	if mode == "" {
		mode = "all"
	}
	previewMaxLen := 40
	if opts.PreviewMaxLen != nil {
		previewMaxLen = *opts.PreviewMaxLen
	}
	return &GatewayEventDispatcher{Adapter: adapter, Sink: sink, ToolMode: mode, PreviewMaxLen: previewMaxLen,
		enqueueToolLine: opts.EnqueueToolLine, onLongTool: opts.OnLongTool, onNotice: opts.OnNotice}
}

// Routes one event, a failure is logged at debug and never reaches the agent worker
func (d *GatewayEventDispatcher) Dispatch(ctx context.Context, event StreamEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("stream-event dispatch error: %v", r))
		}
	}()
	d.dispatch(ctx, event)
}

// Message events to the sink, tool calls to the progress queue, hints and notices to their hooks
func (d *GatewayEventDispatcher) dispatch(ctx context.Context, event StreamEvent) {
	switch ev := event.(type) {
	case MessageChunk, MessageStop, Commentary:
		if d.Sink != nil {
			d.Adapter.RenderMessageEvent(event, d.Sink)
		}
	case ToolCallChunk:
		d.dispatchToolCall(ctx, ev)
	case LongToolHint:
		if d.onLongTool != nil {
			d.onLongTool(ev)
		}
	case GatewayNotice:
		if d.onNotice != nil {
			d.onNotice(ev)
		}
	}
}

// Enqueues the adapter's tool line unless progress is off, a new mode repeat, or the adapter ate it
func (d *GatewayEventDispatcher) dispatchToolCall(ctx context.Context, event ToolCallChunk) {
	if d.ToolMode == "off" || d.enqueueToolLine == nil {
		return
	}
	if d.ToolMode == "new" && d.lastTool != nil && event.ToolName == *d.lastTool {
		return
	}
	name := event.ToolName
	d.lastTool = &name
	line, ok := d.Adapter.FormatToolEvent(ctx, event, d.ToolMode, d.PreviewMaxLen)
	if ok && line != "" {
		d.enqueueToolLine(line)
	}
}
