package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// One SQLite write a step sequence yields
type writeStep func(ctx context.Context) (any, error)

// Runs one yielded write and hands its outcome back to the sequence
type stepRunner func(write writeStep) (any, error)

// Sequence of write steps that asks its runner to perform each write and returns its final value
type writeSteps func(run stepRunner) (any, error)

// Optional fields of an API binding request
type apiBindOptions struct {
	HostedDispatch map[string]any
	DeclaredKey    string
	CallerKey      string
	WalkedFrom     string
}

// Session a new turn on a retained chain lands on, its stored id or the next generation once deleted
func continuedAPISession(ctx context.Context, runner any, sessionID, requestID string) (string, error) {
	if sessionID == "" {
		return sessionID, nil
	}
	authority, err := activeAuthority(ctx, runner)
	if err != nil {
		return "", err
	}
	if authority == nil {
		return sessionID, nil
	}
	return implicitAPISession(ctx, authority.DB, sessionID, requestID)
}

// Pins the shared listener identity on an API source, returning the transport profile to persist
func pinAPIIdentity(ctx context.Context, runner any, source *SessionSource, transportProfile *string) (*string, error) {
	if transportProfile == nil {
		name := "default"
		if primary, ok := runner.(identityPrimaryRunner); ok && primary.PrimaryProfileName() != "" {
			name = primary.PrimaryProfileName()
		}
		transportProfile = &name
	}
	identity, err := restoreIdentity(ctx, source, runner, transportProfile)
	if err != nil || identity == nil {
		return nil, err
	}
	return &identity.TransportProfile, nil
}

// Drives a write step sequence inline, each write run by the caller
func runSteps(ctx context.Context, steps writeSteps) (any, error) {
	return steps(func(write writeStep) (any, error) { return write(ctx) })
}

// Drives a write step sequence as one tracked task, each write off the caller in admission order
func runStepsOffLoop(ctx context.Context, a *SessionAuthority, steps writeSteps, orphaned func(result any)) (any, error) {
	task := trackMutation(a, func(taskCtx context.Context) (any, error) {
		a.apiAdmissions.Lock()
		defer a.apiAdmissions.Unlock()
		return steps(func(write writeStep) (any, error) {
			return trackedWrite(taskCtx, a, write, nil, true, nil)
		})
	})
	select {
	case <-task.done:
		return task.result, task.err
	case <-ctx.Done():
		if orphaned != nil {
			go func() {
				if result, err := task.wait(); err == nil {
					orphaned(result)
				}
			}()
		}
		return nil, ctx.Err()
	}
}

// Reserves an API source for a session from the authenticated API edge, driving its binding inline
func bindAPISession(ctx context.Context, a *SessionAuthority, sessionID string, opts apiBindOptions) (SessionRef, error) {
	result, err := runSteps(ctx, bindAPISteps(ctx, a, sessionID, opts))
	if err != nil {
		return SessionRef{}, err
	}
	return result.(SessionRef), nil
}

// True when a state_meta key holds a value
func metaKeyHeld(ctx context.Context, conn state.SQLiteConn, key string) (bool, error) {
	var one int
	err := conn.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key=?", key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Value of a state_meta key, false when absent
func metaValue(ctx context.Context, conn state.SQLiteConn, key string) (string, bool, error) {
	var value sql.NullString
	err := conn.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value.String, err == nil, err
}

// Decoded JSON object of stored text, the error Python raises for any other shape
func loadsObject(text string) (*pyjson.Object, error) {
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(decoded) + "' object is not subscriptable"}
	}
	return obj, nil
}

