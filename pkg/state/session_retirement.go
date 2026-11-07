package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	TerminalResultPrefix    = "gateway.admission.result.v1."
	TerminalAdmissionPrefix = "gateway.terminal_admission.v1."
	TerminalIdentityPrefix  = "gateway.terminal_identity.v1."
	TerminalWorkerPrefix    = "gateway.terminal_worker.v1."
	terminalStoredKey       = "_stored"

	MediaRetiredPrefix     = "gateway.media.retired.v1."
	SpilloverRetiredPrefix = "gateway.spillover.retired.v1."

	RouteFencePrefix       = "gateway.retired_route.v1."
	ViewerClosePrefix      = "gateway.viewer_close.v1:"
	LocalAPIBindingPrefix  = "gateway.api.binding.v1."
	LocalAPIDeclaredPrefix = "gateway.api.conversation.v1."
	retryHorizonS          = 7 * 86400
)

// Hooks into pkg/gateway and pkg/tools that the process wiring assigns, since both import this package
var (
	AdmissionMediaReferences func(payload *pyjson.Object) ([]*pyjson.Object, error)
	HostedDocumentReferences func(requestID string, payload *pyjson.Object) ([]*pyjson.Object, error)
	ReleaseUnheldMedia       func(db *SessionDB, references []any) error
	SpilloverSQLFilter       string
	SpilloverNamesInText     func(texts ...string) map[string]bool
	ReferencedSpilloverNames func(spillDir string) (map[string]bool, bool)
)

// Panics naming the hook the process wiring left unassigned
func requireHook(set bool, name, owner string) {
	if !set {
		panic("state." + name + " unassigned: the process wiring installs " + owner)
	}
}

var terminalTranscriptFields = map[string]bool{"messages": true, "last_reasoning": true, "tools": true, "_turn_user_row": true}

var mutationTranscriptFields = []string{"target_message", "summary", "title"}

var receiptSessionPaths = []string{"$.result.session_id", "$.result.target_message.session_id", "$.result.target_session_id", "$.result.previous_target_session_id"}

const liveLedgerSQL = `SELECT 1 FROM session_admissions WHERE target_session_id=? AND status IN ('queued','started','unknown')
    UNION ALL SELECT 1 FROM worker_executions WHERE session_id=? AND status IN ('registered','running','unknown') LIMIT 1`

var derivedAPISQL = "(source='bot_room' OR (source='api_server' AND id GLOB 'api-" + strings.Repeat("[0-9a-f]", 16) + "'))"

// Object whose keys are exactly the given names
func objectKeysAre(o *pyjson.Object, names ...string) bool {
	if o.Len() != len(names) {
		return false
	}
	for _, n := range names {
		if _, ok := o.Get(n); !ok {
			return false
		}
	}
	return true
}

// Stored form of a terminal result keeping the closing reply once through a reply_ref envelope
func encodeTerminalResult(result any) any {
	obj, ok := result.(*pyjson.Object)
	if !ok {
		return result
	}
	if raw, has := obj.Get(terminalStoredKey); has {
		out, env := obj.Copy(), pyjson.NewObject()
		env.Set("v", int64(1))
		env.Set("raw", raw)
		out.Set(terminalStoredKey, env)
		return out
	}
	value, _ := obj.Get("result")
	vobj, ok := value.(*pyjson.Object)
	if !ok {
		return result
	}
	rawMessages, _ := vobj.Get("messages")
	list, ok := rawMessages.([]any)
	if !ok {
		return result
	}
	messages := slices.Clone(list)
	reply, _ := vobj.Get("final_response")
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if m, ok := messages[i].(*pyjson.Object); ok {
			if role, _ := m.Get("role"); role == "assistant" {
				last = i
				break
			}
		}
	}
	text, isText := reply.(string)
	if !isText || text == "" || last < 0 {
		return result
	}
	closing := messages[last].(*pyjson.Object)
	if content, _ := closing.Get("content"); !pyjson.Equal(content, text) {
		return result
	}
	stripped := closing.Copy()
	stripped.Set("content", nil)
	messages[last] = stripped
	nv, out, env := vobj.Copy(), obj.Copy(), pyjson.NewObject()
	nv.Set("messages", messages)
	out.Set("result", nv)
	env.Set("v", int64(1))
	env.Set("reply_ref", int64(last))
	out.Set(terminalStoredKey, env)
	return out
}

