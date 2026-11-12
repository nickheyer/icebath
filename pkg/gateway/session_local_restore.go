package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner that keeps a registry of served profile authorities
type authorityRegistryRunner interface {
	SessionAuthorities() *SessionAuthorities
}

// Runner that predates the registry and holds its single launch authority
type launchAuthorityRunner = SessionAuthorityHolder

// Runner holding the launch adapters and the live adapter map of each served secondary profile
type adapterMapRunner interface {
	Adapters() *pydict.Dict[Platform, PlatformAdapter]
	ProfileAdapterMap(name string) *pydict.Dict[Platform, PlatformAdapter]
}

var localAdapterMu sync.Mutex

// Registry of the runner serving the authority, nil for a bare runner
func registryOf(runner any) *SessionAuthorities {
	if r, ok := runner.(authorityRegistryRunner); ok {
		return r.SessionAuthorities()
	}
	return nil
}

// Served profile name of the authority, false for the launch profile and for runners without a registry
func servedProfile(a *SessionAuthority) (string, bool) {
	if registry := registryOf(a.Runner); registry != nil {
		return registry.ProfileName(a)
	}
	return "", false
}

// Adapter map local sessions of the authority live in, the launch adapters or its served profile's own map
func localAdapterMap(ctx context.Context, a *SessionAuthority) (*pydict.Dict[Platform, PlatformAdapter], error) {
	runner, ok := a.Runner.(adapterMapRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'adapters'", a.Runner)}
	}
	if name, served := servedProfile(a); served {
		return runner.ProfileAdapterMap(name), nil
	}
	return runner.Adapters(), nil
}

// Local adapter the authority's local sessions and detached conversations deliver through, created on first use
func localAdapter(ctx context.Context, a *SessionAuthority) (PlatformAdapter, error) {
	adapters, err := localAdapterMap(ctx, a)
	if err != nil {
		return nil, err
	}
	localAdapterMu.Lock()
	defer localAdapterMu.Unlock()
	adapter, ok := adapters.Get(PlatformLocal)
	if !ok || adapter == nil {
		adapter = NewLocalSessionAdapter(a)
		adapters.Set(PlatformLocal, adapter)
	}
	return adapter, nil
}

// Local source of a session the authority owns, carrying the served profile of a secondary
func localSource(ctx context.Context, a *SessionAuthority, chatID, userID string) (*SessionSource, error) {
	source := &SessionSource{Platform: PlatformLocal, ChatID: chatID, UserID: &userID, ChatType: "dm"}
	if name, served := servedProfile(a); served {
		source.Profile = &name
	}
	return source, nil
}

// Receipt check failure with the function and line where it was raised
type checkFailure struct {
	err  error
	fn   string
	line int
}

func (e *checkFailure) Error() string { return e.err.Error() }

func (e *checkFailure) Unwrap() error { return e.err }

// Marks the calling function and line on a failed receipt check, an earlier mark kept
func failedAt(err error) error {
	if err == nil {
		return nil
	}
	if _, marked := err.(*checkFailure); marked {
		return err
	}
	pc, _, line, ok := runtime.Caller(1)
	if !ok {
		return err
	}
	name := runtime.FuncForPC(pc).Name()
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return &checkFailure{err: err, fn: name, line: line}
}

// Marks the named check and the line that called it on its failure, an earlier mark kept
func failedIn(check string, err error) error {
	if err == nil {
		return nil
	}
	if _, marked := err.(*checkFailure); marked {
		return err
	}
	_, _, line, _ := runtime.Caller(1)
	return &checkFailure{err: err, fn: check, line: line}
}