// Binding of an API session as write steps, the binding transaction yielded and the session restored after it
func bindAPISteps(ctx context.Context, a *SessionAuthority, sessionID string, opts apiBindOptions) writeSteps {
	return func(run stepRunner) (any, error) {
		if err := a.requireAdmissionOpen(false); err != nil {
			return nil, err
		}
		if sessionID == "" || isPathUnsafe(sessionID, true) {
			return nil, runtimeRefusal("invalid_params")
		}
		storageSource := "api_server"
		var title *string
		var roomIdentity []string
		roomGeneration := 0
		if opts.HostedDispatch != nil {
			identity, err := hostedMemberIdentity(opts.HostedDispatch)
			if err != nil {
				return nil, err
			}
			roomIdentity = identity
			generation, expected, err := hostedMemberSession(ctx, a.DB, roomIdentity)
			if err != nil {
				return nil, err
			}
			roomGeneration = generation
			if expected != sessionID {
				return nil, runtimeRefusal("admission_conflict")
			}
			storageSource = "bot_room"
			group := "Group: " + roomIdentity[1]
			title = &group
		}
		if live, held := a.live(sessionID); held && !isHistoryMount(live) {
			if live.Source == nil {
				return nil, &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'platform'"}
			}
			denied := live.Source.Platform != PlatformAPIServer
			if !denied {
				row, err := a.DB.GetSession(ctx, sessionID)
				if err != nil {
					return nil, err
				}
				if row == nil {
					return nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
				}
				stored, _ := row.Get("source")
				denied = stored != any(storageSource)
			}
			if denied {
				return nil, runtimeRefusal("permission_denied")
			}
		}
		user := "api"
		source := &SessionSource{Platform: PlatformAPIServer, ChatID: sessionID, UserID: &user, ChatType: "dm"}
		if name, served := servedProfile(a); served {
			source.Profile = &name
		}
		transportProfile, err := pinAPIIdentity(ctx, a.Runner, source, nil)
		if err != nil {
			return nil, err
		}
		route, err := a.Runner.SessionStore().generateSessionKey(ctx, source, nil)
		if err != nil {
			return nil, err
		}
		now := sessionNow()
		entry := NewSessionEntry(route, sessionID, now, now)
		entry.Origin, entry.Platform, entry.TransportProfile = source, PlatformAPIServer, transportProfile
		receipt := pyjson.ObjectOf("profile_id", a.ProfileID, "session_id", sessionID, "route", route, "entry", entry.ToDict())
		if opts.DeclaredKey != "" {
			receipt.Set("declared_key", opts.DeclaredKey)
		}
		if roomIdentity != nil {
			members := make([]any, len(roomIdentity))
			for i, part := range roomIdentity {
				members[i] = part
			}
			receipt.Set("storage_source", storageSource)
			receipt.Set("room_identity", members)
			if roomGeneration != 0 {
				receipt.Set("room_generation", roomGeneration)
			}
		}
		write := func(ctx context.Context, conn state.SQLiteConn) error {
			if err := state.RuntimeEpoch(ctx, conn, a.Epoch); err != nil {
				return err
			}
			if err := a.requireAdmissionOpen(false); err != nil {
				return err
			}
			if retired, err := metaKeyHeld(ctx, conn, state.RetiredPrefix+sessionID); err != nil {
				return err
			} else if retired {
				return runtimeRefusal("not_found")
			}
			if opts.WalkedFrom != "" {
				kept, err := state.KeepPassedFences(ctx, conn, func(n int) string { return apiGenerationID(opts.WalkedFrom, n) }, sessionID)
				if err != nil {
					return err
				}
				if !kept {
					return runtimeRefusal("chain_moved")
				}
			}
			saved, bound, err := metaValue(ctx, conn, state.LocalAPIBindingPrefix+sessionID)
			if err != nil {
				return err
			}
			if bound {
				binding, err := loadsObject(saved)
				if err != nil {
					return err
				}
				held, has := binding.Get("storage_source")
				if !has {
					held = "api_server"
				}
				if held != any(storageSource) {
					return runtimeRefusal("permission_denied")
				}
				if declared, _ := binding.Get("declared_key"); opts.DeclaredKey != "" && declared != any(opts.DeclaredKey) {
					return runtimeRefusal("admission_conflict")
				}
				return nil
			}
			if opts.DeclaredKey != "" {
				owner, held, err := metaValue(ctx, conn, state.LocalAPIDeclaredPrefix+opts.DeclaredKey)
				if err != nil {
					return err
				}
				if held && owner != sessionID {
					return runtimeRefusal("admission_conflict")
				}
				if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING",
					state.LocalAPIDeclaredPrefix+opts.DeclaredKey, sessionID); err != nil {
					return err
				}
			}
			var rowSourceLabel, rowKey, rowTitle sql.NullString
			exists := true
			err = conn.QueryRowContext(ctx, "SELECT source,session_key,title FROM sessions WHERE id=?", sessionID).Scan(&rowSourceLabel, &rowKey, &rowTitle)
			if errors.Is(err, sql.ErrNoRows) {
				exists = false
			} else if err != nil {
				return err
			}
			if exists && (!rowSourceLabel.Valid || rowSourceLabel.String != storageSource) {
				return runtimeRefusal("permission_denied")
			}
			if storageSource == "bot_room" {
				if exists && (!rowTitle.Valid || rowTitle.String != *title) {
					return runtimeRefusal("admission_conflict")
				}
				var one int
				err := conn.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE title=? AND id!=?", *title, sessionID).Scan(&one)
				if err == nil {
					return runtimeRefusal("admission_conflict")
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			legacyKey := opts.DeclaredKey
			if legacyKey == "" {
				legacyKey = opts.CallerKey
			}
			if exists && rowKey.Valid && rowKey.String != "" && rowKey.String != route {
				if legacyKey == "" || rowKey.String != legacyKey {
					return runtimeRefusal("admission_conflict")
				}
				owner, held, err := metaValue(ctx, conn, state.LocalAPIDeclaredPrefix+legacyKey)
				if err != nil {
					return err
				}
				if held && owner != sessionID {
					return runtimeRefusal("admission_conflict")
				}
				if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING",
					state.LocalAPIDeclaredPrefix+legacyKey, sessionID); err != nil {
					return err
				}
				receipt.Set("declared_key", legacyKey)
			}
			holder := sessionID
			var existing sql.NullString
			err = conn.QueryRowContext(ctx, "SELECT entry_json FROM gateway_routing WHERE scope='' AND session_key=?", route).Scan(&existing)
			if err == nil {
				routed, err := loadsObject(existing.String)
				if err != nil {
					return err
				}
				named, has := routed.Get("session_id")
				if !has {
					return &pyKeyError{key: "session_id"}
				}
				holder = pyStrOf(named)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if holder != sessionID {
				healed, err := healReplacement(ctx, conn, sessionID, holder)
				if err != nil {
					return err
				}
				if !healed {
					return runtimeRefusal("admission_conflict")
				}
			}
			hidden := 0
			if storageSource == "bot_room" {
				hidden = 1
			}
			var titleValue any
			if title != nil {
				titleValue = *title
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO sessions(id,source,title,hidden,started_at) VALUES(?,?,?,?,?)
                        ON CONFLICT(id) DO NOTHING`, sessionID, storageSource, titleValue, hidden, now.timestamp()); err != nil {
				return err
			}
			originJSON, err := state.RuntimeJSON(source.ToDict())
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET session_key=?,chat_id=?,user_id=?,chat_type=?,origin_json=? WHERE id=?",
				route, sessionID, user, "dm", originJSON, sessionID); err != nil {
				return err
			}
			entryJSON, err := state.RuntimeJSON(entry.ToDict())
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('',?,?,?) "+
				"ON CONFLICT(scope,session_key) DO UPDATE SET entry_json=excluded.entry_json", route, entryJSON, now.timestamp()); err != nil {
				return err
			}
			receiptJSON, err := state.RuntimeJSON(receipt)
			if err != nil {
				return err
			}
			_, err = conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", state.LocalAPIBindingPrefix+sessionID, receiptJSON)
			return err
		}
		if _, err := run(func(ctx context.Context) (any, error) {
			return nil, a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error { return write(ctx, conn) })
		}); err != nil {
			return nil, err
		}
		if err := restoreAPISession(ctx, a, sessionID); err != nil {
			return nil, err
		}
		return SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}, nil
	}
}

// True when the caller may take its route back from a heal replacement holding no binding
func healReplacement(ctx context.Context, conn state.SQLiteConn, caller, holder string) (bool, error) {
	bound, err := metaKeyHeld(ctx, conn, state.LocalAPIBindingPrefix+holder)
	if err != nil || bound {
		return false, err
	}
	slog.Warn(fmt.Sprintf("API route of %s taken back from heal replacement %s", caller, holder))
	return true, nil
}

// Reopens the finalized row an admitted API turn runs on
func reopenAPISession(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	target, err := a.PhysicalTarget(ctx, ref)
	if err != nil {
		return err
	}
	row, err := a.DB.GetSession(ctx, target)
	if err != nil || row == nil {
		return err
	}
	if reason, _ := row.Get("end_reason"); reason != nil {
		return a.DB.ReopenSession(ctx, target)
	}
	return nil
}

// Restores an API session from its binding receipt into the authority, its route entry and live session
func restoreAPISession(ctx context.Context, a *SessionAuthority, sessionID string) error {
	var saved string
	var bound bool
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var err error
		saved, bound, err = metaValue(ctx, conn, state.LocalAPIBindingPrefix+sessionID)
		return err
	})
	if err != nil {
		return err
	}
	if !bound {
		return runtimeRefusal("not_found")
	}
	receipt, err := loadsObject(saved)
	if err != nil {
		return err
	}
	profileID, err := receiptField(receipt, "profile_id")
	if err != nil {
		return err
	}
	if profileID != any(a.ProfileID) {
		return runtimeRefusal("profile_mismatch")
	}
	rawEntry, err := receiptField(receipt, "entry")
	if err != nil {
		return err
	}
	entryDict, ok := rawEntry.(*pyjson.Object)
	if !ok {
		return &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(rawEntry) + "' object has no attribute 'get'"}
	}
	entry, err := SessionEntryFromDict(ctx, entryDict)
	if err != nil {
		return err
	}
	source := entry.Origin
	if source == nil {
		return &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'profile'"}
	}
	if _, err := pinAPIIdentity(ctx, a.Runner, source, entry.TransportProfile); err != nil {
		return err
	}
	row, err := a.DB.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	owner, err := receiptField(receipt, "session_id")
	if err != nil {
		return err
	}
	conflict := owner != any(sessionID) || entry.SessionID != sessionID || source.Platform != PlatformAPIServer ||
		source.ChatID != sessionID || row == nil
	storage, labelled := receipt.Get("storage_source")
	if !conflict {
		expected := storage
		if !labelled {
			expected = "api_server"
		}
		label, _ := row.Get("source")
		key, _ := row.Get("session_key")
		chat, _ := row.Get("chat_id")
		user, _ := row.Get("user_id")
		conflict = label != expected || key != any(entry.SessionKey) || chat != any(sessionID) || user != optStrAny(source.UserID)
	}
	if !conflict {
		route, err := a.Runner.SessionStore().generateSessionKey(ctx, source, nil)
		if err != nil {
			return err
		}
		conflict = entry.SessionKey != route
	}
	if conflict {
		return runtimeRefusal("admission_conflict")
	}
	if storage == any("bot_room") {
		rawIdentity, err := receiptField(receipt, "room_identity")
		if err != nil {
			return err
		}
		parts, isList := rawIdentity.([]any)
		if !isList {
			return &PyError{Kind: "TypeError", Msg: "can only join an iterable"}
		}
		identity := make([]string, len(parts))
		for i, part := range parts {
			text, isText := part.(string)
			if !isText {
				return &PyError{Kind: "TypeError", Msg: fmt.Sprintf("sequence item %d: expected str instance, %s found", i, jsonValueTypeName(part))}
			}
			identity[i] = text
		}
		generation := 0
		if raw, has := receipt.Get("room_generation"); has {
			if n := rowInt(pyjson.ObjectOf("n", raw), "n"); n != nil {
				generation = int(*n)
			}
		}
		if len(identity) < 2 {
			return &PyError{Kind: "IndexError", Msg: "list index out of range"}
		}
		title, _ := row.Get("title")
		hidden, _ := row.Get("hidden")
		if roomSessionID(identity, generation) != sessionID || title != any("Group: "+identity[1]) || !pyTruthy(hidden) {
			return runtimeRefusal("admission_conflict")
		}
	}
	store := a.Runner.SessionStore()
	target, err := a.PhysicalTarget(ctx, SessionRef{ProfileID: a.ProfileID, SessionID: sessionID})
	if err != nil {
		return err
	}
	entry.SessionID = target
	store.mu.Lock()
	store.ensureLoadedLocked(ctx)
	current := store.entries.get(entry.SessionKey)
	if current != nil {
		lineage, err := a.DB.GetCompressionLineage(ctx, sessionID)
		if err != nil {
			store.mu.Unlock()
			return err
		}
		named := false
		for _, id := range lineage {
			named = named || id == current.SessionID
		}
		if !named {
			healed := false
			err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
				var err error
				healed, err = healReplacement(ctx, conn, sessionID, current.SessionID)
				return err
			})
			if err != nil || !healed {
				store.mu.Unlock()
				if err != nil {
					return err
				}
				return runtimeRefusal("admission_conflict")
			}
		}
	}
	if current != nil && current.SessionID == target {
		store.entries.set(entry.SessionKey, current)
	} else {
		store.entries.set(entry.SessionKey, entry)
	}
	store.mu.Unlock()
	a.mu.Lock()
	if live := a.Sessions[sessionID]; live == nil {
		a.Sessions[sessionID] = NewLiveSession(source, entry.SessionKey)
	} else if isHistoryMount(live) {
		live.Source, live.Route = source, entry.SessionKey
	}
	a.mu.Unlock()
	return nil
}
