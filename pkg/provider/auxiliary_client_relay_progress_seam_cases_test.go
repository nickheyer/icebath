package provider

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func auxRelaySeamChunk(text string) map[string]any {
	return map[string]any{"id": "r1", "model": "m", "usage": nil, "choices": []any{map[string]any{"finish_reason": nil,
		"delta": map[string]any{"content": text, "reasoning": nil, "reasoning_content": nil, "reasoning_details": nil, "tool_calls": nil}}}}
}

// Client streaming two chunks when asked to stream and answering plain otherwise, recording the stream flag
func auxRelaySeamClient(stream func() AuxChatStream) *auxScriptedClient {
	return &auxScriptedClient{base: "https://example.test/v1", create: func(kwargs map[string]any) (any, error) {
		if kwargs["stream"] == true {
			return stream(), nil
		}
		return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "plain"}}}}, nil
	}}
}

func auxRelaySeamWire(client *auxScriptedClient) []any {
	client.mu.Lock()
	defer client.mu.Unlock()
	wire := make([]any, 0, len(client.calls))
	for _, c := range client.calls {
		wire = append(wire, c["stream"])
	}
	return wire
}

// Chat stream failing with the error once its chunks are exhausted
type auxFailingStream struct {
	chunks []map[string]any
	err    error
}

func (s *auxFailingStream) Next(context.Context) (map[string]any, error) {
	if len(s.chunks) == 0 {
		return nil, s.err
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func (s *auxFailingStream) Close() error { return nil }

func TestRelayDefaultCallbackStreamsAndTicksHookSyncAndAsync(t *testing.T) {
	var ticks atomic.Int64
	ctx := AuxProgressHook(auxMainTestHome(t, ""), func() error { ticks.Add(1); return nil })
	twoChunks := func() AuxChatStream {
		return &auxSliceStream{chunks: []map[string]any{auxRelaySeamChunk("hello"), auxRelaySeamChunk(" world")}}
	}
	syncClient := auxRelaySeamClient(twoChunks)
	resp, err := auxRelaySyncCompletion(ctx, syncClient, map[string]any{"model": "m", "messages": []any{}}, "", "", nil)
	if err != nil || auxActualContent(t, resp) != "hello world" {
		t.Fatalf("sync got (%v, %v)", resp, err)
	}
	if wire := auxRelaySeamWire(syncClient); len(wire) != 1 || wire[0] != true {
		t.Fatalf("sync wire %v", wire)
	}
	syncTicks := ticks.Load()
	if syncTicks < 2 {
		t.Fatalf("sync ticks %d", syncTicks)
	}
	asyncClient := auxRelaySeamClient(twoChunks)
	resp, err = auxRelayAsyncCompletion(ctx, asyncClient, map[string]any{"model": "m", "messages": []any{}}, "", "", nil)
	if err != nil || auxActualContent(t, resp) != "hello world" {
		t.Fatalf("async got (%v, %v)", resp, err)
	}
	if wire := auxRelaySeamWire(asyncClient); len(wire) != 1 || wire[0] != true {
		t.Fatalf("async wire %v", wire)
	}
	if ticks.Load()-syncTicks < 2 {
		t.Fatalf("async ticks %d", ticks.Load()-syncTicks)
	}
}

func TestRelayDefaultCallbackIsPlainCreateWithoutHook(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	client := auxRelaySeamClient(func() AuxChatStream { return &auxSliceStream{} })
	resp, err := auxRelaySyncCompletion(ctx, client, map[string]any{"model": "m", "messages": []any{}}, "", "", nil)
	if err != nil || auxActualContent(t, resp) != "plain" {
		t.Fatalf("got (%v, %v)", resp, err)
	}
	if wire := auxRelaySeamWire(client); len(wire) != 1 || wire[0] != nil {
		t.Fatalf("wire %v", wire)
	}
}

func TestAsyncMidstreamFailureIsNotRetriedNonStreaming(t *testing.T) {
	ctx := AuxProgressHook(auxMainTestHome(t, ""), func() error { return nil })
	client := auxRelaySeamClient(func() AuxChatStream {
		return &auxFailingStream{chunks: []map[string]any{auxRelaySeamChunk("partial")}, err: errors.New("bad frame")}
	})
	_, err := auxAcreateWithProgress(ctx, client, map[string]any{"model": "m", "messages": []any{}}, "", false)
	if err == nil || !strings.Contains(err.Error(), "bad frame") {
		t.Fatalf("midstream failure must propagate, got %v", err)
	}
	if wire := auxRelaySeamWire(client); len(wire) != 1 || wire[0] != true {
		t.Fatalf("wire %v", wire)
	}
}
