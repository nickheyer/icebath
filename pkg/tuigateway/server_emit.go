package tuigateway

import (
	"context"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Real stdout reserved for JSON RPC frames, the module stdio transport over it and the live session records
var (
	stdoutMu       sync.Mutex
	realStdout     io.Writer = os.Stdout
	stdioTransport           = NewStdioTransport(func() io.Writer { return realStdout }, &stdoutMu)
	sessionsMu     sync.RWMutex
	sessions       = map[string]*Session{}
	sessionOrder   []string
	// Presentation mute of a diagnostic only turn, installed by the agent package that owns the mute state
	presentationMuted = func(ctx context.Context, event, sid string) bool { return false }
)

func init() {
	BindSinks(func(frame *pyjson.Object) { WriteJSON(context.Background(), frame) },
		func(event, sid string, payload map[string]any) { Emit(context.Background(), event, sid, payload) },
		sessionClientAnswersRequests, sessionAnsweringClients)
}

// Detached WebSocket sink that keeps a session resumable while dropping stale frames
type dropTransport struct{ _ byte }

func (*dropTransport) Write(obj *pyjson.Object) bool { return false }

func (*dropTransport) Close() {}

// Sink of every session whose WebSocket client left, in place of stdio that an embedding desktop captures
var detachedWSTransport Transport = &dropTransport{}

// Installs the predicate that mutes freeform events of the session whose turn is diagnostic only
func SetEventPresentationMuted(muted func(ctx context.Context, event, sid string) bool) {
	presentationMuted = muted
}

// Record of a live session by id, nil when the process holds none
func SessionRecord(sid string) *Session {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	return sessions[sid]
}

// Stores the record of a live session at the end of the registry order, a nil record dropping it
func SetSessionRecord(sid string, record *Session) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	_, known := sessions[sid]
	if record == nil {
		if known {
			delete(sessions, sid)
			sessionOrder = slices.DeleteFunc(sessionOrder, func(id string) bool { return id == sid })
		}
		return
	}
	if !known {
		sessionOrder = append(sessionOrder, sid)
	}
	sessions[sid] = record
}

// One live session with its id
type sessionEntry struct {
	sid     string
	session *Session
}

// Live sessions in the order they were registered
func sessionEntries() []sessionEntry {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	out := make([]sessionEntry, 0, len(sessionOrder))
	for _, sid := range sessionOrder {
		out = append(out, sessionEntry{sid, sessions[sid]})
	}
	return out
}

// Writes one frame through the transport of its session, else the context bound one, else module stdio
func WriteJSON(ctx context.Context, obj *pyjson.Object) bool {
	StampEvent(obj)
	params, _ := obj.GetOr("params", nil).(*pyjson.Object)
	_, idIsString := obj.GetOr("id", nil).(string)
	_, hasMethod := obj.Get("method")
	if obj.GetOr("method", nil) == "event" || (idIsString && hasMethod) {
		ProjectRoomMemberActivity(ctx, obj, SessionRecord)
		if params != nil {
			if sid, _ := params.GetOr("session_id", nil).(string); sid != "" {
				if transport, ok := SessionRecord(sid).Get("transport").(Transport); ok && transport != nil {
					return transport.Write(obj)
				}
			}
		}
	}
	if transport := CurrentTransport(ctx); transport != nil {
		return transport.Write(obj)
	}
	return stdioTransport.Write(obj)
}

// Emits one event for a session unless its presentation is muted, stamped with the running turn identity
func Emit(ctx context.Context, event, sid string, payload map[string]any) bool {
	if presentationMuted(ctx, event, sid) {
		return false
	}
	return WriteJSON(ctx, EventFrame(event, sid, StampSessionFrame(SessionRecord(sid), event, payload)))
}