// Python exception class of a Go error raised by a receipt check
func checkErrorClass(err error) string {
	if marked, ok := err.(*checkFailure); ok {
		err = marked.err
	}
	if _, refused := err.(*state.RuntimeStoreError); refused {
		return "RuntimeStoreError"
	}
	var keyErr *pyKeyError
	if errors.As(err, &keyErr) {
		return "KeyError"
	}
	var typed *PyError
	if errors.As(err, &typed) {
		return typed.Kind
	}
	var undecodable *pyjson.DecodeError
	if errors.As(err, &undecodable) {
		return "JSONDecodeError"
	}
	return pyrepr.ExceptionName(err)
}

// True for the KeyError, TypeError, ValueError and AttributeError family a refused receipt raises
func receiptCheckFailure(err error) bool {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return true
	}
	switch checkErrorClass(err) {
	case "KeyError", "TypeError", "ValueError", "AttributeError", "JSONDecodeError", "UnicodeDecodeError", "UnicodeEncodeError":
		return true
	}
	return false
}

// Reason or error with the function and line of the failed check, followed through every wrapped cause
func refusedCheck(err error) string {
	where, inner := "", err
	if marked, ok := err.(*checkFailure); ok {
		where, inner = fmt.Sprintf(" at %s:%d", marked.fn, marked.line), marked.err
	}
	var cause string
	if refusal := refusalOf(inner); refusal != nil {
		cause = refusal.Reason
	} else {
		cause = checkErrorClass(inner) + ": " + pyrepr.ExceptionMessage(inner)
	}
	chained := ""
	if next := checkCause(inner); next != nil {
		chained = ", caused by " + refusedCheck(next)
	}
	return cause + where + chained
}

// Refusal an error is, not one it was merely raised from
func refusalOf(err error) *state.RuntimeStoreError {
	switch e := err.(type) {
	case *state.RuntimeStoreError:
		return e
	case *receiptRefused:
		return e.RuntimeStoreError
	case *catchUpPending:
		return e.RuntimeStoreError
	}
	return nil
}

// Error a failed check was raised from, nil when it stands alone
func checkCause(err error) error {
	switch e := err.(type) {
	case *receiptRefused:
		return e.cause
	case *catchUpPending:
		return nil
	}
	return errors.Unwrap(err)
}

// Refused receipt the restore already logged with its failing check
type receiptRefused struct {
	*state.RuntimeStoreError
	cause error
}

func (e *receiptRefused) Unwrap() error { return e.RuntimeStoreError }

// Refusal raised while a receipt's compression catch up commits in a worker
type catchUpPending struct {
	*state.RuntimeStoreError
}

func (e *catchUpPending) Unwrap() error { return e.RuntimeStoreError }

// Value of a receipt key, the KeyError of an absent one
func receiptField(receipt *pyjson.Object, key string) (any, error) {
	value, ok := receipt.Get(key)
	if !ok {
		return nil, &pyKeyError{key: key}
	}
	return value, nil
}

// True when two sources serialise to the same dict
func sameSourceDict(a, b *SessionSource) bool {
	return reflect.DeepEqual(pyjson.Plain(a.ToDict()), pyjson.Plain(b.ToDict()))
}

// Receipt after recording compression steps an older build published past its tip, handed to a worker on the owner loop
func withUntrackedCompression(ctx context.Context, a *SessionAuthority, sid string, receipt *pyjson.Object) (*pyjson.Object, error) {
	adoptable := false
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		children, routed, err := state.AdoptableCompression(ctx, conn, receipt)
		adoptable = children != nil || routed != nil
		return err
	})
	if err != nil {
		return nil, failedIn("AdoptableCompression", err)
	}
	if !adoptable {
		return receipt, nil
	}
	if onOwnerLoop(ctx) {
		scheduleAdoption(ctx, a, sid, receipt)
		return nil, &catchUpPending{state.NewRuntimeStoreError("storage_unavailable", nil, nil)}
	}
	if err := a.DB.AdoptUntrackedCompression(ctx, a.Epoch, sid, receipt); err != nil {
		return nil, failedIn("AdoptUntrackedCompression", err)
	}
	adopted, err := a.DB.LocalReceipt(ctx, sid)
	return adopted, failedIn("LocalReceipt", err)
}