// Decodes a stored terminal result, only an exact encodeTerminalResult envelope being unwrapped
func loadTerminalResult(text string) (any, error) {
	result, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	obj, ok := result.(*pyjson.Object)
	if !ok {
		return result, nil
	}
	rawEnv, _ := obj.Get(terminalStoredKey)
	env, ok := rawEnv.(*pyjson.Object)
	if !ok {
		return result, nil
	}
	version, _ := env.Get("v")
	if objectKeysAre(env, "v", "raw") && version == int64(1) {
		raw, _ := env.Get("raw")
		obj.Set(terminalStoredKey, raw)
		return obj, nil
	}
	value, _ := obj.Get("result")
	ref, _ := env.Get("reply_ref")
	vobj, _ := value.(*pyjson.Object)
	var messages []any
	listed := false
	if vobj != nil {
		raw, _ := vobj.Get("messages")
		messages, listed = raw.([]any)
	}
	idx, isInt := ref.(int64)
	if !objectKeysAre(env, "v", "reply_ref") || version != int64(1) || !isInt || !listed || idx < 0 || idx >= int64(len(messages)) {
		return result, nil
	}
	reply, _ := vobj.Get("final_response")
	text2, isText := reply.(string)
	msg, isObj := messages[idx].(*pyjson.Object)
	if !isText || !isObj {
		return result, nil
	}
	role, _ := msg.Get("role")
	content, has := msg.Get("content")
	if role != "assistant" || !has || content != nil {
		return result, nil
	}
	restored := msg.Copy()
	restored.Set("content", text2)
	messages[idx] = restored
	obj.Delete(terminalStoredKey)
	return obj, nil
}

// State meta key holding the terminal admission id of one principal request
func terminalIdentityKey(principalID, sessionID, requestID any) (string, error) {
	fp, err := AdmissionFingerprint(pyStr(sessionID), map[string]any{"principal": principalID, "request": requestID})
	if err != nil {
		return "", err
	}
	return TerminalIdentityPrefix + fp, nil
}

// Records the media an admission payload references as durable deletion candidates
func retireMedia(ctx context.Context, conn SQLiteConn, payload *pyjson.Object, requestID string) error {
	requireHook(AdmissionMediaReferences != nil, "AdmissionMediaReferences", "gateway admission_media_references")
	requireHook(HostedDocumentReferences != nil, "HostedDocumentReferences", "gateway hosted_document_references")
	refs, err := AdmissionMediaReferences(payload)
	if err != nil {
		return err
	}
	hosted, err := HostedDocumentReferences(requestID, payload)
	if err != nil {
		return err
	}
	refs = append(refs, hosted...)
	if turn, has := payload.Get("api_turn_v1"); has {
		tobj, ok := turn.(*pyjson.Object)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(turn))
		}
		if media, has := tobj.Get("media"); has {
			list, ok := media.([]any)
			if !ok {
				return fmt.Errorf("'%s' object is not iterable", pyTypeName(media))
			}
			for _, m := range list {
				mo, ok := m.(*pyjson.Object)
				if !ok {
					return fmt.Errorf("'%s' object is not subscriptable", pyTypeName(m))
				}
				refs = append(refs, mo)
			}
		}
	}
	for _, ref := range refs {
		path, has := ref.Get("path")
		ps, ok := path.(string)
		if !has || !ok {
			return fmt.Errorf("media reference without a str path: %s", pyReprValue(path))
		}
		sum := sha256.Sum256([]byte(ps))
		text, err := pyjson.Dumps(ref)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta(key,value) VALUES(?,?)", MediaRetiredPrefix+hex.EncodeToString(sum[:]), text); err != nil {
			return err
		}
	}
	return nil
}

// Records the spillover archives the doomed sessions' messages point at inside the delete transaction
func retireSpillover(ctx context.Context, conn SQLiteConn, sessionIDs []string) error {
	requireHook(SpilloverSQLFilter != "", "SpilloverSQLFilter", "tools.SpilloverSQLFilter")
	requireHook(SpilloverNamesInText != nil, "SpilloverNamesInText", "tools.SpilloverNamesInText")
	for _, chunk := range idChunks(sessionIDs, sqlInChunk) {
		rows, err := connRows(ctx, conn, "SELECT content, api_content FROM messages WHERE session_id IN ("+placeholders(len(chunk))+") AND "+SpilloverSQLFilter, stringsToAny(chunk)...)
		if err != nil {
			return err
		}
		for _, row := range rows {
			content, _ := row["content"].(string)
			apiContent, _ := row["api_content"].(string)
			names := SpilloverNamesInText(content, apiContent)
			sorted := make([]string, 0, len(names))
			for n := range names {
				sorted = append(sorted, n)
			}
			slices.Sort(sorted)
			for _, n := range sorted {
				text, err := pyjson.Dumps(n)
				if err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta(key,value) VALUES(?,?)", SpilloverRetiredPrefix+n, text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// State meta rows under a key prefix through the primary key range
func (d *SessionDB) metaPrefixRows(ctx context.Context, prefix string) ([]map[string]any, error) {
	lo, hi, err := PrefixRange(prefix)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	err = d.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		rows, err = connRows(ctx, conn, "SELECT key,value FROM state_meta WHERE key>=? AND key<?", lo, hi)
		return err
	})
	return rows, err
}

// Deletes the given state meta keys in one write
func (d *SessionDB) deleteMetaKeys(ctx context.Context, keys []string) error {
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		for _, k := range keys {
			if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=?", k); err != nil {
				return err
			}
		}
		return nil
	}, nil)
}

