package tuigateway

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Connected client transports global broadcasts fan out to, and whether stdout is the JSON RPC client channel
var (
	liveTransportsMu  sync.Mutex
	liveTransports    []Transport
	stdioIsRPCChannel atomic.Bool
)

func serverLog() *slog.Logger { return slog.Default().With("logger", "tui_gateway.server") }

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
	forgetServerRequests(transport)
}

// Marks real stdout as the JSON RPC client channel, true only in the stdio TUI process
func SetStdioIsRPCChannel(on bool) { stdioIsRPCChannel.Store(on) }

// True when real stdout is the JSON RPC client channel
func StdioIsRPCChannel() bool { return stdioIsRPCChannel.Load() }

// JSON RPC event notification of one event for a session, the payload key present only with a payload
func EventFrame(event, sid string, payload map[string]any) *pyjson.Object {
	checkEventPayload(event, payload)
	params := pyjson.ObjectOf("type", event, "session_id", sid)
	if payload != nil {
		params.Set("payload", payload)
	}
	return pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params", params)
}

// Fans a session less event to every connected client, stdio when it is the client channel, else drops it
func BroadcastGlobalEvent(event string, payload map[string]any, skip ...Transport) {
	liveTransportsMu.Lock()
	targets := slices.DeleteFunc(slices.Clone(liveTransports), func(t Transport) bool { return slices.Contains(skip, t) })
	liveTransportsMu.Unlock()
	if len(targets) == 0 && len(skip) == 0 {
		if stdioIsRPCChannel.Load() {
			emitEvent(event, "", payload)
			return
		}
		serverLog().Debug(fmt.Sprintf("global-event broadcast dropped (no connected client) type=%s", event))
		return
	}
	frame := EventFrame(event, "", payload)
	for _, transport := range targets {
		writeGlobalEvent(transport, frame, event)
	}
}

// Writes one frame to one peer, a panicking peer logged so the rest still receive it
func writeGlobalEvent(transport Transport, frame *pyjson.Object, event string) {
	defer func() {
		if r := recover(); r != nil {
			serverLog().Debug(fmt.Sprintf("global-event broadcast write failed type=%s", event), "exc_info", fmt.Errorf("%v", r))
		}
	}()
	transport.Write(frame)
}
