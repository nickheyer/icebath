package provider

import (
	"fmt"
	"log/slog"
)

type streamWriterClaimer interface{ ClaimStreamWriter() int }

type streamWriterChecker interface{ StreamWriterIsCurrent(token int) bool }

// StreamWriterFence is the single writer fence an agent owns over its delta sink
type StreamWriterFence interface {
	streamWriterClaimer
	streamWriterChecker
}

// Claims the delta sink for a stream attempt, zero when the agent lacks the fence or the claim panics
func ClaimStreamWriter(agent any) int {
	fence, ok := agent.(streamWriterClaimer)
	if !ok {
		return 0
	}
	return fenceCall(fence.ClaimStreamWriter, 0, "claim failed; proceeding unfenced")
}

// True while token is the active writer, a zero token or a fenceless agent cannot prove supersession
func StreamWriterIsCurrent(agent any, token int) bool {
	if token == 0 {
		return true
	}
	fence, ok := agent.(streamWriterChecker)
	if !ok {
		return true
	}
	return fenceCall(func() bool { return fence.StreamWriterIsCurrent(token) }, true, "is_current check failed; treating as current")
}

// Runs one fence method and answers fallback with a debug log when it panics
func fenceCall[T any](fn func() T, fallback T, failureNote string) (out T) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("stream single-writer: %s", failureNote), "panic", r)
			out = fallback
		}
	}()
	return fn()
}
