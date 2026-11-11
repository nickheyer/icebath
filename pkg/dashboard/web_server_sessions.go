package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Continuation descendants of a session, excluding delegated, branched, reset and tool owned children
var descendantsSQL = `
            WITH RECURSIVE descendants(id, parent_session_id, started_at) AS (
                SELECT id, parent_session_id, started_at FROM sessions WHERE id = ?
                UNION
                SELECT s.id, s.parent_session_id, s.started_at
                FROM sessions s
                JOIN descendants d ON s.parent_session_id = d.id
                WHERE ` + state.SQLJSONExtract("s.model_config", "$._delegate_from") + ` IS NULL
                  AND ` + state.SQLJSONExtract("s.model_config", "$._branched_from") + ` IS NULL
                  AND NOT (` + state.ResetChildSQL("s") + `)
                  AND COALESCE(s.source, '') != 'tool'
            )
            SELECT id, parent_session_id, started_at FROM descendants
            `

// Detail the toast of a pre fence desktop client maps to its update prompt
const clientUpgradeRequired = "client_upgrade_required: this app is out of sync (different versions) " +
	"with the Icebath backend; update the app to edit sessions"

// Rows of the descendant query, or of the rich session list when the store has no open connection
func descendantRows(ctx context.Context, sid string, db *state.DB) ([]*pyjson.Object, error) {
	if !db.HasWriterConn() {
		limit := 10000
		return db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Limit: &limit, Offset: 0, CompactRows: true})
	}
	var rows []*pyjson.Object
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		result, err := conn.QueryContext(ctx, descendantsSQL, sid)
		if err != nil {
			return err
		}
		defer result.Close()
		for result.Next() {
			var id, parent, started any
			if err := result.Scan(&id, &parent, &started); err != nil {
				return err
			}
			rows = append(rows, pyjson.ObjectOf("id", sqlText(id), "parent_session_id", sqlText(parent), "started_at", started))
		}
		return result.Err()
	})
	return rows, err
}

// Python value of a scanned TEXT column, bytes decoded as the sqlite3 module does
func sqlText(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case sql.RawBytes:
		return string(t)
	}
	return v
}

// Newest continuation leaf of a session and the path to it, empty when the session is unknown
func sessionLatestDescendant(ctx context.Context, sessionID string, db *state.DB) (string, []string, error) {
	sid, err := db.ResolveSessionID(sessionID)
	if err != nil || sid == "" {
		return "", []string{}, err
	}
	session, err := db.GetSession(ctx, sid)
	if err != nil || session == nil {
		return "", []string{}, err
	}
	rows, err := descendantRows(ctx, sid, db)
	if err != nil {
		return "", nil, err
	}
	children := map[string][]*pyjson.Object{}
	for _, row := range rows {
		rid, _ := row.Get("id")
		parent, _ := row.Get("parent_session_id")
		if pyTruthy(rid) && pyTruthy(parent) {
			key := pyStrKey(parent)
			children[key] = append(children[key], row)
		}
	}
	started := func(row *pyjson.Object) float64 {
		raw, _ := row.Get("started_at")
		if !pyTruthy(raw) {
			raw = int64(0)
		}
		f, err := pyint.Float(raw)
		if err != nil {
			return 0.0
		}
		return f
	}
	current, path, seen := sid, []string{sid}, map[string]bool{sid: true}
	for len(children[current]) > 0 {
		var candidates []*pyjson.Object
		for _, row := range children[current] {
			id, _ := row.Get("id")
			if !seen[pyStrKey(id)] {
				candidates = append(candidates, row)
			}
		}
		if len(candidates) == 0 {
			break
		}
		sort.SliceStable(candidates, func(i, j int) bool { return started(candidates[i]) > started(candidates[j]) })
		id, _ := candidates[0].Get("id")
		current = pyStrKey(id)
		path = append(path, current)
		seen[current] = true
	}
	return current, path, nil
}

// Session id of a row value, ids being TEXT in the store
func pyStrKey(v any) string {
	s, _ := v.(string)
	return s
}

// Capabilities a dashboard session mutation principal holds
var sessionMutationCapabilities = map[string]bool{"session:read": true, "session:control": true, "session:create": true,
	"session:operator": true}

// Home of a mutation's store, the profile home when one is named and this process's store dir otherwise
func sessionMutationHome(ctx context.Context, profile string) (string, error) {
	if profile != "" {
		_, profileHome, err := cronProfileHome(ctx, profile)
		return profileHome, err
	}
	return filepath.Dir(state.ResolveDefaultDBPath(ctx)), nil
}