// Unlinks retired spillover archives no surviving message references, keeping all when references are unknown
func (d *SessionDB) collectRetiredSpillover(ctx context.Context) error {
	requireHook(ReferencedSpilloverNames != nil, "ReferencedSpilloverNames", "tools.ReferencedSpilloverNames")
	rows, err := d.metaPrefixRows(ctx, SpilloverRetiredPrefix)
	if err != nil || len(rows) == 0 {
		return err
	}
	spillDir := filepath.Join(filepath.Dir(d.path), "cache", "spillover")
	referenced, known := ReferencedSpilloverNames(spillDir)
	if !known {
		return nil
	}
	var done []string
	for _, row := range rows {
		name, err := pyjson.Loads(pyStr(row["value"]))
		if err != nil {
			return err
		}
		ns, ok := name.(string)
		if !ok {
			return fmt.Errorf("unsupported operand type(s) for /: 'PosixPath' and '%s'", pyTypeName(name))
		}
		if !referenced[ns] {
			if err := os.Remove(filepath.Join(spillDir, ns)); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		done = append(done, pyStr(row["key"]))
	}
	return d.deleteMetaKeys(ctx, done)
}

// Releases retired media no admission or retained transcript row owns and drops collected candidates
func (d *SessionDB) collectRetiredMedia(ctx context.Context) error {
	requireHook(ReleaseUnheldMedia != nil, "ReleaseUnheldMedia", "gateway release_unheld_media")
	if err := d.collectRetiredSpillover(ctx); err != nil {
		return err
	}
	rows, err := d.metaPrefixRows(ctx, MediaRetiredPrefix)
	if err != nil || len(rows) == 0 {
		return err
	}
	refs := make([]any, 0, len(rows))
	for _, row := range rows {
		v, err := pyjson.Loads(pyStr(row["value"]))
		if err != nil {
			return err
		}
		refs = append(refs, v)
	}
	if err := ReleaseUnheldMedia(d, refs); err != nil {
		return err
	}
	var gone []string
	for _, row := range rows {
		v, err := pyjson.Loads(pyStr(row["value"]))
		if err != nil {
			return err
		}
		o, ok := v.(*pyjson.Object)
		if !ok {
			return fmt.Errorf("'%s' object is not subscriptable", pyTypeName(v))
		}
		path, has := o.Get("path")
		if !has {
			return fmt.Errorf("KeyError: 'path'")
		}
		ps, ok := path.(string)
		if !ok {
			return fmt.Errorf("expected str, bytes or os.PathLike object, not %s", pyTypeName(path))
		}
		if ps == "" {
			ps = "."
		}
		if _, err := os.Stat(ps); err != nil {
			gone = append(gone, pyStr(row["key"]))
		}
	}
	if len(gone) == 0 {
		return nil
	}
	return d.deleteMetaKeys(ctx, gone)
}

// Session id a stored routing or receipt JSON object names, nil for anything else
func entrySessionID(raw any) any {
	text, ok := raw.(string)
	if !ok {
		return nil
	}
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	if o, ok := value.(*pyjson.Object); ok {
		v, _ := o.Get("session_id")
		return v
	}
	return nil
}

// Membership of a decoded value in a string set, refusing unhashable values as Python does
func hashableIn(v any, set map[string]bool) (bool, error) {
	switch x := v.(type) {
	case []any, *pyjson.Object:
		return false, fmt.Errorf("unhashable type: '%s'", pyTypeName(x))
	case string:
		return set[x], nil
	}
	return false, nil
}

// Compacts a retired admission result to its exact retry outcome without transcript copies
func compactTerminalResult(ctx context.Context, conn SQLiteConn, admissionID string) error {
	saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", TerminalResultPrefix+admissionID)
	if err != nil || saved == nil {
		return err
	}
	value, err := loadTerminalResult(pyStr(saved["value"]))
	if err != nil {
		return err
	}
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(value))
	}
	if result, _ := obj.Get("result"); result != nil {
		if ro, ok := result.(*pyjson.Object); ok {
			kept := pyjson.NewObject()
			for _, k := range ro.Keys {
				if !terminalTranscriptFields[k] {
					kept.Set(k, ro.Vals[k])
				}
			}
			kept.Set("messages", []any{})
			obj.Set("result", kept)
		}
	}
	text, err := runtimeJSON(encodeTerminalResult(obj))
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", text, TerminalResultPrefix+admissionID)
	return err
}

