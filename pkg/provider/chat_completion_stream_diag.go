package provider

import (
	"io"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Adds one real chunk to the attempt diagnostics, stamping the first chunk time once
func countDiagChunk(diag *StreamDiag, now time.Time, size int) {
	if diag == nil {
		return
	}
	diag.Chunks++
	if diag.FirstChunkAt == nil {
		t := now
		diag.FirstChunkAt = &t
	}
	diag.Bytes += size
}

// Framing floor plus delta string lengths of a chat chunk, or the text and partial json of an event delta
func estimateChunkBytes(chunk map[string]any) int {
	size := 40
	add := func(obj any, keys ...string) {
		m, _ := obj.(map[string]any)
		for _, k := range keys {
			if v, ok := m[k].(string); ok {
				size += pystr.Len(v)
			}
		}
	}
	if choices, _ := chunk["choices"].([]any); len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		if delta, ok := choice["delta"].(map[string]any); ok {
			add(delta, "content", "reasoning_content", "reasoning")
			calls, _ := delta["tool_calls"].([]any)
			for _, tc := range calls {
				m, _ := tc.(map[string]any)
				if fn, ok := m["function"].(map[string]any); ok {
					add(fn, "arguments", "name")
				}
			}
		}
		return size
	}
	add(chunk["delta"], "text", "partial_json")
	return size
}

// Buffered URL and body of an outgoing request, the body nil when it cannot be replayed
func failedStreamRequestOf(r *http.Request) *FailedStreamRequest {
	out := &FailedStreamRequest{URL: r.URL.String()}
	if r.GetBody != nil {
		if rc, err := r.GetBody(); err == nil {
			out.Content, _ = io.ReadAll(rc)
			_ = rc.Close()
		}
	}
	return out
}

// Attaches the outgoing request below the timeout phase so the phase stays the direct cause the classifier reads
func withWireRequest(base error, wire *FailedStreamRequest) error {
	if tpe, ok := base.(*TimeoutPhaseError); ok {
		return &TimeoutPhaseError{Phase: tpe.Phase, Err: &StreamRequestError{Err: tpe.Err, Request: wire}}
	}
	return &StreamRequestError{Err: base, Request: wire}
}