// Whether two paths resolve to the same file system location
func samePathResolved(a, b string) (bool, error) {
	ra, err := pypath.Resolve(a)
	if err != nil {
		return false, err
	}
	rb, err := pypath.Resolve(b)
	if err != nil {
		return false, err
	}
	return pypath.Normcase(ra) == pypath.Normcase(rb), nil
}

// Principal and owning authority of a mutation, the same boundary for prepare and apply, nil authority for a standalone store
func sessionMutationContext(ctx context.Context, req *Request, profile string) (*gateway.SessionAuthority, gateway.Principal, error) {
	var none gateway.Principal
	native := nativePrincipal(&req.HTTPConnection)
	session, _ := req.State().Get("session").(*gateway.DashboardSession)
	app := req.App()
	var subject string
	switch {
	case native != nil:
		subject = pyrepr.Str(native["subject"])
	case session != nil:
		var issuer any
		if session.Issuer != "" {
			issuer = session.Issuer
		}
		s, err := gateway.PrincipalSubject(map[string]any{"user_id": session.UserID, "provider": session.Provider, "issuer": issuer})
		if err != nil {
			return nil, none, err
		}
		subject = s
	case !pyTruthy(app.State.Get("auth_required")) && hasValidSessionToken(req):
		subject = gateway.HostOwnerSubject()
	default:
		return nil, none, NewHTTPException(401, "Unauthorized")
	}
	authority, _ := app.State.Get("session_authority").(*gateway.SessionAuthority)
	profileHome, err := sessionMutationHome(ctx, profile)
	if err != nil {
		return nil, none, err
	}
	if authority == nil {
		if app.State.Get("gateway_runner") != nil {
			return nil, none, NewHTTPException(503, "session_authority_unavailable")
		}
		resolved, err := pypath.Resolve(profileHome)
		if err != nil {
			return nil, none, err
		}
		return nil, gateway.Principal{Subject: subject, ProfileID: resolved, Capabilities: sessionMutationCapabilities,
			TransportID: "http"}, nil
	}
	var owner *gateway.SessionAuthority
	if runner := app.State.Get("gateway_runner"); runner != nil {
		if owner, err = gateway.AuthorityForHome(runner, profileHome); err != nil {
			return nil, none, err
		}
	}
	if owner == nil {
		same, err := samePathResolved(profileHome, filepath.Dir(authority.DB.Path()))
		if err != nil {
			return nil, none, err
		}
		if same {
			owner = authority
		}
	}
	if owner == nil {
		return nil, none, NewHTTPException(403, "profile_mismatch")
	}
	authority = owner
	if native != nil && !pyrepr.Equal(native["profile_id"], authority.ProfileID) {
		return nil, none, NewHTTPException(403, "profile_mismatch")
	}
	transport := "http"
	if native != nil {
		transport = "native-http:" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	return authority, gateway.Principal{Subject: subject, ProfileID: authority.ProfileID, Capabilities: sessionMutationCapabilities,
		TransportID: transport}, nil
}

// Binds a verified native HTTP actor as a native legacy transport for one mutation, returning its unbind
func nativeHistoryTransport(req *Request, authority *gateway.SessionAuthority, actor gateway.Principal) func() {
	var native map[string]any
	if authority != nil && strings.HasPrefix(actor.TransportID, "native-http:") {
		native = nativePrincipal(&req.HTTPConnection)
	}
	if native == nil {
		return func() {}
	}
	gateway.BindNativeTransport(authority, actor, map[string]any{"provider": "local", "profile_id": authority.ProfileID,
		"instance_id": native["instance_id"]})
	return func() { gateway.UnbindNativeTransport(authority, actor) }
}

// Identity and edit of one session mutation request
type sessionMutationParams struct {
	SessionID          string
	RequestID          string
	ExpectedRevision   int64
	Operation          string
	Payload            any
	ExpectedGeneration *int64
}

// Wire params of a mutation in the order the owner verb reads them
func (p sessionMutationParams) object() *pyjson.Object {
	params := pyjson.ObjectOf("session_id", p.SessionID, "request_id", p.RequestID, "expected_revision", p.ExpectedRevision,
		"operation", p.Operation, "payload", p.Payload)
	if p.ExpectedGeneration != nil {
		params.Set("expected_generation", *p.ExpectedGeneration)
	}
	return params
}

// Sessions list of an import payload
func importPayloadSessions(payload any) []any {
	var sessions any
	switch p := payload.(type) {
	case *pyjson.Object:
		sessions, _ = p.Get("sessions")
	case map[string]any:
		sessions = p["sessions"]
	}
	list, _ := sessions.([]any)
	return list
}

// Standalone edit as the receipt transaction on this process's single writer under the exact owner lock
func mutateOfflineStore(ctx context.Context, profile string, actor gateway.Principal, params sessionMutationParams) (*pyjson.Object, error) {
	return withSessionMaintenance(ctx, profile, func(ctx context.Context) (*pyjson.Object, error) {
		var scoped *string
		if profile != "" {
			scoped = &profile
		}
		db, err := openSessionDBForProfile(ctx, scoped, false)
		if err != nil {
			return nil, err
		}
		defer db.Close()
		var writeGuard func(ctx context.Context, conn state.SQLiteConn) error
		if params.Operation == "import" {
			normalized, problems, err := db.ValidateImportPayload(importPayloadSessions(params.Payload))
			if err != nil {
				return nil, err
			}
			if len(problems) > 0 {
				return nil, NewHTTPException(400, pyjson.ObjectOf("errors", problems))
			}
			ids := make([]string, len(normalized))
			for i, item := range normalized {
				session, _ := item.Get("session")
				if obj, ok := session.(*pyjson.Object); ok {
					id, _ := obj.Get("id")
					ids[i] = pyrepr.Str(id)
				}
			}
			historyActor := gateway.HistoryActorOf(actor)
			writeGuard = func(ctx context.Context, conn state.SQLiteConn) error {
				return state.ImportHistoryControl(ctx, conn, historyActor, ids)
			}
		}
		var epoch int64
		found := false
		err = db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			rows, err := conn.QueryContext(ctx, "SELECT epoch FROM runtime_epoch WHERE singleton=1")
			if err != nil {
				return err
			}
			defer rows.Close()
			if rows.Next() {
				found = true
				if err := rows.Scan(&epoch); err != nil {
					return err
				}
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
		if !found {
			if epoch, err = db.BeginRuntimeEpoch(ctx, "dashboard-offline"); err != nil {
				return nil, err
			}
		}
		return db.MutateRuntimeSession(ctx, state.RuntimeMutation{Epoch: epoch, PrincipalID: actor.Subject,
			SessionID: params.SessionID, RequestID: params.RequestID, ExpectedRevision: params.ExpectedRevision,
			Operation: params.Operation, Payload: params.Payload, ExpectedGeneration: params.ExpectedGeneration,
			AuthorizeWrite: writeGuard})
	})
}

// HTTP error that keeps the error it was raised from, the raise from of a refusal
type causedHTTPException struct {
	exc   *HTTPException
	cause error
}

func (e *causedHTTPException) Error() string   { return e.exc.Error() }
func (e *causedHTTPException) Unwrap() []error { return []error{e.exc, e.cause} }

// Whether an error is a transport failure of the owner forward rather than the owner's verdict
func ownerTransportFailure(err error) bool {
	var client *gateway.GatewayClientError
	var discovery *gateway.DiscoveryError
	return errors.As(err, &client) || errors.As(err, &discovery) || pypath.IsOSError(err) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

// Forwards a standalone dashboard mutation to the live gateway owner, the offline store taking it when none answers
func mutateViaGatewayOwner(ctx context.Context, profile string, actor gateway.Principal, params sessionMutationParams) (*pyjson.Object, error) {
	profileHome, err := sessionMutationHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	discovery := gateway.DiscoverGatewayEndpoint(ctx, profileHome, 1500*time.Millisecond)
	endpoint := discovery.Endpoint
	if endpoint == nil {
		result, err := mutateOfflineStore(ctx, profile, actor, params)
		var exc *HTTPException
		var conflict *gateway.OwnershipConflict
		if err == nil || !errors.As(err, &exc) || discovery.State == "absent" || !errors.As(err, &conflict) {
			return result, err
		}
		known := ""
		for _, value := range []string{discovery.ReasonCode, discovery.Detail} {
			if value != "" {
				known += ", " + value
			}
		}
		webServerLog().Warn(fmt.Sprintf("session write for %s: gateway owner not reachable (%s%s)", profileHome, discovery.State, known))
		reason := discovery.ReasonCode
		if reason == "" {
			reason = discovery.State
		}
		return nil, &causedHTTPException{NewHTTPException(503, fmt.Sprintf("owner_unavailable: %s: %s", reason,
			pyrepr.Str(exc.Detail))), err}
	}
	if endpoint.ControlHome == "" {
		pinned := *endpoint
		resolved, err := pypath.Resolve(pypath.Expanduser(profileHome))
		if err != nil {
			return nil, err
		}
		pinned.ControlHome = resolved
		endpoint = &pinned
	}
	call := map[string]any{"session_id": params.SessionID, "request_id": params.RequestID,
		"expected_revision": params.ExpectedRevision, "operation": params.Operation, "payload": params.Payload}
	if params.ExpectedGeneration != nil {
		call["expected_generation"] = *params.ExpectedGeneration
	}
	subject := actor.Subject
	if subject == gateway.HostOwnerSubject() {
		subject = ""
	}
	result, err := func() (any, error) {
		client, err := gateway.ConnectGatewayWith(ctx, gateway.ConnectGatewayOptions{Endpoint: endpoint, Subject: subject})
		if err != nil {
			return nil, err
		}
		defer client.Close()
		return client.RPCTimeout(ctx, "session.mutate", call, 0)
	}()
	var refusal *gateway.GatewayRPCError
	if err != nil && !errors.As(err, &refusal) && ownerTransportFailure(err) {
		return nil, &causedHTTPException{NewHTTPException(503, "owner_unreachable"), err}
	}
	if err != nil {
		return nil, err
	}
	return mutationResultObject(result)
}

// Owner answer of a mutation as an ordered object
func mutationResultObject(result any) (*pyjson.Object, error) {
	switch r := result.(type) {
	case *pyjson.Object:
		return r, nil
	case map[string]any:
		obj := pyjson.NewObject()
		keys := make([]string, 0, len(r))
		for key := range r {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			obj.Set(key, r[key])
		}
		return obj, nil
	}
	return nil, fmt.Errorf("gateway session.mutate answered %T, not an object", result)
}

// Status of each mutation refusal reason, a storage failure being a server fault
var mutationRefusalStatus = map[string]int{"permission_denied": 403, "profile_mismatch": 403, "not_found": 404,
	"invalid_params": 400, "runtime_draining": 503, "storage_unavailable": 503}

// Status of a refusal reason, 409 for any reason the map does not name
func mutationRefusalStatusOf(reason string) int {
	if status, ok := mutationRefusalStatus[reason]; ok {
		return status
	}
	return 409
}

// Resolves the authenticated principal and owner of a session mutation and applies it, never opening a second writer
func mutateSessionRequest(ctx context.Context, req *Request, profile, sessionID string, requestID *string, expectedRevision *int64,
	operation string, payload any, expectedGeneration *int64) (*pyjson.Object, error) {
	authority, actor, err := sessionMutationContext(ctx, req, profile)
	if err != nil {
		return nil, err
	}
	if operation == "import" && authority != nil {
		_, problems, err := authority.DB.ValidateImportPayload(importPayloadSessions(payload))
		if err != nil {
			return nil, err
		}
		if len(problems) > 0 {
			return nil, NewHTTPException(400, pyjson.ObjectOf("errors", problems))
		}
	}
	if requestID == nil && expectedRevision == nil && expectedGeneration == nil {
		return nil, NewHTTPException(409, clientUpgradeRequired)
	}
	if requestID == nil || expectedRevision == nil {
		return nil, NewHTTPException(409, "mutation_identity_required")
	}
	params := sessionMutationParams{SessionID: sessionID, RequestID: *requestID, ExpectedRevision: *expectedRevision,
		Operation: operation, Payload: payload, ExpectedGeneration: expectedGeneration}
	var result *pyjson.Object
	if authority == nil {
		result, err = mutateViaGatewayOwner(ctx, profile, actor, params)
	} else {
		unbind := nativeHistoryTransport(req, authority, actor)
		result, err = gateway.MutateSession(ctx, authority, actor, gateway.SessionRef{ProfileID: authority.ProfileID,
			SessionID: sessionID}, params.object())
		unbind()
	}
	if err == nil {
		out := pyjson.ObjectOf("ok", true)
		for _, key := range result.Keys {
			value, _ := result.Get(key)
			out.Set(key, value)
		}
		return out, nil
	}
	var storeErr *state.RuntimeStoreError
	var clientErr *gateway.GatewayClientError
	var rpcErr *gateway.GatewayRPCError
	var valueErr interface{ PyTypeName() string }
	switch {
	case errors.As(err, &storeErr):
		return nil, &causedHTTPException{NewHTTPException(mutationRefusalStatusOf(storeErr.Reason), storeErr.Reason), err}
	case errors.As(err, &rpcErr):
		return nil, &causedHTTPException{NewHTTPException(mutationRefusalStatusOf(rpcErr.Error()), rpcErr.Error()), err}
	case errors.As(err, &clientErr):
		return nil, &causedHTTPException{NewHTTPException(mutationRefusalStatusOf(clientErr.Error()), clientErr.Error()), err}
	case state.IsMutationGuardRefusal(err):
		return nil, &causedHTTPException{NewHTTPException(409, "session_busy"), err}
	case errors.As(err, &valueErr) && valueErr.PyTypeName() == "ValueError":
		return nil, &causedHTTPException{NewHTTPException(400, err.Error()), err}
	}
	if _, ok := state.SQLiteClassOf(err); ok {
		return nil, &causedHTTPException{NewHTTPException(503, "storage_unavailable"), err}
	}
	return nil, err
}