// Moves every terminal admission and worker of the sessions into compact retired receipts, refusing live work
func retireTerminalReceipts(ctx context.Context, conn SQLiteConn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		admissions, err := queryObjects(ctx, conn, "SELECT * FROM session_admissions WHERE target_session_id=?", sid)
		if err != nil {
			return err
		}
		workers, err := queryObjects(ctx, conn, "SELECT * FROM worker_executions WHERE session_id=?", sid)
		if err != nil {
			return err
		}
		busy, unknown := false, false
		for _, r := range append(slices.Clone(admissions), workers...) {
			if st := rowValue(r, "status"); st != "terminal" {
				busy = true
				unknown = unknown || st == "unknown"
			}
		}
		if busy {
			if unknown {
				return runtimeRefusal("unknown_execution")
			}
			return runtimeRefusal("session_busy")
		}
		for _, row := range admissions {
			payload, err := pyjson.Loads(pyStr(rowValue(row, "payload_json")))
			if err != nil {
				return err
			}
			po, ok := payload.(*pyjson.Object)
			if !ok {
				return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(payload))
			}
			if err := retireMedia(ctx, conn, po, pyStr(rowValue(row, "request_id"))); err != nil {
				return err
			}
			row.Set("payload_json", "{}")
			row.Set("lineage_json", "[]")
			row.Set("retired_at", now())
			admissionID := pyStr(rowValue(row, "admission_id"))
			if err := compactTerminalResult(ctx, conn, admissionID); err != nil {
				return err
			}
			text, err := runtimeJSON(row)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", TerminalAdmissionPrefix+admissionID, text); err != nil {
				return err
			}
			key, err := terminalIdentityKey(rowValue(row, "principal_id"), sid, rowValue(row, "request_id"))
			if err != nil {
				return err
			}
			idText, err := pyjson.Dumps(rowValue(row, "admission_id"))
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, idText); err != nil {
				return err
			}
		}
		closing, err := AdmissionFingerprint(sid, map[string]any{"operation": "execution.finish", "payload": map[string]any{}})
		if err != nil {
			return err
		}
		for _, row := range workers {
			executionID := rowValue(row, "execution_id")
			receipts, err := queryObjects(ctx, conn, "SELECT sequence,payload_digest,result_json FROM worker_receipts WHERE execution_id=? ORDER BY sequence", executionID)
			if err != nil {
				return err
			}
			list := make([]any, 0, len(receipts))
			for _, r := range receipts {
				if rowValue(r, "payload_digest") != closing {
					r.Set("result_json", nil)
				}
				list = append(list, r)
			}
			row.Set("receipts", list)
			row.Set("retired_at", now())
			text, err := runtimeJSON(row)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", TerminalWorkerPrefix+pyStr(executionID), text); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "DELETE FROM worker_receipts WHERE execution_id=?", executionID); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM worker_executions WHERE session_id=?", sid); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM session_admissions WHERE target_session_id=?", sid); err != nil {
			return err
		}
	}
	return nil
}

// Complete deletion fence for ids deleted in this transaction, expiring ids aging out at the retry horizon
func retireSessions(ctx context.Context, conn SQLiteConn, sessionIDs, expiring []string) error {
	if err := retireSpillover(ctx, conn, sessionIDs); err != nil {
		return err
	}
	if err := retireTerminalReceipts(ctx, conn, sessionIDs); err != nil {
		return err
	}
	if err := retireMutationReceipts(ctx, conn, sessionIDs); err != nil {
		return err
	}
	if err := retireRoutes(ctx, conn, sessionIDs, expiring); err != nil {
		return err
	}
	return retireLocalReceipts(ctx, conn, sessionIDs)
}

// Strips the user content of mutation receipts naming the sessions while keeping their exact retry identity
func retireMutationReceipts(ctx context.Context, conn SQLiteConn, sessionIDs []string) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	named := make([]string, len(receiptSessionPaths))
	for i, p := range receiptSessionPaths {
		named[i] = "json_extract(value,'" + p + "') IN (SELECT value FROM json_each(?1))"
	}
	ids, err := pyjson.Dumps(stringsToAny(sessionIDs))
	if err != nil {
		return err
	}
	rows, err := connRows(ctx, conn, "SELECT key,value FROM state_meta WHERE key GLOB 'gateway.mutation.v1.*' AND "+
		"CASE WHEN json_valid(value) THEN ("+strings.Join(named, " OR ")+") END", ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		receipt, err := pyjson.Loads(pyStr(row["value"]))
		if err != nil {
			return err
		}
		ro, ok := receipt.(*pyjson.Object)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(receipt))
		}
		result, _ := ro.Get("result")
		res, ok := result.(*pyjson.Object)
		if !ok || !slices.ContainsFunc(mutationTranscriptFields, func(f string) bool { _, has := res.Get(f); return has }) {
			continue
		}
		stripped := pyjson.NewObject()
		for _, k := range res.Keys {
			if slices.Contains(mutationTranscriptFields, k) {
				stripped.Set(k, nil)
			} else {
				stripped.Set(k, res.Vals[k])
			}
		}
		ro.Set("result", stripped)
		text, err := runtimeJSON(ro)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", text, row["key"]); err != nil {
			return err
		}
	}
	return nil
}

// Writes a retirement marker, a sweep marker never shortening a permanent one and unscoped markers staying unscoped
func markRetired(ctx context.Context, conn SQLiteConn, key string, expires, own bool) error {
	value := pyjson.NewObject()
	value.Set("retired_at", now())
	if own {
		value.Set("own", true)
	}
	if expires {
		value.Set("expires", true)
		text, err := pyjson.Dumps(value)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE "+
			"SET value=excluded.value WHERE json_valid(state_meta.value) "+
			"AND json_extract(state_meta.value, '$.expires')", key, text)
		return err
	}
	text, err := pyjson.Dumps(value)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=CASE "+
		"WHEN NOT json_valid(state_meta.value) THEN json_remove(excluded.value, '$.own') "+
		"WHEN json_extract(state_meta.value, '$.own') IS NOT NULL "+
		"OR json_extract(state_meta.value, '$.expires') THEN excluded.value "+
		"ELSE json_remove(excluded.value, '$.own') END", key, text)
	return err
}