// Entry of a receipt once its route, origin and stored identity are this authority's
func checkedEntry(ctx context.Context, a *SessionAuthority, receipt *pyjson.Object, source *SessionSource, route string) (*SessionEntry, error) {
	stored, err := receiptField(receipt, "route")
	if err != nil {
		return nil, failedAt(err)
	}
	if stored != any(route) {
		return nil, failedAt(&PyError{Kind: "ValueError", Msg: "route mismatch"})
	}
	rawEntry, err := receiptField(receipt, "entry")
	if err != nil {
		return nil, failedAt(err)
	}
	entryDict, ok := rawEntry.(*pyjson.Object)
	if !ok {
		return nil, failedAt(&PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(rawEntry) + "' object is not subscriptable"})
	}
	entry, err := SessionEntryFromDict(ctx, entryDict)
	if err != nil {
		return nil, failedIn("SessionEntryFromDict", err)
	}
	rawOrigin, err := receiptField(entryDict, "origin")
	if err != nil {
		return nil, failedAt(err)
	}
	origin, ok := rawOrigin.(*pyjson.Object)
	if !ok {
		return nil, failedAt(&PyError{Kind: "TypeError", Msg: "argument of type '" + jsonValueTypeName(rawOrigin) + "' is not iterable"})
	}
	if entry.Origin == nil {
		return nil, failedAt(&PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'profile'"})
	}
	if _, has := origin.Get("profile"); !has {
		entry.Origin.Profile = source.Profile
	}
	if entry.SessionKey != route || !sameSourceDict(entry.Origin, source) {
		return nil, failedAt(&PyError{Kind: "ValueError", Msg: "entry mismatch"})
	}
	row, err := a.DB.GetSession(ctx, entry.SessionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, failedAt(&PyError{Kind: "ValueError", Msg: "stored identity mismatch"})
	}
	key, _ := row.Get("session_key")
	chat, _ := row.Get("chat_id")
	user, _ := row.Get("user_id")
	if key != any(route) || chat != any(source.ChatID) || user != optStrAny(source.UserID) {
		return nil, failedAt(&PyError{Kind: "ValueError", Msg: "stored identity mismatch"})
	}
	return entry, nil
}

// Stored receipt of a session with its local source, route and entry, once all are this owner's
func ownedReceipt(ctx context.Context, a *SessionAuthority, sid string) (*pyjson.Object, *SessionSource, string, *SessionEntry, error) {
	fail := func(err error) (*pyjson.Object, *SessionSource, string, *SessionEntry, error) {
		return nil, nil, "", nil, err
	}
	receipt, err := a.DB.LocalReceipt(ctx, sid)
	if err != nil {
		return fail(failedIn("LocalReceipt", err))
	}
	profileID, err := receiptField(receipt, "profile_id")
	if err != nil {
		return fail(failedAt(err))
	}
	if profileID != any(a.ProfileID) {
		return fail(failedAt(runtimeRefusal("profile_mismatch")))
	}
	chatID, legacy := receipt.Get("legacy_session_id")
	if !legacy {
		principal, err := receiptField(receipt, "principal_id")
		if err != nil {
			return fail(failedAt(err))
		}
		request, err := receiptField(receipt, "request_id")
		if err != nil {
			return fail(failedAt(err))
		}
		identity, err := state.LocalIdentity(profileID, principal, request)
		if err != nil {
			return fail(failedIn("LocalIdentity", err))
		}
		chatID = identity
	}
	sessionID, err := receiptField(receipt, "session_id")
	if err != nil {
		return fail(failedAt(err))
	}
	if sessionID != any(sid) || chatID != any(sid) {
		return fail(failedAt(&PyError{Kind: "ValueError", Msg: "identity mismatch"}))
	}
	principal, err := receiptField(receipt, "principal_id")
	if err != nil {
		return fail(failedAt(err))
	}
	userID, ok := principal.(string)
	if !ok {
		return fail(failedAt(&PyError{Kind: "ValueError", Msg: "entry mismatch"}))
	}
	source, err := localSource(ctx, a, sid, userID)
	if err != nil {
		return fail(err)
	}
	route, err := a.Runner.SessionStore().generateSessionKey(ctx, source, nil)
	if err != nil {
		return fail(failedAt(err))
	}
	entry, err := checkedEntry(ctx, a, receipt, source, route)
	if err != nil {
		return fail(err)
	}
	return receipt, source, route, entry, nil
}

