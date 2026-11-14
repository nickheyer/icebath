package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"sync"
)

// Meta key prefix of a local creation receipt
const localPolicyPrefix = "gateway.local_policy.v1:"

// Creation receipt fields lineage reads, the owner id and its segment list
type localReceipt struct {
	SessionID json.RawMessage `json:"session_id"`
	Lineage   json.RawMessage `json:"lineage"`
}

// Segment list of a receipt, false unless it is a list of strings
func (r localReceipt) segments() ([]string, bool) {
	var raw []json.RawMessage
	if json.Unmarshal(r.Lineage, &raw) != nil || raw == nil {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		var s string
		if json.Unmarshal(item, &s) != nil {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// Receipt stored for owner on c, false when absent or undecodable
func readLocalReceipt(ctx context.Context, c Conn, owner string) (localReceipt, bool, error) {
	var value sql.NullString
	err := c.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key=?", localPolicyPrefix+owner).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return localReceipt{}, false, nil
	}
	if err != nil {
		return localReceipt{}, false, err
	}
	var r localReceipt
	if json.Unmarshal([]byte(value.String), &r) != nil {
		return localReceipt{}, false, nil
	}
	return r, true, nil
}

// Creation id whose receipt lineage holds the physical segment, the segment itself otherwise
func LocalLineageOwnerIn(ctx context.Context, c Conn, sessionID string) (string, error) {
	var owner sql.NullString
	err := c.QueryRowContext(ctx, "SELECT chat_id FROM sessions WHERE id=?", sessionID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionID, nil
	}
	if err != nil {
		return "", err
	}
	if !owner.Valid || owner.String == "" || owner.String == sessionID {
		return sessionID, nil
	}
	r, ok, err := readLocalReceipt(ctx, c, owner.String)
	if err != nil || !ok {
		return sessionID, err
	}
	var raw []json.RawMessage
	if json.Unmarshal(r.Lineage, &raw) != nil || raw == nil {
		return sessionID, nil
	}
	for _, item := range raw {
		var s string
		if json.Unmarshal(item, &s) == nil && s == sessionID {
			return owner.String, nil
		}
	}
	return sessionID, nil
}

// Every row of the local conversation the session belongs to, its creation id first
func LocalConversationIDsIn(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	owner, err := LocalLineageOwnerIn(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	r, ok, err := readLocalReceipt(ctx, c, owner)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if !ok || json.Unmarshal(r.Lineage, &raw) != nil || raw == nil {
		return []string{sessionID}, nil
	}
	out := []string{owner}
	for _, item := range raw {
		var s string
		if json.Unmarshal(item, &s) == nil && !containsString(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// The session alone outside a local lineage, else the whole conversation with the session first
func ownedLineageIDs(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	ids, err := LocalConversationIDsIn(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	out := []string{sessionID}
	for _, id := range ids {
		if !containsString(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// Head of one local reset lineage a listing shows as the conversation's row
type localHead struct {
	owner   string
	lineage []string
}

// Roots of earlier reset segments a listing drops, and the latest segment of each lineage mapped to its owner
func localLineageIndex(ctx context.Context, c Conn) (map[string]bool, map[string]localHead, error) {
	values, err := queryStrings(ctx, c, "SELECT value FROM state_meta WHERE key GLOB ? AND json_valid(value) "+
		"AND json_array_length(value, '$.lineage') > 1", localPolicyPrefix+"*")
	if err != nil {
		return nil, nil, err
	}
	superseded, heads := map[string]bool{}, map[string]localHead{}
	for _, v := range values {
		var r localReceipt
		if json.Unmarshal([]byte(v), &r) != nil {
			continue
		}
		var owner string
		if json.Unmarshal(r.SessionID, &owner) != nil {
			continue
		}
		lineage, ok := r.segments()
		if !ok {
			continue
		}
		ended := map[string]string{}
		rows, err := c.QueryContext(ctx, "SELECT id,end_reason FROM sessions WHERE id IN ("+bindMarks(len(lineage))+")", stringArgs(lineage)...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id string
			var reason sql.NullString
			if err := rows.Scan(&id, &reason); err != nil {
				rows.Close()
				return nil, nil, err
			}
			ended[id] = reason.String
		}
		if err := rowsDone(rows); err != nil {
			return nil, nil, err
		}
		var starts []string
		for i, sid := range lineage {
			if i == 0 || ended[lineage[i-1]] == "session_reset" {
				starts = append(starts, sid)
			}
		}
		for _, sid := range starts[:len(starts)-1] {
			superseded[sid] = true
		}
		heads[starts[len(starts)-1]] = localHead{owner: owner, lineage: lineage}
	}
	return superseded, heads, nil
}

// Drops earlier reset segments from a listing or count in SQL before paging, returning the heads to annotate
func (s *Store) excludeSupersededSegments(ctx context.Context, where *[]string, args *[]any) (map[string]localHead, error) {
	var superseded map[string]bool
	var heads map[string]localHead
	err := s.Read(ctx, func(c Conn) error {
		var err error
		superseded, heads, err = s.cachedLineageIndex(ctx, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(superseded) > 0 {
		ids := make([]string, 0, len(superseded))
		for id := range superseded {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		enc, err := json.Marshal(ids)
		if err != nil {
			return nil, err
		}
		*where = append(*where, "s.id NOT IN (SELECT value FROM json_each(?))")
		*args = append(*args, string(enc))
	}
	return heads, nil
}

// Names every segment of a local conversation on its representative row
func annotateLocalLineages(rows []SessionListing, heads map[string]localHead) {
	for i := range rows {
		key := rows[i].LineageRootID
		if key == "" {
			key = rows[i].ID
		}
		head, ok := heads[key]
		if !ok {
			continue
		}
		var merged []string
		for _, id := range append(append([]string{}, head.lineage...), rows[i].LineageIDs...) {
			if !containsString(merged, id) {
				merged = append(merged, id)
			}
		}
		rows[i].LineageIDs = merged
	}
}

// Connections whose lineage index a store remembers at most
const lineageCacheSize = 8

// Lineage index one connection computed, with the counters that tell when it went stale
type lineageSnapshot struct {
	changes, version int64
	superseded       map[string]bool
	heads            map[string]localHead
}

// Lineage indexes of the most recently used connections of one store, least recent first
type lineageCache struct {
	mu    sync.Mutex
	saved map[*conn]lineageSnapshot
	order []*conn
}

// Snapshot c stored at exactly these counters, moved to most recent, false when it has none
func (l *lineageCache) get(c *conn, changes, version int64) (lineageSnapshot, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snap, ok := l.saved[c]
	if !ok || snap.changes != changes || snap.version != version {
		return lineageSnapshot{}, false
	}
	l.touch(c)
	return snap, true
}

// Moves c to the most recent end of the order (caller holds mu)
func (l *lineageCache) touch(c *conn) {
	l.order = append(slices.DeleteFunc(l.order, func(o *conn) bool { return o == c }), c)
}

// Stores the snapshot of c as most recent, forgetting the least recent connection past the bound
func (l *lineageCache) put(c *conn, snap lineageSnapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.saved == nil {
		l.saved = map[*conn]lineageSnapshot{}
	}
	l.saved[c] = snap
	l.touch(c)
	if len(l.order) > lineageCacheSize {
		delete(l.saved, l.order[0])
		l.order = l.order[1:]
	}
}

// Forgets what c stored
func (l *lineageCache) drop(c *conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.saved, c)
	l.order = slices.DeleteFunc(l.order, func(o *conn) bool { return o == c })
}

// Writes of this connection and commits of every other one, either moving when the lineage index may have
func lineageVersion(ctx context.Context, c Conn) (int64, int64, error) {
	var changes, version int64
	if err := c.QueryRowContext(ctx, "SELECT total_changes()").Scan(&changes); err != nil {
		return 0, 0, err
	}
	err := c.QueryRowContext(ctx, "PRAGMA main.data_version").Scan(&version)
	return changes, version, err
}

// Lineage index of c, reused while neither counter moved and never kept from inside a transaction, read only for callers
func (s *Store) cachedLineageIndex(ctx context.Context, c Conn) (map[string]bool, map[string]localHead, error) {
	r, lent := c.(*runner)
	if !lent {
		return localLineageIndex(ctx, c)
	}
	cache := &s.tables.state.lineage
	open, err := r.c.inTransaction()
	if err != nil {
		return nil, nil, err
	}
	if open {
		cache.drop(r.c)
		return localLineageIndex(ctx, c)
	}
	changes, version, err := lineageVersion(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	if snap, ok := cache.get(r.c, changes, version); ok {
		return snap.superseded, snap.heads, nil
	}
	superseded, heads, err := localLineageIndex(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	after, afterVersion, err := lineageVersion(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	if open, err = r.c.inTransaction(); err != nil {
		return nil, nil, err
	}
	if !open && after == changes && afterVersion == version {
		cache.put(r.c, lineageSnapshot{changes: changes, version: version, superseded: superseded, heads: heads})
	}
	return superseded, heads, nil
}
