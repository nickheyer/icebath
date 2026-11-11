package acp

import (
	"context"
	"os"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Rich session rows the gateway session store reader lists
type RichSessionLister interface {
	ListSessionsRich(ctx context.Context, o state.ListSessionsRichOptions) ([]*pyjson.Object, error)
}

// Gateway session store reader over a state database, assigned in cmd/icebath/wiring.go
var NewSurfaceLabelReader func(db *state.SessionDB) RichSessionLister

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func openReadOnly(ctx context.Context, path string) (*state.SessionDB, error) {
	return state.NewSessionDB(ctx, path, true)
}

// ACP session rows of a state database keyed by id, empty when the database is absent
func ReadCatalogRows(ctx context.Context, path string) (map[string]*pyjson.Object, []string, error) {
	if !pathExists(path) {
		return map[string]*pyjson.Object{}, nil, nil
	}
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	limit := 1000
	rows, err := NewSurfaceLabelReader(db).ListSessionsRich(ctx, state.ListSessionsRichOptions{Source: "acp", Limit: &limit})
	if err != nil {
		return nil, nil, err
	}
	out := map[string]*pyjson.Object{}
	var order []string
	for _, r := range rows {
		id := pyStr(dget(r, "id"))
		if _, seen := out[id]; !seen {
			order = append(order, id)
		}
		out[id] = r
	}
	return out, order, nil
}

// Physical id to its retained logical owner from each private local creation receipt's lineage
func localOwners(ctx context.Context, db *state.SessionDB) (map[string]string, error) {
	owners := map[string]string{}
	var raws []string
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT value FROM state_meta WHERE key GLOB ?", state.LocalPolicyPrefix+"*")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw any
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			switch x := raw.(type) {
			case string:
				raws = append(raws, x)
			case []byte:
				raws = append(raws, string(x))
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for _, raw := range raws {
		receipt, err := pyjson.Loads(raw)
		if err != nil {
			continue
		}
		obj, ok := receipt.(*pyjson.Object)
		if !ok {
			continue
		}
		owner, ok := dget(obj, "session_id").(string)
		if !ok {
			continue
		}
		lineage, _ := dget(obj, "lineage").([]any)
		for _, p := range lineage {
			if physical, ok := p.(string); ok {
				owners[physical] = owner
			}
		}
	}
	return owners, nil
}

// Local creation receipt owners of a state database, empty when it is absent
func ReadLocalOwners(ctx context.Context, path string) (map[string]string, error) {
	if !pathExists(path) {
		return map[string]string{}, nil
	}
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return localOwners(ctx, db)
}

// Logical session id of an id an editor holds, the compression root mapped through its local owner
func LogicalSessionID(ctx context.Context, path, sessionID string) (string, error) {
	if !pathExists(path) {
		return sessionID, nil
	}
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	lineage, err := db.GetCompressionLineage(ctx, sessionID)
	if err != nil {
		return "", err
	}
	root := sessionID
	if len(lineage) > 0 {
		root = lineage[0]
	}
	owners, err := localOwners(ctx, db)
	if err != nil {
		return "", err
	}
	if o, ok := owners[root]; ok {
		return o, nil
	}
	return root, nil
}

// Historical ACP sessions with messages, optionally under one cwd, one row per logical owner newest first
func CatalogSessions(ctx context.Context, path, cwd string) ([]*pyjson.Object, error) {
	normalized := ""
	if cwd != "" {
		normalized = normalizeCwdForCompare(cwd)
	}
	owners, err := ReadLocalOwners(ctx, path)
	if err != nil {
		return nil, err
	}
	rows, order, err := ReadCatalogRows(ctx, path)
	if err != nil {
		return nil, err
	}
	results := map[string]*pyjson.Object{}
	var keys []string
	for _, id := range order {
		row := rows[id]
		sid := id
		if root := dget(row, "_lineage_root_id"); truthy(root) {
			sid = pyStr(root)
		}
		if o, ok := owners[sid]; ok {
			sid = o
		}
		count := int(pyIntValue(pyrepr.Or(dget(row, "message_count"), int64(0))))
		sessionCwd := pyStr(pyrepr.Or(dget(row, "cwd"), dgetOr(parseModelConfig(dget(row, "model_config")), "cwd", ".")))
		if count <= 0 || (normalized != "" && normalizeCwdForCompare(sessionCwd) != normalized) {
			continue
		}
		info := sessionInfo(sid, sessionCwd, pyrepr.Or(dget(row, "model"), ""), count, dget(row, "title"),
			dget(row, "preview"), pyrepr.Or(dget(row, "last_active"), dget(row, "started_at")))
		held, ok := results[sid]
		if !ok {
			keys = append(keys, sid)
		}
		if !ok || updatedAtSortKey(dget(info, "updated_at")) > updatedAtSortKey(dget(held, "updated_at")) {
			results[sid] = info
		}
	}
	out := make([]*pyjson.Object, 0, len(keys))
	for _, k := range keys {
		out = append(out, results[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return updatedAtSortKey(dget(out[i], "updated_at")) > updatedAtSortKey(dget(out[j], "updated_at"))
	})
	return out, nil
}
