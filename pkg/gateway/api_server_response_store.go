package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	// Response bodies the store keeps before evicting the least recently used
	APIMaxStoredResponses = 100
	// Settled Idempotency-Key identities retained per response body slot
	APIIdentityRetentionFactor = 10
	// Stored terminal envelope of a body row, a corrupt body reading as absent
	apiResponseRecord = "json_extract(CASE WHEN json_valid(data) THEN data END, '$.response"
)

// ResponseStore keeps Responses API state for previous_response_id chaining and Idempotency-Key identity
type ResponseStore struct {
	mu            sync.Mutex
	maxSize       int
	maxIdentities int
	dbPath        *string
	conn          state.SQLiteConn
}

// Store at the path, the home response_store.db by default, process memory when the file cannot open
func NewResponseStore(ctx context.Context, maxSize int, dbPath string, maxIdentities int) (*ResponseStore, error) {
	if dbPath == "" {
		dbPath = filepath.Join(home.Dir(ctx), "response_store.db")
	}
	s := &ResponseStore{maxSize: maxSize, maxIdentities: maxIdentities}
	if dbPath != ":memory:" {
		path := dbPath
		s.dbPath = &path
	}
	conn, err := state.ConnectSQLite(dbPath, state.SQLiteConfig{Timeout: 5})
	if err == nil {
		if _, err = conn.ExecContext(ctx, "SELECT 1"); err != nil {
			conn.Close()
		}
	}
	if err != nil {
		if conn, err = state.ConnectSQLite(":memory:", state.SQLiteConfig{Timeout: 5}); err != nil {
			return nil, err
		}
		s.dbPath = nil
	}
	s.conn = conn
	if err := s.init(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	s.tightenFilePermissions()
	return s, nil
}

// Creates the tables and indexes, settles legacy and terminal identities and applies the bounds
func (s *ResponseStore) init(ctx context.Context) error {
	if _, err := state.ApplyWALWithFallback(ctx, s.conn, "response_store.db", false); err != nil {
		return err
	}
	for _, ddl := range []string{
		"CREATE TABLE IF NOT EXISTS responses (response_id TEXT PRIMARY KEY, data TEXT NOT NULL, accessed_at REAL NOT NULL)",
		"CREATE TABLE IF NOT EXISTS conversations (name TEXT PRIMARY KEY, response_id TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS response_keys (request_key TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, created_at INTEGER NOT NULL)",
		"CREATE TABLE IF NOT EXISTS response_admissions (request_key TEXT PRIMARY KEY, admission_id TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS response_output_order (request_key TEXT NOT NULL, item_id TEXT NOT NULL, output_index INTEGER NOT NULL, " +
			"PRIMARY KEY(request_key,item_id), UNIQUE(request_key,output_index))",
	} {
		if _, err := s.conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	for _, column := range [][2]string{{"model_json", "model_json TEXT"}, {"settled_at", "settled_at REAL"}, {"response_id", "response_id TEXT"}} {
		if _, err := state.AddColumnIfMissing(ctx, s.conn, "response_keys", column[0], column[1]); err != nil {
			return err
		}
	}
	for _, ddl := range []string{"CREATE INDEX IF NOT EXISTS response_keys_settled ON response_keys(settled_at)",
		"CREATE INDEX IF NOT EXISTS response_keys_response ON response_keys(response_id)"} {
		if _, err := s.conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return s.txn(ctx, func() error {
		if err := s.markSettled(ctx, "1"); err != nil {
			return err
		}
		if err := s.settleTerminalAdmissions(ctx, nil); err != nil {
			return err
		}
		return s.evict(ctx)
	})
}

// Runs the body as one transaction, committed on success and rolled back on error
func (s *ResponseStore) txn(ctx context.Context, body func() error) error {
	if _, err := s.conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	if err := body(); err != nil {
		if _, rollbackErr := s.conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	_, err := s.conn.ExecContext(ctx, "COMMIT")
	return err
}

// Context key marking a call chain that already holds the store lock
type responseStoreLockKey struct{ store *ResponseStore }

// Takes the store lock unless the context already holds it, the reentrant lock of the Python store
func (s *ResponseStore) lock(ctx context.Context) func() {
	if ctx.Value(responseStoreLockKey{s}) != nil {
		return func() {}
	}
	s.mu.Lock()
	return s.mu.Unlock
}

// Runs a sequence of store calls as one unit no other caller interleaves, the calls taking the given context
func (s *ResponseStore) Atomically(ctx context.Context, fn func(ctx context.Context) (any, error)) (any, error) {
	defer s.lock(ctx)()
	return fn(context.WithValue(ctx, responseStoreLockKey{s}, true))
}

// Owner only permissions on the database and its SQLite sidecars
func (s *ResponseStore) tightenFilePermissions() {
	if s.dbPath == nil {
		return
	}
	for _, candidate := range []string{*s.dbPath, *s.dbPath + "-wal", *s.dbPath + "-shm"} {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		if err := os.Chmod(candidate, 0o600); err != nil {
			apiServerLog().Debug(fmt.Sprintf("Failed to restrict response store permissions for %s", candidate), "error", err)
		}
	}
}

// Stored response by id with its access time refreshed, nil when absent or corrupt
func (s *ResponseStore) Get(ctx context.Context, responseID string) (any, error) {
	defer s.lock(ctx)()
	var data string
	err := s.conn.QueryRowContext(ctx, "SELECT data FROM responses WHERE response_id = ?", responseID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.conn.ExecContext(ctx, "UPDATE responses SET accessed_at = ? WHERE response_id = ?", apiNow(), responseID); err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(data)
	if err != nil {
		var decodeErr *pyjson.DecodeError
		if !errors.As(err, &decodeErr) {
			return nil, err
		}
		apiServerLog().Warn(fmt.Sprintf("Corrupted JSON in response store for id=%s, evicting entry", responseID))
		_, err := s.conn.ExecContext(ctx, "DELETE FROM responses WHERE response_id = ?", responseID)
		return nil, err
	}
	return decoded, nil
}

// Stores a response, an idem record never overwritten, evicting past capacity
func (s *ResponseStore) Put(ctx context.Context, responseID string, data any) error {
	defer s.lock(ctx)()
	encoded, err := pyjson.Dumps(data, pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return err
	}
	insert := "INSERT OR REPLACE"
	idem := strings.HasPrefix(responseID, "idem:")
	if idem {
		insert = "INSERT OR IGNORE"
	}
	return s.txn(ctx, func() error {
		if _, err := s.conn.ExecContext(ctx, insert+" INTO responses (response_id, data, accessed_at) VALUES (?, ?, ?)",
			responseID, encoded, apiNow()); err != nil {
			return err
		}
		if idem {
			if err := s.markSettled(ctx, "request_key = ?", responseID); err != nil {
				return err
			}
		}
		return s.evict(ctx)
	})
}

// Settles identities whose terminal replay record is stored, only then may they age out
func (s *ResponseStore) markSettled(ctx context.Context, selector string, params ...any) error {
	args := append([]any{apiNow()}, params...)
	_, err := s.conn.ExecContext(ctx, "UPDATE response_keys SET settled_at=?, response_id=(SELECT "+apiResponseRecord+".id') FROM responses "+
		"WHERE responses.response_id=request_key) WHERE settled_at IS NULL AND "+
		selector+" AND EXISTS (SELECT 1 FROM responses WHERE responses.response_id=request_key "+
		"AND "+apiResponseRecord+"') IS NOT NULL)", args...)
	return err
}

// Settles identities whose canonical admission already finished in this home's state.db without a replay record
func (s *ResponseStore) settleTerminalAdmissions(ctx context.Context, at *float64) error {
	if s.dbPath == nil {
		return nil
	}
	rows, err := s.conn.QueryContext(ctx, "SELECT a.admission_id, k.request_key FROM response_keys k JOIN response_admissions a "+
		"ON a.request_key=k.request_key WHERE k.settled_at IS NULL")
	if err != nil {
		return err
	}
	pending := map[string]string{}
	var ids []string
	for rows.Next() {
		var admissionID, requestKey string
		if err := rows.Scan(&admissionID, &requestKey); err != nil {
			rows.Close()
			return err
		}
		if _, seen := pending[admissionID]; !seen {
			ids = append(ids, admissionID)
		}
		pending[admissionID] = requestKey
	}
	rows.Close()
	stateDB := filepath.Join(filepath.Dir(*s.dbPath), "state.db")
	info, statErr := os.Stat(stateDB)
	if len(pending) == 0 || statErr != nil || !info.Mode().IsRegular() {
		return nil
	}
	uri, err := state.ReadOnlyDBURI(stateDB)
	if err != nil {
		return err
	}
	terminal, err := apiTerminalAdmissions(ctx, uri, ids)
	if err != nil {
		apiServerLog().Debug(fmt.Sprintf("Responses identity settlement skipped: %s unreadable", stateDB), "error", err)
		return nil
	}
	for admissionID := range terminal {
		key := pending[admissionID]
		var settled any
		if at != nil {
			settled = *at
		}
		if _, err := s.conn.ExecContext(ctx, "UPDATE response_keys SET settled_at=COALESCE(?, created_at), response_id=COALESCE(response_id, ?) "+
			"WHERE request_key=? AND settled_at IS NULL", settled, APIDurableResponseID(key), key); err != nil {
			return err
		}
	}
	return nil
}

// Admission ids proven terminal by a read only look at a canonical state.db, live rows or retired tombstones
func apiTerminalAdmissions(ctx context.Context, uri string, ids []string) (map[string]bool, error) {
	canon, err := state.ConnectSQLite(uri, state.SQLiteConfig{Timeout: 5, URI: true})
	if err != nil {
		return nil, err
	}
	defer canon.Close()
	terminal := map[string]bool{}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		prefixed := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
			prefixed[i] = state.TerminalAdmissionPrefix + id
		}
		if err := apiCollectColumn(ctx, canon, "SELECT admission_id FROM session_admissions WHERE status='terminal' "+
			"AND admission_id IN ("+marks+")", args, func(value string) { terminal[value] = true }); err != nil {
			return nil, err
		}
		if err := apiCollectColumn(ctx, canon, "SELECT key FROM state_meta WHERE key IN ("+marks+")", prefixed,
			func(value string) { terminal[strings.TrimPrefix(value, state.TerminalAdmissionPrefix)] = true }); err != nil {
			return nil, err
		}
	}
	return terminal, nil
}

// Feeds each first column text of a query to the collector
func apiCollectColumn(ctx context.Context, conn state.SQLiteConn, query string, args []any, collect func(string)) error {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return err
		}
		collect(value)
	}
	return rows.Err()
}

// Settled identities kept before the oldest are dropped
func (s *ResponseStore) identityBound() int {
	if s.maxIdentities != 0 {
		return s.maxIdentities
	}
	return APIIdentityRetentionFactor * s.maxSize
}

// Drops the selected settled identities from every compact table and their replay record, the count of keys removed
func (s *ResponseStore) forgetIdentities(ctx context.Context, selector string, params ...any) (int64, error) {
	keys := "SELECT request_key FROM response_keys WHERE settled_at IS NOT NULL AND " + selector
	for _, target := range [][2]string{{"response_output_order", "request_key"}, {"response_admissions", "request_key"}, {"responses", "response_id"}} {
		if _, err := s.conn.ExecContext(ctx, "DELETE FROM "+target[0]+" WHERE "+target[1]+" IN ("+keys+")", params...); err != nil {
			return 0, err
		}
	}
	result, err := s.conn.ExecContext(ctx, "DELETE FROM response_keys WHERE request_key IN ("+keys+")", params...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Evicts least recently used bodies past capacity and the oldest settled identities past their bound
func (s *ResponseStore) evict(ctx context.Context) error {
	var count int
	if err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM responses").Scan(&count); err != nil {
		return err
	}
	if count > s.maxSize {
		rows, err := s.conn.QueryContext(ctx, "SELECT response_id FROM responses ORDER BY accessed_at ASC LIMIT ?", count-s.maxSize)
		if err != nil {
			return err
		}
		var evictIDs []any
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			evictIDs = append(evictIDs, id)
		}
		rows.Close()
		if len(evictIDs) > 0 {
			marks := strings.TrimSuffix(strings.Repeat("?,", len(evictIDs)), ",")
			if _, err := s.conn.ExecContext(ctx, "DELETE FROM conversations WHERE response_id IN ("+marks+")", evictIDs...); err != nil {
				return err
			}
			if _, err := s.conn.ExecContext(ctx, "DELETE FROM responses WHERE response_id IN ("+marks+")", evictIDs...); err != nil {
				return err
			}
		}
	}
	now := apiNow()
	if err := s.settleTerminalAdmissions(ctx, &now); err != nil {
		return err
	}
	var settled int
	if err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM response_keys WHERE settled_at IS NOT NULL").Scan(&settled); err != nil {
		return err
	}
	if excess := settled - s.identityBound(); excess > 0 {
		if _, err := s.forgetIdentities(ctx, "1 ORDER BY settled_at, rowid LIMIT ?", excess); err != nil {
			return err
		}
		for _, table := range []string{"response_output_order", "response_admissions"} {
			if _, err := s.conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE request_key NOT IN (SELECT request_key FROM response_keys)"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reserves one immutable request digest, false when the key is bound to another fingerprint
func (s *ResponseStore) BindRequestKey(ctx context.Context, key, fingerprint string, model any) (bool, error) {
	defer s.lock(ctx)()
	modelJSON, err := pyjson.Dumps(model)
	if err != nil {
		return false, err
	}
	var stored string
	err = s.txn(ctx, func() error {
		if _, err := s.conn.ExecContext(ctx, "INSERT OR IGNORE INTO response_keys "+
			"(request_key,fingerprint,created_at,model_json) VALUES (?,?,?,?)", key, fingerprint, int64(apiNow()), modelJSON); err != nil {
			return err
		}
		return s.conn.QueryRowContext(ctx, "SELECT fingerprint FROM response_keys WHERE request_key=?", key).Scan(&stored)
	})
	return stored == fingerprint, err
}

// Accepted wire model label of a request key, upgrading an older binding to the fallback
func (s *ResponseStore) RequestModel(ctx context.Context, key string, fallback any) (any, error) {
	defer s.lock(ctx)()
	encoded, err := pyjson.Dumps(fallback)
	if err != nil {
		return nil, err
	}
	var modelJSON sql.NullString
	found := true
	err = s.txn(ctx, func() error {
		if _, err := s.conn.ExecContext(ctx, "UPDATE response_keys SET model_json=? WHERE request_key=? AND model_json IS NULL", encoded, key); err != nil {
			return err
		}
		err := s.conn.QueryRowContext(ctx, "SELECT model_json FROM response_keys WHERE request_key=?", key).Scan(&modelJSON)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	if err != nil || !found {
		return fallback, err
	}
	if !modelJSON.Valid {
		return nil, &PyError{Kind: "TypeError", Msg: "the JSON object must be str, bytes or bytearray, not NoneType"}
	}
	return pyjson.Loads(modelJSON.String)
}

// Output index allocated to an item of a request, shared by retries and parallel observers
func (s *ResponseStore) OutputIndex(ctx context.Context, key, itemID string) (int64, error) {
	defer s.lock(ctx)()
	return s.outputIndexLocked(ctx, key, itemID)
}

// Output index allocation under the held store lock
func (s *ResponseStore) outputIndexLocked(ctx context.Context, key, itemID string) (int64, error) {
	var index int64
	err := s.txn(ctx, func() error {
		if _, err := s.conn.ExecContext(ctx, "INSERT OR IGNORE INTO response_output_order "+
			"(request_key,item_id,output_index) SELECT ?,?,COALESCE(MAX(output_index)+1,0) "+
			"FROM response_output_order WHERE request_key=?", key, itemID, key); err != nil {
			return err
		}
		return s.conn.QueryRowContext(ctx, "SELECT output_index FROM response_output_order WHERE request_key=? AND item_id=?",
			key, itemID).Scan(&index)
	})
	return index, err
}

// Items sorted by their allocated output index
func (s *ResponseStore) OrderOutput(ctx context.Context, key string, items []*pyjson.Object) ([]*pyjson.Object, error) {
	defer s.lock(ctx)()
	indexes := make([]int64, len(items))
	for i, item := range items {
		id, found := item.Get("id")
		if !found {
			return nil, &pyKeyError{"id"}
		}
		index, err := s.outputIndexLocked(ctx, key, pyrepr.Str(id))
		if err != nil {
			return nil, err
		}
		indexes[i] = index
	}
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return indexes[order[a]] < indexes[order[b]] })
	out := make([]*pyjson.Object, len(items))
	for i, at := range order {
		out[i] = items[at]
	}
	return out, nil
}

// Creation second of a request key, nil when unbound
func (s *ResponseStore) RequestCreatedAt(ctx context.Context, key string) (*int64, error) {
	defer s.lock(ctx)()
	var created int64
	err := s.conn.QueryRowContext(ctx, "SELECT created_at FROM response_keys WHERE request_key=?", key).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// Whether a request key is unbound or bound to the fingerprint
func (s *ResponseStore) RequestKeyMatches(ctx context.Context, key, fingerprint string) (bool, error) {
	defer s.lock(ctx)()
	var stored string
	err := s.conn.QueryRowContext(ctx, "SELECT fingerprint FROM response_keys WHERE request_key=?", key).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return err == nil && stored == fingerprint, err
}

// Canonical admission bound to a request key, empty when none
func (s *ResponseStore) RequestAdmission(ctx context.Context, key string) (string, error) {
	defer s.lock(ctx)()
	return s.requestAdmissionLocked(ctx, key)
}

// Bound admission lookup under the held store lock
func (s *ResponseStore) requestAdmissionLocked(ctx context.Context, key string) (string, error) {
	var admissionID string
	err := s.conn.QueryRowContext(ctx, "SELECT admission_id FROM response_admissions WHERE request_key=?", key).Scan(&admissionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return admissionID, err
}

// Binds a request key to its canonical admission, refused as a conflict when bound to another
func (s *ResponseStore) BindRequestAdmission(ctx context.Context, key, admissionID string) error {
	defer s.lock(ctx)()
	return s.txn(ctx, func() error {
		if _, err := s.conn.ExecContext(ctx, "INSERT OR IGNORE INTO response_admissions VALUES (?,?)", key, admissionID); err != nil {
			return err
		}
		bound, err := s.requestAdmissionLocked(ctx, key)
		if err != nil {
			return err
		}
		if bound != admissionID {
			return runtimeRefusal("admission_conflict")
		}
		return nil
	})
}

// Drops a request key that never reached an admission
func (s *ResponseStore) ForgetUnadmittedRequest(ctx context.Context, key string) error {
	defer s.lock(ctx)()
	return s.txn(ctx, func() error {
		_, err := s.conn.ExecContext(ctx, "DELETE FROM response_keys WHERE request_key=? AND NOT EXISTS "+
			"(SELECT 1 FROM response_admissions WHERE request_key=?)", key, key)
		return err
	})
}

// Removes a response, its conversation mappings and its settled identity, true when anything was deleted
func (s *ResponseStore) Delete(ctx context.Context, responseID string) (bool, error) {
	defer s.lock(ctx)()
	var removed, forgotten int64
	err := s.txn(ctx, func() error {
		if _, err := s.conn.ExecContext(ctx, "DELETE FROM conversations WHERE response_id = ?", responseID); err != nil {
			return err
		}
		result, err := s.conn.ExecContext(ctx, "DELETE FROM responses WHERE response_id = ?", responseID)
		if err != nil {
			return err
		}
		if removed, err = result.RowsAffected(); err != nil {
			return err
		}
		now := apiNow()
		if err := s.settleTerminalAdmissions(ctx, &now); err != nil {
			return err
		}
		forgotten, err = s.forgetIdentities(ctx, "response_id = ?", responseID)
		return err
	})
	return removed > 0 || forgotten > 0, err
}

// Latest response id of a conversation name, empty when unmapped
func (s *ResponseStore) GetConversation(ctx context.Context, name string) (string, error) {
	defer s.lock(ctx)()
	var responseID string
	err := s.conn.QueryRowContext(ctx, "SELECT response_id FROM conversations WHERE name = ?", name).Scan(&responseID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return responseID, err
}

// Maps a conversation name to its latest response id
func (s *ResponseStore) SetConversation(ctx context.Context, name, responseID string) error {
	defer s.lock(ctx)()
	_, err := s.conn.ExecContext(ctx, "INSERT OR REPLACE INTO conversations (name, response_id) VALUES (?, ?)", name, responseID)
	return err
}

// Closes the connection, a close failure swallowed
func (s *ResponseStore) Close(ctx context.Context) {
	defer s.lock(ctx)()
	s.conn.Close()
}

// Stored response body count
func (s *ResponseStore) Len(ctx context.Context) (int, error) {
	defer s.lock(ctx)()
	var count int
	err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM responses").Scan(&count)
	return count, err
}