// Marks the sessions retired, drops their API settings and viewer closes and every route naming them
func retireRoutes(ctx context.Context, conn SQLiteConn, sessionIDs, expiring []string) error {
	targets := map[string]bool{}
	for _, sid := range sessionIDs {
		if err := markRetired(ctx, conn, RetiredPrefix+sid, slices.Contains(expiring, sid), true); err != nil {
			return err
		}
		for _, key := range []string{"gateway.api.settings.v1." + sid, ViewerClosePrefix + sid} {
			if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=?", key); err != nil {
				return err
			}
		}
		targets[sid] = true
	}
	rows, err := connRows(ctx, conn, "SELECT scope,session_key,entry_json FROM gateway_routing")
	if err != nil {
		return err
	}
	for _, row := range rows {
		hit, err := hashableIn(entrySessionID(row["entry_json"]), targets)
		if err != nil {
			return err
		}
		if hit {
			if _, err := conn.ExecContext(ctx, "DELETE FROM gateway_routing WHERE scope=? AND session_key=?", row["scope"], row["session_key"]); err != nil {
				return err
			}
		}
	}
	return nil
}

// Current physical target a creation receipt routes to, nil for a malformed receipt
func localPolicyTarget(raw any) any {
	text, ok := raw.(string)
	if !ok {
		return nil
	}
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	o, ok := value.(*pyjson.Object)
	if !ok {
		return nil
	}
	entry, _ := o.Get("entry")
	eo, ok := entry.(*pyjson.Object)
	if !ok {
		return nil
	}
	sid, _ := eo.Get("session_id")
	return sid
}

// Deletes the creation policies routed to the sessions, their API bindings and declared conversation keys
func retireLocalReceipts(ctx context.Context, conn SQLiteConn, sessionIDs []string) error {
	targets := map[string]bool{}
	for _, sid := range sessionIDs {
		targets[sid] = true
	}
	if len(targets) == 0 {
		return nil
	}
	var gone []string
	policies, err := connRows(ctx, conn, "SELECT key,value FROM state_meta WHERE key GLOB ?", LocalPolicyPrefix+"*")
	if err != nil {
		return err
	}
	for _, row := range policies {
		hit, err := hashableIn(localPolicyTarget(row["value"]), targets)
		if err != nil {
			return err
		}
		if hit {
			gone = append(gone, pyStr(row["key"]))
		}
	}
	declared, err := connRows(ctx, conn, "SELECT key,value FROM state_meta WHERE key GLOB ?", LocalAPIDeclaredPrefix+"*")
	if err != nil {
		return err
	}
	for _, row := range declared {
		if v, ok := row["value"].(string); ok && targets[v] {
			gone = append(gone, pyStr(row["key"]))
		}
	}
	for _, sid := range sessionIDs {
		gone = append(gone, LocalAPIBindingPrefix+sid)
	}
	for _, key := range gone {
		if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=?", key); err != nil {
			return err
		}
	}
	return nil
}

// Saved lineage list of a local creation policy, nil when absent or malformed
func localPolicyLineage(ctx context.Context, conn SQLiteConn, owner string) ([]any, bool, error) {
	saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+owner)
	if err != nil || saved == nil {
		return nil, saved != nil, err
	}
	text, ok := saved["value"].(string)
	if !ok {
		return nil, false, nil
	}
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil, false, nil
	}
	o, ok := value.(*pyjson.Object)
	if !ok {
		return nil, false, nil
	}
	lineage, _ := o.Get("lineage")
	list, ok := lineage.([]any)
	return list, ok, nil
}

// Creation id whose receipt lineage holds a physical segment, the segment itself otherwise
func localLineageOwner(ctx context.Context, conn SQLiteConn, sessionID string) (string, error) {
	row, err := connRow(ctx, conn, "SELECT chat_id FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return "", err
	}
	owner, _ := row["chat_id"].(string)
	if owner == "" || owner == sessionID {
		return sessionID, nil
	}
	lineage, ok, err := localPolicyLineage(ctx, conn, owner)
	if err != nil || !ok {
		return sessionID, err
	}
	if slices.ContainsFunc(lineage, func(v any) bool { return v == any(sessionID) }) {
		return owner, nil
	}
	return sessionID, nil
}