// True when a session holds no transcript, ledger rows or children
func unusedSession(ctx context.Context, conn state.SQLiteConn, sessionID string) (bool, error) {
	var one int
	err := conn.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE session_id=? UNION ALL SELECT 1 FROM session_admissions "+
		"WHERE target_session_id=? UNION ALL SELECT 1 FROM worker_executions WHERE session_id=? "+
		"UNION ALL SELECT 1 FROM sessions WHERE parent_session_id=? LIMIT 1", sessionID, sessionID, sessionID, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return false, err
}

// True for an empty replacement the stale route heal minted on a local route outside the receipt lineage
func strayRouteTarget(ctx context.Context, a *SessionAuthority, receipt *pyjson.Object, sessionID string) (bool, error) {
	lineage, err := receiptLineage(receipt)
	if err != nil {
		return false, err
	}
	if sessionID == "" || lineage[sessionID] {
		return false, nil
	}
	unused := false
	err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var err error
		unused, err = unusedSession(ctx, conn, sessionID)
		return err
	})
	if err != nil || !unused {
		return false, err
	}
	owner, _ := receipt.Get("session_id")
	slog.Warn(fmt.Sprintf("Local route for %s named an empty replacement %s outside its lineage; restoring the receipt target",
		pyrepr.Str(owner), sessionID))
	return true, nil
}

