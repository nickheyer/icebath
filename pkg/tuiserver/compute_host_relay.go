package tuiserver

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// The process compute host supervisor, built on first use
var (
	computeHostSupervisorMu sync.Mutex
	computeHostSupervisor   *HostSupervisor
)

// Cap of the wait a session.compress blocks its RPC on, under the Desktop compress deadline
const computeHostCompressWaitCapSecs = 630.0

// Whether turns run in the compute host, never inside the compute host child itself
func turnIsolationEnabled(ctx context.Context, cfg map[string]any) bool {
	if os.Getenv("ICEBATH_COMPUTE_HOST_CHILD") == "1" {
		return false
	}
	return truthy(loadDashboardProcessIsolationConfig(ctx, cfg)["turn_isolation"])
}

// Whether a session routes to the compute host, a lazy unbuilt session or one an isolated turn already owns
func sessionUsesComputeHost(ctx context.Context, session *tuigateway.Session, cfg map[string]any) bool {
	if !turnIsolationEnabled(ctx, cfg) {
		return false
	}
	if truthy(session.Get("_compute_host_active")) {
		return true
	}
	_, hasReady := session.Lookup("agent_ready")
	return session.Get("agent") == nil && hasReady && session.Get("agent_ready") != nil
}

// The process supervisor, created and started under the isolation config on first use
func getComputeHostSupervisor(ctx context.Context, cfg map[string]any) (*HostSupervisor, error) {
	isolation := cfg
	if isolation == nil {
		isolation = loadDashboardProcessIsolationConfig(ctx, nil)
	}
	computeHostSupervisorMu.Lock()
	defer computeHostSupervisorMu.Unlock()
	if computeHostSupervisor == nil {
		heartbeat, _ := pyIntOf(isolation["compute_host_heartbeat_secs"])
		if heartbeat == 0 {
			heartbeat = 15
		}
		respawn, _ := pyIntOf(isolation["compute_host_respawn_max"])
		if respawn == 0 {
			respawn = 3
		}
		sup, err := NewHostSupervisor(ctx, HostSupervisorOptions{RPCSink: func(message *pyjson.Object) { relayComputeHostRPC(context.WithoutCancel(ctx), message) },
			HeartbeatSecs: heartbeat, RespawnMax: respawn, Autostart: true})
		if err != nil {
			return nil, err
		}
		computeHostSupervisor = sup
	}
	return computeHostSupervisor, nil
}

// Locks the history lock a session holds and returns the unlock, a no-op when it holds none
func historyLockOf(session *tuigateway.Session) func() {
	lock, _ := session.Get("history_lock").(sync.Locker)
	if lock == nil {
		return func() {}
	}
	lock.Lock()
	return lock.Unlock
}

// Whether the mirrored open request of a session is the given id, the caller holding its history lock
func openRequestMatches(session *tuigateway.Session, requestID any) bool {
	mirrored, isDict := session.Get("_compute_host_open_request").(map[string]any)
	return isDict && pyrepr.Equal(mirrored["id"], requestID)
}

// Relays a host frame to the client, mirroring the server request the host holds open for reconnects
func relayComputeHostRPC(ctx context.Context, message *pyjson.Object) bool {
	params, paramsIsDict := message.GetOr("params", nil).(*pyjson.Object)
	if message.GetOr("method", nil) == "compute_host.activity" {
		if paramsIsDict {
			if session := tuigateway.SessionRecord(strOr(params.GetOr("session_id", nil))); session != nil {
				unlock := historyLockOf(session)
				turnID := params.GetOr("turn_id", nil)
				if truthy(session.Get("running")) && truthy(turnID) && pyrepr.Equal(session.Get("_compute_host_turn_id"), turnID) {
					session.Set("_compute_host_activity_ns", params.GetOr("activity_ns", nil))
				}
				unlock()
			}
		}
		return true
	}
	if message.GetOr("method", nil) == "event" && paramsIsDict && !truthy(params.GetOr("session_id", nil)) {
		payload, _ := pyjson.Plain(params.GetOr("payload", nil)).(map[string]any)
		tuigateway.BroadcastGlobalEvent(strOr(params.GetOr("type", nil)), payload)
		return true
	}
	method, hasMethod := message.Get("method")
	if id, isText := message.GetOr("id", nil).(string); isText && hasMethod && method != nil && method != "event" {
		if paramsIsDict {
			if session := tuigateway.SessionRecord(strOr(params.GetOr("session_id", nil))); session != nil {
				unlock := historyLockOf(session)
				session.Set("_compute_host_open_request", map[string]any{"id": id, "method": method, "params": pyjson.Plain(params)})
				unlock()
			}
		}
	} else if paramsIsDict && params.GetOr("type", nil) == "request.cancel" {
		session := tuigateway.SessionRecord(strOr(params.GetOr("session_id", nil)))
		if payload, isDict := params.GetOr("payload", nil).(*pyjson.Object); session != nil && isDict {
			unlock := historyLockOf(session)
			if openRequestMatches(session, payload.GetOr("id", nil)) {
				session.Pop("_compute_host_open_request")
			}
			unlock()
		}
	}
	return tuigateway.WriteJSON(ctx, message)
}

// Live session mirroring the host owned server request, ok false when none does
func computeHostRequestSession(requestID string) (string, *tuigateway.Session, bool) {
	if requestID == "" {
		return "", nil, false
	}
	for _, live := range tuigateway.LiveSessions() {
		unlock := historyLockOf(live.Session)
		matches := openRequestMatches(live.Session, requestID)
		unlock()
		if matches {
			return live.SID, live.Session, true
		}
	}
	return "", nil, false
}

// Forwards a client response to the compute host child owning the request, false when no child owns it
func relayComputeHostResponse(ctx context.Context, frame *pyjson.Object) bool {
	sid, session, found := computeHostRequestSession(strOr(frame.GetOr("id", nil)))
	if !found || !sessionUsesComputeHost(ctx, session, nil) {
		return false
	}
	unlock := historyLockOf(session)
	session.Pop("_compute_host_open_request")
	unlock()
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				logger().Debug(fmt.Sprintf("compute-host response relay failed sid=%s: %s", sid, failureText(failure)))
			}
		}()
		sup, err := getComputeHostSupervisor(ctx, nil)
		if err == nil {
			copied := pyjson.NewObject()
			for _, key := range frame.Keys {
				copied.Set(key, frame.Vals[key])
			}
			_, err = sup.Respond(sid, pyjson.ObjectOf("frame", copied), 15*time.Second)
		}
		if err != nil {
			logger().Debug(fmt.Sprintf("compute-host response relay failed sid=%s: %s", sid, err))
		}
	}()
	return true
}
