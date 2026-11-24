package tuihost

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Connected client transports global broadcasts fan out to, and whether stdout is the JSON RPC client channel
var (
	liveTransportsMu  sync.Mutex
	liveTransports    []Transport
	stdioIsRPCChannel atomic.Bool
)

func serverLog() *slog.Logger { return slog.Default().With("logger", "tuigateway") }

// Tracks a connected client transport for global broadcasts, a nil or already tracked one changing nothing
func RegisterLiveTransport(transport Transport) {
	if transport == nil {
		return
	}
	liveTransportsMu.Lock()
	defer liveTransportsMu.Unlock()
	if !slices.Contains(liveTransports, transport) {
		liveTransports = append(liveTransports, transport)
	}
}

// Stops tracking a transport on disconnect and drops the server requests it still owed an answer
func UnregisterLiveTransport(transport Transport) {
	liveTransportsMu.Lock()
	liveTransports = slices.DeleteFunc(liveTransports, func(t Transport) bool { return t == transport })
	liveTransportsMu.Unlock()
	Forget(transport)
}

// True when real stdout is the JSON RPC client channel
func StdioIsRPCChannel() bool { return stdioIsRPCChannel.Load() }

// JSON RPC event notification of one event for a session, the payload key present only with a payload
func EventFrame(event, sid string, payload map[string]any) *jsonutil.Object {
	payload = tuigateway.SealPayload(event, payload)
	params := jsonutil.ObjectOf("type", event, "session_id", sid)
	if payload != nil {
		params.Set("payload", payload)
	}
	return jsonutil.ObjectOf("jsonrpc", "2.0", "method", "event", "params", params)
}

// Fans a session less event to every connected client, stdio when it is the client channel, else drops it
func BroadcastGlobalEvent(event string, payload map[string]any, skip ...Transport) {
	broadcastEvent(event, "", payload, skip...)
}

// BroadcastSessionEvent fans one session's event to every connected client with the session id on its frame, for a session whose own
// transport may already be gone
func BroadcastSessionEvent(event, sid string, payload map[string]any) {
	broadcastEvent(event, sid, payload)
}

// Fans an event framed for sid, empty for none, to every connected client, stdio when it is the client channel, else drops it
func broadcastEvent(event, sid string, payload map[string]any, skip ...Transport) {
	liveTransportsMu.Lock()
	targets := slices.DeleteFunc(slices.Clone(liveTransports), func(t Transport) bool { return slices.Contains(skip, t) })
	liveTransportsMu.Unlock()
	if len(targets) == 0 && len(skip) == 0 {
		if stdioIsRPCChannel.Load() {
			Emit(context.Background(), event, sid, payload)
			return
		}
		serverLog().Debug(fmt.Sprintf("global-event broadcast dropped (no connected client) type=%s", event))
		return
	}
	frame := EventFrame(event, sid, payload)
	for _, transport := range targets {
		writeGlobalEvent(transport, frame, event)
	}
}

// Writes one frame to one peer, a panicking peer logged so the rest still receive it
func writeGlobalEvent(transport Transport, frame *jsonutil.Object, event string) {
	defer func() {
		if r := recover(); r != nil {
			serverLog().Debug(fmt.Sprintf("global-event broadcast write failed type=%s", event), "exc_info", fmt.Errorf("%v", r))
		}
	}()
	transport.Write(frame)
}
