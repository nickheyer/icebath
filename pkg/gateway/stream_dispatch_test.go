package gateway

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type streamFakeSink struct {
	deltas, commentary []string
	segmentBreaks      int
}

func (s *streamFakeSink) OnDelta(text string)      { s.deltas = append(s.deltas, text) }
func (s *streamFakeSink) OnCommentary(text string) { s.commentary = append(s.commentary, text) }
func (s *streamFakeSink) OnSegmentBreak()          { s.segmentBreaks++ }

func toolChunk(name, preview string) ToolCallChunk {
	return NewToolCallChunk(name, &preview, nil, nil, 0)
}

func TestStreamDispatchMessageChunkFlowsToSinkOnDelta(t *testing.T) {
	sink := &streamFakeSink{}
	d := NewGatewayEventDispatcher(newBaseFake(PlatformTelegram), sink, DispatcherOptions{})
	d.Dispatch(context.Background(), MessageChunk{Text: "hello "})
	d.Dispatch(context.Background(), MessageChunk{Text: "world"})
	if !reflect.DeepEqual(sink.deltas, []string{"hello ", "world"}) {
		t.Fatal(sink.deltas)
	}
}

func TestStreamDispatchIntermediateMessageStopBreaksSegmentButFinalDoesNot(t *testing.T) {
	sink := &streamFakeSink{}
	d := NewGatewayEventDispatcher(newBaseFake(PlatformTelegram), sink, DispatcherOptions{})
	d.Dispatch(context.Background(), MessageStop{Final: false})
	d.Dispatch(context.Background(), MessageStop{Final: true})
	if sink.segmentBreaks != 1 {
		t.Fatal(sink.segmentBreaks)
	}
}

func TestStreamDispatchCommentaryFlowsToSink(t *testing.T) {
	sink := &streamFakeSink{}
	d := NewGatewayEventDispatcher(newBaseFake(PlatformTelegram), sink, DispatcherOptions{})
	d.Dispatch(context.Background(), Commentary{Text: "I'll inspect the repo first."})
	if !reflect.DeepEqual(sink.commentary, []string{"I'll inspect the repo first."}) {
		t.Fatal(sink.commentary)
	}
}

func TestStreamDispatchToolCallChunkRendersDefaultChrome(t *testing.T) {
	var lines []string
	d := NewGatewayEventDispatcher(newBaseFake(PlatformTelegram), &streamFakeSink{}, DispatcherOptions{
		EnqueueToolLine: func(line string) { lines = append(lines, line) }, ToolMode: "all"})
	d.Dispatch(context.Background(), toolChunk("terminal", "ls -la"))
	if len(lines) != 1 || !strings.Contains(lines[0], "terminal") || !strings.Contains(lines[0], "ls -la") {
		t.Fatal(lines)
	}
}

func TestStreamDispatchNewModeDedupsSameTool(t *testing.T) {
	var lines []string
	d := NewGatewayEventDispatcher(newBaseFake(PlatformTelegram), &streamFakeSink{}, DispatcherOptions{
		EnqueueToolLine: func(line string) { lines = append(lines, line) }, ToolMode: "new"})
	d.Dispatch(context.Background(), toolChunk("terminal", "a"))
	d.Dispatch(context.Background(), toolChunk("terminal", "b"))
	d.Dispatch(context.Background(), toolChunk("read_file", "c"))
	if len(lines) != 2 {
		t.Fatal(lines)
	}
}

type panickingRenderer struct{ *baseFakeAdapter }

func (panickingRenderer) RenderMessageEvent(StreamEvent, StreamSink) { panic("render blew up") }

func TestStreamDispatchRoutesControlEventsAndContainsFailures(t *testing.T) {
	var hints []LongToolHint
	var notices []GatewayNotice
	var lines []string
	d := NewGatewayEventDispatcher(newBaseFake(PlatformTelegram), nil, DispatcherOptions{
		EnqueueToolLine: func(line string) { lines = append(lines, line) }, ToolMode: "off",
		OnLongTool: func(h LongToolHint) { hints = append(hints, h) }, OnNotice: func(n GatewayNotice) { notices = append(notices, n) }})
	ctx := context.Background()
	d.Dispatch(ctx, MessageChunk{Text: "dropped without a sink"})
	d.Dispatch(ctx, toolChunk("terminal", "ls"))
	d.Dispatch(ctx, NewToolCallFinished("terminal"))
	d.Dispatch(ctx, LongToolHint{ToolName: "terminal", Duration: 31})
	d.Dispatch(ctx, NewGatewayNotice("restart", "back online"))
	if len(lines) != 0 || len(hints) != 1 || hints[0].ToolName != "terminal" || len(notices) != 1 || notices[0].Kind != "restart" {
		t.Fatalf("lines %v hints %v notices %v", lines, hints, notices)
	}
	if NewGatewayEventDispatcher(nil, nil, DispatcherOptions{}).ToolMode != "all" {
		t.Fatal("tool mode default")
	}
	boom := NewGatewayEventDispatcher(panickingRenderer{newBaseFake(PlatformTelegram)}, &streamFakeSink{}, DispatcherOptions{})
	boom.Dispatch(ctx, MessageChunk{Text: "x"})
}