// Restores a local session from its private receipt into the authority, its route, policy and admission source
func restoreLocalSession(ctx context.Context, a *SessionAuthority, sid string) (SessionRef, error) {
	var (
		receipt *pyjson.Object
		source  *SessionSource
		route   string
		entry   *SessionEntry
		policy  *LocalSessionPolicy
		target  string
	)
	store := a.Runner.SessionStore()
	err := func() error {
		var err error
		receipt, source, route, entry, err = ownedReceipt(ctx, a, sid)
		if err != nil {
			return err
		}
		adopted, err := withUntrackedCompression(ctx, a, sid, receipt)
		if err != nil {
			return err
		}
		rawPolicy, err := receiptField(adopted, "policy")
		if err != nil {
			return failedAt(err)
		}
		if policy, err = restorePolicy(rawPolicy); err != nil {
			return failedIn("restorePolicy", err)
		}
		if adopted != receipt {
			receipt = adopted
			if entry, err = checkedEntry(ctx, a, adopted, source, route); err != nil {
				return err
			}
		}
		target = entry.SessionID
		return failedIn("ValidateLocalLineage", a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			_, err := state.ValidateLocalLineage(ctx, conn, receipt)
			return err
		}))
	}()
	if err != nil {
		var pending *catchUpPending
		if errors.As(err, &pending) || !receiptCheckFailure(err) {
			return SessionRef{}, err
		}
		slog.Warn(fmt.Sprintf("Local session %s receipt refused: %s", sid, refusedCheck(err)))
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return SessionRef{}, &receiptRefused{state.NewRuntimeStoreError(refusal.Reason, refusal.Field, refusal.Detail), err}
		}
		return SessionRef{}, &receiptRefused{state.NewRuntimeStoreError("storage_unavailable", nil, nil), err}
	}
	adapter, err := localAdapter(ctx, a)
	if err != nil {
		return SessionRef{}, err
	}
	local, ok := adapter.(*LocalSessionAdapter)
	if !ok || local.Authority != a {
		return SessionRef{}, runtimeRefusal("runtime_draining")
	}
	lineage, err := receiptLineage(receipt)
	if err != nil {
		return SessionRef{}, err
	}
	chatID := source.ChatID
	live, _ := a.live(sid)
	switch {
	case live == nil || isHistoryMount(live):
		source = local.keptSource(source)
		store.mu.Lock()
		store.ensureLoadedLocked(ctx)
		current := store.entries.get(route)
		if current != nil && current.SessionID != target {
			stray, err := strayRouteTarget(ctx, a, receipt, current.SessionID)
			if err != nil || !stray {
				store.mu.Unlock()
				if err != nil {
					return SessionRef{}, err
				}
				return SessionRef{}, runtimeRefusal("admission_conflict")
			}
			current = nil
		}
		if current != nil {
			entry.Yolo = current.Yolo
		}
		entry.Origin = source
		store.entries.set(route, entry)
		store.mu.Unlock()
		local.setPolicy(chatID, policy)
		a.mu.Lock()
		if held := a.Sessions[sid]; held == nil {
			a.Sessions[sid] = NewLiveSession(source, route)
			delete(a.retiredLocal, sid)
		} else {
			held.Source, held.Route = source, route
		}
		a.mu.Unlock()
		local.registerSource(source)
	case !local.authorizeSource(live.Source) || !sameSourceDict(live.Source, source) || !samePolicy(local.policy(chatID), policy):
		return SessionRef{}, runtimeRefusal("storage_unavailable")
	case routeAlreadyRestored(store, route, target, lineage):
	default:
		store.mu.Lock()
		current := store.entryLocked(ctx, route)
		if current == nil {
			entry.Origin = source
			store.entries.set(route, entry)
		} else if !lineage[current.SessionID] {
			stray, err := strayRouteTarget(ctx, a, receipt, current.SessionID)
			if err != nil || !stray {
				store.mu.Unlock()
				if err != nil {
					return SessionRef{}, err
				}
				return SessionRef{}, runtimeRefusal("admission_conflict")
			}
			store.entries.retarget(current, target)
		} else {
			store.entries.retarget(current, target)
		}
		store.mu.Unlock()
	}
	return SessionRef{ProfileID: a.ProfileID, SessionID: sid}, nil
}

// True when two frozen policies hold the same fields
func samePolicy(held, policy *LocalSessionPolicy) bool {
	return held != nil && policy != nil && reflect.DeepEqual(*held, *policy)
}

// True when the loaded route already names the target, a lineage member, so a claim writes nothing
func routeAlreadyRestored(store *SessionStore, route, target string, lineage map[string]bool) bool {
	current, held := store.entries.sessionIDOf(route)
	return held && current == target && lineage[target]
}

// Current physical transcript of a logical session, a local receipt's entry or the compression tip
func transcriptTarget(ctx context.Context, a *SessionAuthority, ref SessionRef) (string, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return "", &pyKeyError{key: ref.SessionID}
	}
	if live.Source != nil && live.Source.Platform == PlatformLocal {
		if _, err := restoreLocalSession(ctx, a, ref.SessionID); err != nil {
			return "", err
		}
		return receiptEntryTarget(ctx, a.DB, ref.SessionID)
	}
	return a.PhysicalTarget(ctx, ref)
}

// Session id the entry of a stored local receipt names
func receiptEntryTarget(ctx context.Context, db *state.SessionDB, sid string) (string, error) {
	receipt, err := db.LocalReceipt(ctx, sid)
	if err != nil {
		return "", err
	}
	rawEntry, err := receiptField(receipt, "entry")
	if err != nil {
		return "", err
	}
	entry, ok := rawEntry.(*pyjson.Object)
	if !ok {
		return "", &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(rawEntry) + "' object is not subscriptable"}
	}
	target, err := receiptField(entry, "session_id")
	if err != nil {
		return "", err
	}
	text, _ := target.(string)
	return text, nil
}
