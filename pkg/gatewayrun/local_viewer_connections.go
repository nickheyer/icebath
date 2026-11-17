package gatewayrun

import (
	"maps"
	"reflect"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Authority and canonical session that last ran a turn under a process session key
type connectionRoute struct {
	authority *gateway.SessionAuthority
	sessionID string
}

var localConnections = struct {
	mu     sync.Mutex
	routes map[string]connectionRoute
	hook   func(op *tools.ConnectionOperation, change, snapshot *pyjson.Object)
}{routes: map[string]connectionRoute{}}

func funcIdentity(fn any) uintptr {
	if fn == nil || reflect.ValueOf(fn).IsNil() {
		return 0
	}
	return reflect.ValueOf(fn).Pointer()
}

// Routes connection updates of operations our turns opened to the owning session, others to the wrapped hook
func installConnectionUpdateHook() {
	previous := tools.OperationOnChange
	localConnections.mu.Lock()
	ours := localConnections.hook
	localConnections.mu.Unlock()
	if ours != nil && previous != nil && funcIdentity(previous) == funcIdentity(ours) {
		return
	}
	var hook func(op *tools.ConnectionOperation, change, snapshot *pyjson.Object)
	hook = func(op *tools.ConnectionOperation, change, snapshot *pyjson.Object) {
		localConnections.mu.Lock()
		route, ok := localConnections.routes[op.SessionKey]
		localConnections.mu.Unlock()
		var live *gateway.LiveSession
		if ok {
			live, _ = route.authority.Live(route.sessionID)
		}
		if live == nil || home.Key(route.authority.ProfileID) != op.ProfileKey {
			if previous != nil {
				previous(op, change, snapshot)
			}
			return
		}
		payload := pyjson.NewObject()
		if snapshot != nil {
			for _, key := range snapshot.Keys {
				payload.Set(key, snapshot.Vals[key])
			}
		}
		settled := false
		if snapshot != nil {
			settled = snapshot.GetOr("settled_at", nil) != nil
		}
		payload.Set("settled", settled)
		if change != nil {
			for _, key := range change.Keys {
				payload.Set(key, change.Vals[key])
			}
		}
		payload.Set("owner", pyjson.ObjectOf("type", "session", "session_id", route.sessionID))
		live.EventStream.Publish(route.sessionID, tuigateway.ConnectorUIPayload(payload), "connection.update", nil)
	}
	localConnections.mu.Lock()
	localConnections.hook = hook
	localConnections.mu.Unlock()
	tools.OperationOnChange = hook
}

// Publishes this turn's connection requests and routes its later connection updates to the owning session
func wireLocalConnections(t *TurnRunner, sinks *agent.LocalViewerSinks) {
	owner := t.approvalOwner
	if owner == nil {
		return
	}
	installConnectionUpdateHook()
	if key := t.ctx.sessionKey; key != "" {
		localConnections.mu.Lock()
		localConnections.routes[key] = connectionRoute{authority: owner.authority, sessionID: owner.sessionID}
		localConnections.mu.Unlock()
	}
	publish := t.publishExecution
	sinks.Connection = func(payload map[string]any) { publish("connection.request", maps.Clone(payload)) }
}