// Every row of the local conversation a session belongs to, its creation id first
func localConversationIDs(ctx context.Context, conn SQLiteConn, sessionID string) ([]string, error) {
	owner, err := localLineageOwner(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	lineage, ok, err := localPolicyLineage(ctx, conn, owner)
	if err != nil || !ok {
		return []string{sessionID}, err
	}
	out := orderedIDs{}
	out.add(owner)
	for _, v := range lineage {
		if s, ok := v.(string); ok {
			out.add(s)
		}
	}
	return out.list, nil
}

// The session alone outside a local lineage, else its whole local conversation with the session first
func ownedLineageIDs(ctx context.Context, conn SQLiteConn, sessionID string) ([]string, error) {
	ids, err := localConversationIDs(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	out := orderedIDs{}
	out.add(sessionID)
	out.add(ids...)
	return out.list, nil
}

// Insertion ordered string set
type orderedIDs struct {
	list []string
	seen map[string]bool
}

func (o *orderedIDs) add(ids ...string) {
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	for _, id := range ids {
		if !o.seen[id] {
			o.seen[id] = true
			o.list = append(o.list, id)
		}
	}
}

// Member id to every id of its local conversation, plus the ids of derived Bot Chat and A2A conversations
func localConversationsIndex(ctx context.Context, conn SQLiteConn) (map[string][]string, map[string]bool, error) {
	members, derived := map[string][]string{}, map[string]bool{}
	rows, err := connRows(ctx, conn, "SELECT value FROM state_meta WHERE key GLOB ?", LocalPolicyPrefix+"*")
	if err != nil {
		return nil, nil, err
	}
	for _, row := range rows {
		text, ok := row["value"].(string)
		if !ok {
			continue
		}
		value, err := pyjson.Loads(text)
		if err != nil {
			continue
		}
		receipt, ok := value.(*pyjson.Object)
		if !ok {
			continue
		}
		entry, _ := receipt.Get("entry")
		lineage, _ := receipt.Get("lineage")
		first, _ := receipt.Get("session_id")
		candidates := []any{first}
		if list, ok := lineage.([]any); ok {
			candidates = append(candidates, list...)
		}
		if eo, ok := entry.(*pyjson.Object); ok {
			sid, _ := eo.Get("session_id")
			candidates = append(candidates, sid)
		} else {
			candidates = append(candidates, nil)
		}
		var ids []string
		for _, c := range candidates {
			if s, ok := c.(string); ok {
				ids = append(ids, s)
			}
		}
		for _, id := range ids {
			members[id] = ids
		}
		request, _ := receipt.Get("request_id")
		label := ""
		if pyTruthy(request) {
			label = pyStr(request)
		}
		if strings.HasPrefix(label, "bot-chat:") || strings.HasPrefix(label, "a2a:") {
			for _, id := range ids {
				derived[id] = true
			}
		}
	}
	return members, derived, nil
}

// Ids among the sessions of derived API sessions
func derivedAPISessions(ctx context.Context, conn SQLiteConn, sessionIDs []string) (map[string]bool, error) {
	ids, err := pyjson.Dumps(stringsToAny(sessionIDs))
	if err != nil {
		return nil, err
	}
	found, err := queryStrings(ctx, conn, "SELECT id FROM sessions WHERE "+derivedAPISQL+" AND id IN (SELECT value FROM json_each(?))", ids)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, id := range found {
		out[id] = true
	}
	return out, nil
}

// Sweep fence that retires only idle whole conversations and returns those ids, derived ids keeping permanent markers
func retirePrunable(ctx context.Context, conn SQLiteConn, sessionIDs []string) ([]string, error) {
	swept := map[string]bool{}
	for _, sid := range sessionIDs {
		swept[sid] = true
	}
	conversations, derived, err := localConversationsIndex(ctx, conn)
	if err != nil {
		return nil, err
	}
	var quiet []string
	for _, sid := range sessionIDs {
		whole, ok := conversations[sid]
		if !ok {
			whole = []string{sid}
		}
		removable := true
		for _, x := range whole {
			if !swept[x] {
				removable = false
				break
			}
		}
		for _, x := range whole {
			if !removable {
				break
			}
			live, err := connRow(ctx, conn, liveLedgerSQL, x, x)
			if err != nil {
				return nil, err
			}
			removable = live == nil
		}
		if removable {
			quiet = append(quiet, sid)
		}
	}
	apiDerived, err := derivedAPISessions(ctx, conn, quiet)
	if err != nil {
		return nil, err
	}
	var expiring []string
	for _, sid := range quiet {
		if !derived[sid] && !apiDerived[sid] {
			expiring = append(expiring, sid)
		}
	}
	if err := retireSessions(ctx, conn, quiet, expiring); err != nil {
		return nil, err
	}
	return quiet, nil
}

// Expired retirement predicate over a state_meta value
const retiredExpiredSQL = "json_valid(value) AND json_extract(value, '$.retired_at') < ?"

// Drops the sweep expires markers under the prefix retired before the cutoff
func reclaimExpired(ctx context.Context, conn SQLiteConn, prefix string, cutoff float64) error {
	_, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key GLOB ? AND "+retiredExpiredSQL+" AND json_extract(value, '$.expires')", prefix+"*", cutoff)
	return err
}

// Reclaims terminal admissions, workers and sweep markers retired longer ago than olderThan, user delete fences kept
func pruneRetiredTombstones(ctx context.Context, conn SQLiteConn, olderThan float64) error {
	at := now()
	for _, prefix := range []string{TerminalAdmissionPrefix, TerminalWorkerPrefix} {
		if _, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=json_set(value, '$.retired_at', ?) WHERE key GLOB ? "+
			"AND json_valid(value) AND json_extract(value, '$.retired_at') IS NULL", at, prefix+"*"); err != nil {
			return err
		}
	}
	cutoff := at - olderThan
	rows, err := queryObjects(ctx, conn, "SELECT key,value FROM state_meta WHERE key GLOB ? AND "+retiredExpiredSQL, TerminalAdmissionPrefix+"*", cutoff)
	if err != nil {
		return err
	}
	for _, r := range rows {
		decoded, err := pyjson.Loads(pyStr(rowValue(r, "value")))
		if err != nil {
			return err
		}
		row, _ := decoded.(*pyjson.Object)
		field := func(name string) (any, error) {
			if row == nil {
				return nil, &pyint.Error{Kind: "TypeError", Msg: "list indices must be integers or slices, not str"}
			}
			v, ok := row.Get(name)
			if !ok {
				return nil, &pyint.Error{Kind: "KeyError", Msg: "'" + name + "'"}
			}
			return v, nil
		}
		admission, err := field("admission_id")
		if err != nil {
			return err
		}
		admissionID, isStr := admission.(string)
		if !isStr {
			return &pyint.Error{Kind: "TypeError", Msg: "can only concatenate str (not \"" + pyTypeName(admission) + "\") to str"}
		}
		var parts [3]any
		for i, name := range []string{"principal_id", "target_session_id", "request_id"} {
			if parts[i], err = field(name); err != nil {
				return err
			}
		}
		identity, err := terminalIdentityKey(parts[0], parts[1], parts[2])
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key IN (?,?,?)", rowValue(r, "key"), TerminalResultPrefix+admissionID, identity); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key GLOB ? AND "+retiredExpiredSQL, TerminalWorkerPrefix+"*", cutoff); err != nil {
		return err
	}
	for _, prefix := range []string{RetiredPrefix, RouteFencePrefix} {
		if err := reclaimExpired(ctx, conn, prefix, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// True when this store holds the retired marker of the session id
func RetiredIn(ctx context.Context, conn SQLiteConn, sessionID string) (bool, error) {
	row, err := connRow(ctx, conn, "SELECT 1 AS one FROM state_meta WHERE key=?", RetiredPrefix+sessionID)
	return row != nil, err
}

// True when the session id was retired in this store, read through a pooled reader
func (d *SessionDB) RetiredSession(ctx context.Context, sessionID string) (bool, error) {
	var retired bool
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		retired, err = RetiredIn(ctx, conn, sessionID)
		return err
	})
	return retired, err
}

// Refusal code and message for creating an explicit id that is live or retired, empty strings when the create may proceed
func CreateConflict(ctx context.Context, conn SQLiteConn, sessionID string) (string, string, error) {
	row, err := connRow(ctx, conn, "SELECT 1 AS one FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return "", "", err
	}
	if row != nil {
		return "session_exists", "Session already exists", nil
	}
	retired, err := RetiredIn(ctx, conn, sessionID)
	if err != nil || !retired {
		return "", "", err
	}
	return "session_deleted", "Session was deleted; its id cannot be reused", nil
}

// Probe over one derived session id inside the read or write that walks the generations
type GenerationProbe func(ctx context.Context, conn SQLiteConn, sessionID string) (bool, error)

// First derived generation a new request may use, skipping retired ids unless pinned and live ids spent
func (d *SessionDB) FirstGeneration(ctx context.Context, derive func(int) string, pinned, spent GenerationProbe) (int, string, error) {
	generation, sid := 0, ""
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		for {
			sid = derive(generation)
			retired, err := RetiredIn(ctx, conn, sid)
			if err != nil {
				return err
			}
			if retired {
				if pinned != nil {
					hold, err := pinned(ctx, conn, sid)
					if err != nil || hold {
						return err
					}
				}
			} else if spent == nil {
				return nil
			} else if used, err := spent(ctx, conn, sid); err != nil || !used {
				return err
			}
			generation++
		}
	})
	return generation, sid, err
}

