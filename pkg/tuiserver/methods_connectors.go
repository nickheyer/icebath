package tuiserver

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Connector handler body answering a response or raising an error the guard maps to a refusal
type connectorBody func(ctx context.Context, rid any, params map[string]any) (map[string]any, error)

var connectorsRegistry = &handlerRegistry{}

func init() {
	connectorsRegistry.install()
	names := make([]string, 0, len(connectorRPCMethods))
	for name := range connectorRPCMethods {
		names = append(names, name)
	}
	addLongHandlers(names...)
	installConnectionUpdateHook()
}

// Snapshot with its settled flag
func connectorSnapshotView(snapshot *pyjson.Object) *pyjson.Object {
	view := snapshot.Copy()
	view.Set("settled", view.GetOr("settled_at", nil) != nil)
	return view
}

// Emits one transition or settlement of an operation to its owning session
func connectionUpdate(operation *tools.ConnectionOperation, change, snapshot *pyjson.Object) {
	payload := connectorSnapshotView(snapshot)
	if change != nil {
		for _, key := range change.Keys {
			payload.Set(key, change.Vals[key])
		}
	}
	sid := ""
	for _, live := range tuihost.LiveSessions() {
		profileDir := strOr(live.Session.Get("profile_home"))
		if profileDir == "" {
			profileDir = home.Process()
		}
		if live.Session.Get("session_key") == operation.SessionKey && home.Key(profileDir) == operation.ProfileKey {
			sid = live.SID
			break
		}
	}
	if sid == "" {
		return
	}
	payload.Set("owner", pyjson.ObjectOf("type", "session", "session_id", sid))
	tuihost.Emit(context.Background(), "connection.update", sid, pyjson.Plain(tuihost.ConnectorUIPayload(payload)).(map[string]any))
}

var connectionUpdateHookOnce sync.Once

// Installs the operation change hook once
func installConnectionUpdateHook() {
	connectionUpdateHookOnce.Do(func() { tools.OperationOnChange = connectionUpdate })
}
