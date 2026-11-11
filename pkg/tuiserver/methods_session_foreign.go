package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

// Foreign session browser of this backend, assigned from pkg/app in cmd/icebath/wiring.go
var ForeignSessions struct {
	List    func(source, offset, limit any) (map[string]any, error)
	Preview func(ctx context.Context, handle any, db *state.DB) (map[string]any, error)
	Import  func(ctx context.Context, handle any, db *state.DB, profile string) (*pyjson.Object, error)
}

var sessionForeignRegistry = &handlerRegistry{}

func init() {
	sessionForeignRegistry.method("session.foreign.list", foreignList)
	sessionForeignRegistry.method("session.foreign.preview", foreignPreview)
	sessionForeignRegistry.method("session.foreign.import", foreignImport)
	sessionForeignRegistry.install()
}

// Value of a param or its default when the key is absent
func paramOr(params map[string]any, key string, def any) any {
	if v, present := params[key]; present {
		return v
	}
	return def
}

// One page of foreign sessions found on this backend
func foreignList(ctx context.Context, rid any, params map[string]any) map[string]any {
	page, err := ForeignSessions.List(params["source"], paramOr(params, "offset", 0), paramOr(params, "limit", 25))
	if err != nil {
		if isValueError(err) {
			return rpcErr(rid, -32602, err.Error(), nil)
		}
		if pypath.IsOSError(err) {
			return rpcErr(rid, -32000, "Could not read session folders on this backend", nil)
		}
		panic(err)
	}
	return ok(rid, page)
}

// Preview or import of one foreign session against the session store of the requested profile
func foreignHistoryRequest(ctx context.Context, rid any, params map[string]any, importing bool) map[string]any {
	db, release, err := profileDB(ctx, params, importing)
	if err == nil {
		defer release()
		if db == nil {
			return dbUnavailableError(ctx, rid, -32000)
		}
		var result any
		if importing {
			result, err = ForeignSessions.Import(ctx, params["id"], db, responseProfileName(ctx, strOr(params["profile"])))
		} else {
			result, err = ForeignSessions.Preview(ctx, params["id"], db)
		}
		if err == nil {
			return ok(rid, result)
		}
	}
	if isValueError(err) {
		return rpcErr(rid, -32602, err.Error(), nil)
	}
	if pypath.IsOSError(err) {
		return rpcErr(rid, -32000, "Could not read this session on the backend", nil)
	}
	panic(err)
}

func foreignPreview(ctx context.Context, rid any, params map[string]any) map[string]any {
	return foreignHistoryRequest(ctx, rid, params, false)
}

func foreignImport(ctx context.Context, rid any, params map[string]any) map[string]any {
	return foreignHistoryRequest(ctx, rid, params, true)
}