// Makes permanent every fence a walk passed before the session id, false when a passed marker was reclaimed
func KeepPassedFences(ctx context.Context, conn SQLiteConn, derive func(int) string, sessionID string) (bool, error) {
	for generation := 0; ; generation++ {
		sid := derive(generation)
		if sid == sessionID {
			return true, nil
		}
		retired, err := RetiredIn(ctx, conn, sid)
		if err != nil || !retired {
			return false, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=json_remove(value, '$.expires') WHERE key=? AND json_valid(value)", RetiredPrefix+sid); err != nil {
			return false, err
		}
	}
}

// Profile slot of an agent namespaced routing key, main for the launch profile and any other key shape
func RouteNamespace(sessionKey any) string {
	key := ""
	if sessionKey != nil {
		key = pyStr(sessionKey)
	}
	parts := strings.Split(key, ":")
	if len(parts) > 2 && parts[0] == "agent" && parts[1] != "" {
		return parts[1]
	}
	return "main"
}

// State meta key of the routing fence one profile namespace holds for a retired session
func RouteFenceKey(namespace, sessionID string) string {
	return RouteFencePrefix + namespace + ":" + sessionID
}

// True when a routing save must skip the entry, bound by name through routeRetiredParams
const routeRetiredSQL = `(
    EXISTS (SELECT 1 FROM state_meta m
             WHERE m.key = :retired_prefix || json_extract(:entry, '$.session_id')
               AND (:ns = 'main' OR NOT CASE WHEN json_valid(m.value)
                                             THEN json_extract(m.value, '$.own') IS NOT NULL ELSE 0 END))
    OR EXISTS (SELECT 1 FROM state_meta f
                WHERE f.key = :fence_prefix || :ns || ':' || json_extract(:entry, '$.session_id')
                  AND NOT CASE WHEN json_valid(f.value)
                               THEN coalesce(json_extract(f.value, '$.expires')
                                             AND json_extract(f.value, '$.retired_at') < :now - :horizon, 0)
                               ELSE 0 END
                  AND (:ns != 'main' OR NOT EXISTS (
                          SELECT 1 FROM sessions s WHERE s.id = json_extract(:entry, '$.session_id')))))`

