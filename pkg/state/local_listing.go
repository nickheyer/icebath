package state

import (
	"context"
	"slices"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Owner and segment ids of one local reset lineage
type localLineageHead struct {
	owner   string
	lineage []string
}

// Superseded earlier reset segment roots and the latest segment heads of local conversations
type localLineageIndexValue struct {
	superseded map[string]bool
	heads      map[string]localLineageHead
}

// Total changes and data version of a connection, the cache validity stamp
type localLineageVersion struct {
	totalChanges, dataVersion int64
}

type localLineageCacheEntry struct {
	conn    SQLiteConn
	version localLineageVersion
	value   localLineageIndexValue
}

// Per database recently used lineage index answers keyed by connection, at most eight retained
type localLineageCache struct {
	mu      sync.Mutex
	entries []localLineageCacheEntry
}

// Superseded reset segment roots and latest segment heads from the local policy receipts
func localLineageIndex(ctx context.Context, conn SQLiteConn) (localLineageIndexValue, error) {
	out := localLineageIndexValue{superseded: map[string]bool{}, heads: map[string]localLineageHead{}}
	rows, err := queryObjects(ctx, conn, "SELECT value FROM state_meta WHERE key GLOB ? AND json_valid(value) "+
		"AND json_array_length(value, '$.lineage') > 1", LocalPolicyPrefix+"*")
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		decoded, err := pyjson.Loads(pyStr(rowValue(row, "value")))
		if err != nil {
			return out, err
		}
		receipt, _ := decoded.(*pyjson.Object)
		owner, ok := rowValue(receipt, "session_id").(string)
		rawLineage, _ := rowValue(receipt, "lineage").([]any)
		lineage := make([]string, 0, len(rawLineage))
		for _, sid := range rawLineage {
			if s, isStr := sid.(string); isStr {
				lineage = append(lineage, s)
			} else {
				ok = false
			}
		}
		if !ok {
			continue
		}
		endedRows, err := queryObjects(ctx, conn, "SELECT id,end_reason FROM sessions WHERE id IN ("+placeholders(len(lineage))+")", stringsToAny(lineage)...)
		if err != nil {
			return out, err
		}
		ended := map[string]any{}
		for _, r := range endedRows {
			ended[pyStr(rowValue(r, "id"))] = rowValue(r, "end_reason")
		}
		var starts []string
		for i, sid := range lineage {
			if i == 0 || ended[lineage[i-1]] == "session_reset" {
				starts = append(starts, sid)
			}
		}
		for _, sid := range starts[:len(starts)-1] {
			out.superseded[sid] = true
		}
		out.heads[starts[len(starts)-1]] = localLineageHead{owner: owner, lineage: lineage}
	}
	return out, nil
}

// Connection change stamp seeing both its own writes and other connections' commits
func lineageVersion(ctx context.Context, conn SQLiteConn) (localLineageVersion, error) {
	changes, err := totalChanges(ctx, conn)
	if err != nil {
		return localLineageVersion{}, err
	}
	var dataVersion int64
	err = conn.QueryRowContext(ctx, "PRAGMA main.data_version").Scan(&dataVersion)
	return localLineageVersion{totalChanges: changes, dataVersion: dataVersion}, err
}

// Deep copy so a caller mutating the answer never corrupts the cached entry
func copyLineageIndex(value localLineageIndexValue) localLineageIndexValue {
	out := localLineageIndexValue{superseded: make(map[string]bool, len(value.superseded)), heads: make(map[string]localLineageHead, len(value.heads))}
	for k := range value.superseded {
		out.superseded[k] = true
	}
	for k, h := range value.heads {
		out.heads[k] = localLineageHead{owner: h.owner, lineage: slices.Clone(h.lineage)}
	}
	return out
}

// Lineage index cached per connection while its version holds, never caching inside a transaction
func (d *SessionDB) cachedLocalLineageIndex(ctx context.Context, conn SQLiteConn) (localLineageIndexValue, error) {
	cache := &d.localLineage
	auto, err := sqliteAutocommit(conn)
	if err != nil {
		return localLineageIndexValue{}, err
	}
	if !auto {
		cache.mu.Lock()
		cache.drop(conn)
		cache.mu.Unlock()
		return localLineageIndex(ctx, conn)
	}
	version, err := lineageVersion(ctx, conn)
	if err != nil {
		return localLineageIndexValue{}, err
	}
	cache.mu.Lock()
	for i, e := range cache.entries {
		if e.conn == conn && e.version == version {
			cache.entries = append(append(cache.entries[:i:i], cache.entries[i+1:]...), e)
			cache.mu.Unlock()
			return copyLineageIndex(e.value), nil
		}
	}
	cache.mu.Unlock()
	value, err := localLineageIndex(ctx, conn)
	if err != nil {
		return value, err
	}
	auto, err = sqliteAutocommit(conn)
	if err != nil {
		return value, err
	}
	after, err := lineageVersion(ctx, conn)
	if err != nil {
		return value, err
	}
	if auto && after == version {
		cache.mu.Lock()
		cache.drop(conn)
		cache.entries = append(cache.entries, localLineageCacheEntry{conn: conn, version: version, value: value})
		if len(cache.entries) > 8 {
			cache.entries = cache.entries[1:]
		}
		cache.mu.Unlock()
	}
	return copyLineageIndex(value), nil
}

// Removes the entry of a connection, caller holds mu
func (c *localLineageCache) drop(conn SQLiteConn) {
	c.entries = slices.DeleteFunc(c.entries, func(e localLineageCacheEntry) bool { return e.conn == conn })
}

// Drops earlier reset segments from a listing query before paging and returns the lineage heads
func (d *SessionDB) excludeSupersededSegments(ctx context.Context, whereClauses *[]string, params *[]any) (map[string]localLineageHead, error) {
	var value localLineageIndexValue
	if err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		var err error
		value, err = d.cachedLocalLineageIndex(ctx, conn)
		return err
	}); err != nil {
		return nil, err
	}
	if len(value.superseded) > 0 {
		ids := make([]string, 0, len(value.superseded))
		for id := range value.superseded {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		encoded, err := pyjson.Dumps(stringsToAny(ids))
		if err != nil {
			return nil, err
		}
		*whereClauses = append(*whereClauses, "s.id NOT IN (SELECT value FROM json_each(?))")
		*params = append(*params, encoded)
	}
	return value.heads, nil
}

// Names every segment of a local conversation on its representative row as lineage ids
func annotateLocalLineages(sessions []*pyjson.Object, heads map[string]localLineageHead) []*pyjson.Object {
	for _, row := range sessions {
		key := rowValue(row, "_lineage_root_id")
		if !pyTruthy(key) {
			key = rowValue(row, "id")
		}
		ks, isStr := key.(string)
		found, ok := heads[ks]
		if !isStr || !ok {
			continue
		}
		seen := map[string]bool{}
		var ids []any
		add := func(v any) {
			if s, isS := v.(string); isS && !seen[s] {
				seen[s] = true
				ids = append(ids, s)
			}
		}
		for _, sid := range found.lineage {
			add(sid)
		}
		existing, _ := rowValue(row, "_lineage_ids").([]any)
		for _, sid := range existing {
			add(sid)
		}
		row.Set("_lineage_ids", ids)
	}
	return sessions
}