// Named parameters of the retired route predicate for one entry saved at the given time
func routeRetiredParams(sessionKey, entryJSON string, at float64) []any {
	return []any{sql.Named("entry", entryJSON), sql.Named("ns", RouteNamespace(sessionKey)), sql.Named("now", at),
		sql.Named("horizon", retryHorizonS), sql.Named("retired_prefix", RetiredPrefix), sql.Named("fence_prefix", RouteFencePrefix)}
}

// True when a routing save or the legacy sessions json import must skip this entry
func (d *SessionDB) RouteRetired(ctx context.Context, sessionKey, entryJSON string) (bool, error) {
	var retired bool
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		return conn.QueryRowContext(ctx, "SELECT "+routeRetiredSQL, routeRetiredParams(sessionKey, entryJSON, now())...).Scan(&retired)
	})
	return retired, err
}

// Ids among the given sessions whose own retired marker is a sweep marker that ages out
func (d *SessionDB) ExpiringRetirements(ctx context.Context, sessionIDs []string) (map[string]bool, error) {
	ids := make([]any, len(sessionIDs))
	for i, sid := range sessionIDs {
		ids[i] = sid
	}
	list, err := pyjson.Dumps(ids)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	err = d.readCtx(ctx, false, func(conn SQLiteConn) error {
		rows, err := connRows(ctx, conn, "SELECT key FROM state_meta WHERE key IN (SELECT ? || value FROM json_each(?)) "+
			"AND json_valid(value) AND json_extract(value, '$.expires')", RetiredPrefix, list)
		for _, row := range rows {
			out[strings.TrimPrefix(pyStr(row["key"]), RetiredPrefix)] = true
		}
		return err
	})
	return out, err
}

// Fences the retired sessions of one profile namespace in the shared routing index and drops the routes naming them
func fenceSharedRoutes(ctx context.Context, conn SQLiteConn, namespace string, sessionIDs, expiring []string) error {
	if err := reclaimExpired(ctx, conn, RouteFencePrefix, now()-retryHorizonS); err != nil {
		return err
	}
	owned := map[string]map[string]bool{}
	for _, sid := range sessionIDs {
		names := map[string]bool{namespace: true}
		row, err := connRow(ctx, conn, "SELECT 1 AS one FROM sessions WHERE id=?", sid)
		if err != nil {
			return err
		}
		if row == nil {
			names["main"] = true
		}
		for name := range names {
			if err := markRetired(ctx, conn, RouteFenceKey(name, sid), slices.Contains(expiring, sid), false); err != nil {
				return err
			}
		}
		owned[sid] = names
	}
	rows, err := connRows(ctx, conn, "SELECT scope,session_key,entry_json FROM gateway_routing")
	if err != nil {
		return err
	}
	for _, row := range rows {
		target := entrySessionID(row["entry_json"])
		if _, err := hashableIn(target, nil); err != nil {
			return err
		}
		sid, ok := target.(string)
		if !ok || !owned[sid][RouteNamespace(row["session_key"])] {
			continue
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM gateway_routing WHERE scope=? AND session_key=?", row["scope"], row["session_key"]); err != nil {
			return err
		}
	}
	return nil
}

// True when the principal's request already committed a mutation receipt against the session
func (d *SessionDB) HasMutationReceipt(ctx context.Context, principalID, sessionID, requestID string) (bool, error) {
	for _, v := range []string{principalID, sessionID, requestID} {
		if err := runtimeText(v); err != nil {
			return false, err
		}
	}
	key, _, err := MutationReceiptKey(principalID, sessionID, requestID, "model", pyjson.NewObject(), 0, nil)
	if err != nil {
		return false, err
	}
	row, err := d.readOne(ctx, "SELECT 1 AS one FROM state_meta WHERE key=?", key)
	return row != nil, err
}
