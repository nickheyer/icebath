package state

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Lazily created tables whose identity columns depend on optional migrations
var optionalTableNames = []string{"telegram_dm_topic_mode", "telegram_dm_topic_bindings", "delivery_obligations"}

// json.dumps with ensure_ascii False
var pyJSONUnicode = pyJSON{allowNaN: true, itemSep: ", ", keySep: ": "}

// Live column sets of the optional tables that exist
func optionalTableColumns(ctx context.Context, conn SQLiteConn) (map[string]map[string]bool, error) {
	rows, err := connRows(ctx, conn, "SELECT name FROM sqlite_master WHERE type='table' AND name IN (?, ?, ?)",
		optionalTableNames[0], optionalTableNames[1], optionalTableNames[2])
	if err != nil {
		return nil, err
	}
	existing := map[string]bool{}
	for _, r := range rows {
		name, _ := r["name"].(string)
		existing[name] = true
	}
	out := map[string]map[string]bool{}
	for _, table := range optionalTableNames {
		if !existing[table] {
			continue
		}
		cols, err := connRows(ctx, conn, fmt.Sprintf("PRAGMA table_info('%s')", table))
		if err != nil {
			return nil, err
		}
		out[table] = map[string]bool{}
		for _, c := range cols {
			name, _ := c["name"].(string)
			out[table][name] = true
		}
	}
	return out, nil
}

// Names of every table in the database
func existingTables(ctx context.Context, conn SQLiteConn) (map[string]bool, error) {
	rows, err := connRows(ctx, conn, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		name, _ := r["name"].(string)
		out[name] = true
	}
	return out, nil
}

// Rows a statement changed on the caller connection
func execCount(ctx context.Context, conn SQLiteConn, query string, args ...any) (int, error) {
	res, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// JSON text decoded as an ordered object, false for anything json.loads rejects or a non object
func loadsObject(raw any) (pyObject, bool) {
	var text string
	switch x := raw.(type) {
	case string:
		text = x
	case []byte:
		decoded, err := pyJSONDecodeBytes(x)
		if err != nil {
			return nil, false
		}
		text = decoded
	default:
		return nil, false
	}
	v, err := pyLoads(text)
	if err != nil {
		return nil, false
	}
	obj, ok := v.(pyObject)
	return obj, ok
}

// Code point suffix of s after its first n code points
func runeSuffix(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[i:]
		}
		n--
	}
	return ""
}

// Atomically rewrites exact profile identity in this state database, returning per store counts
func (d *SessionDB) RekeyProfileState(oldName, newName string) (map[string]int, error) {
	old, nw := pyStrip(oldName), pyStrip(newName)
	counts := map[string]int{}
	if old == "" || nw == "" || old == nw {
		return counts, nil
	}
	oldNS, newNS := "agent:"+old+":", "agent:"+nw+":"
	nsLen := utf8.RuneCountInString(oldNS)
	ctx := context.Background()
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		existing, err := existingTables(ctx, conn)
		if err != nil {
			return err
		}
		topic, err := optionalTableColumns(ctx, conn)
		if err != nil {
			return err
		}
		collision, err := connRow(ctx, conn, "SELECT old.scope AS scope, ? || substr(old.session_key, ?) AS target FROM gateway_routing AS old JOIN gateway_routing AS target "+
			"ON target.scope = old.scope AND target.session_key = ? || substr(old.session_key, ?) WHERE substr(old.session_key, 1, ?) = ? LIMIT 1",
			newNS, nsLen+1, newNS, nsLen+1, nsLen, oldNS)
		if err != nil {
			return err
		}
		if collision != nil {
			return fmt.Errorf("profile routing collision in scope %s: %s", pyrepr.Repr(collision["scope"]), pyrepr.Repr(collision["target"]))
		}
		for _, spec := range []struct {
			table   string
			columns []string
		}{{"telegram_dm_topic_mode", []string{"chat_id"}}, {"telegram_dm_topic_bindings", []string{"chat_id", "thread_id"}}} {
			if !topic[spec.table]["profile_name"] {
				continue
			}
			eq := make([]string, len(spec.columns))
			for i, c := range spec.columns {
				eq[i] = fmt.Sprintf("target.%s = old.%s", c, c)
			}
			hit, err := connRow(ctx, conn, fmt.Sprintf("SELECT 1 FROM %s AS old JOIN %s AS target ON target.profile_name = ? AND %s WHERE old.profile_name = ? LIMIT 1",
				spec.table, spec.table, strings.Join(eq, " AND ")), nw, old)
			if err != nil {
				return err
			}
			if hit != nil {
				return fmt.Errorf("profile identity collision in %s", spec.table)
			}
		}
		set := func(key, query string, args ...any) error {
			n, err := execCount(ctx, conn, query, args...)
			counts[key] = n
			return err
		}
		if err := set("sessions_profile_name", "UPDATE sessions SET profile_name = ? WHERE profile_name = ?", nw, old); err != nil {
			return err
		}
		if err := set("gateway_heartbeats_profile", "UPDATE gateway_heartbeats SET profile = ? WHERE profile = ?", nw, old); err != nil {
			return err
		}
		if err := set("sessions_session_key", "UPDATE sessions SET session_key = ? || substr(session_key, ?) WHERE substr(session_key, 1, ?) = ?",
			newNS, nsLen+1, nsLen, oldNS); err != nil {
			return err
		}
		origins, err := connRows(ctx, conn, "SELECT id, origin_json FROM sessions WHERE origin_json IS NOT NULL")
		if err != nil {
			return err
		}
		originCount := 0
		for _, r := range origins {
			payload, ok := loadsObject(r["origin_json"])
			if !ok {
				continue
			}
			if v, _ := payload.get("profile"); !pyrepr.Equal(v, old) {
				continue
			}
			text, err := pyDumps(pyObjectSet(payload, "profile", nw), pyJSONUnicode)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET origin_json = ? WHERE id = ?", text, r["id"]); err != nil {
				return err
			}
			originCount++
		}
		counts["sessions_origin_json"] = originCount
		if existing["delivery_obligations"] {
			if topic["delivery_obligations"]["adapter_profile"] {
				if err := set("delivery_obligations_adapter_profile", "UPDATE delivery_obligations SET adapter_profile = ? WHERE adapter_profile = ?", nw, old); err != nil {
					return err
				}
			}
			if err := set("delivery_obligations_session_key", "UPDATE delivery_obligations SET session_key = ? || substr(session_key, ?) WHERE substr(session_key, 1, ?) = ?",
				newNS, nsLen+1, nsLen, oldNS); err != nil {
				return err
			}
		}
		for _, table := range []string{"telegram_dm_topic_mode", "telegram_dm_topic_bindings"} {
			if topic[table]["profile_name"] {
				if err := set(table+"_profile_name", fmt.Sprintf("UPDATE %s SET profile_name = ? WHERE profile_name = ?", table), nw, old); err != nil {
					return err
				}
			}
		}
		if topic["telegram_dm_topic_bindings"]["session_key"] {
			if err := set("telegram_dm_topic_bindings_session_key", "UPDATE telegram_dm_topic_bindings SET session_key = ? || substr(session_key, ?) WHERE substr(session_key, 1, ?) = ?",
				newNS, nsLen+1, nsLen, oldNS); err != nil {
				return err
			}
		}
		routing, err := connRows(ctx, conn, "SELECT rowid AS rowid, session_key, entry_json FROM gateway_routing WHERE substr(session_key, 1, ?) = ?", nsLen, oldNS)
		if err != nil {
			return err
		}
		for _, r := range routing {
			key, _ := r["session_key"].(string)
			newJSON := r["entry_json"]
			if pyTruthy(newJSON) {
				if payload, ok := loadsObject(newJSON); ok {
					if sk, ok := mustGet(payload, "session_key").(string); ok && strings.HasPrefix(sk, oldNS) {
						payload = pyObjectSet(payload, "session_key", newNS+runeSuffix(sk, nsLen))
					}
					if origin, ok := mustGet(payload, "origin").(pyObject); ok {
						if v, _ := origin.get("profile"); pyrepr.Equal(v, old) {
							payload = pyObjectSet(payload, "origin", pyObjectSet(origin, "profile", nw))
						}
					}
					text, err := pyDumps(payload, pyJSONUnicode)
					if err != nil {
						return err
					}
					newJSON = text
				}
			}
			if _, err := conn.ExecContext(ctx, "UPDATE gateway_routing SET session_key = ?, entry_json = ? WHERE rowid = ?",
				newNS+runeSuffix(key, nsLen), newJSON, r["rowid"]); err != nil {
				return err
			}
		}
		counts["gateway_routing"] = len(routing)
		return nil
	}, nil)
	return counts, err
}

// Value of a key in an ordered object, nil when absent
func mustGet(obj pyObject, key string) any {
	v, _ := obj.get(key)
	return v
}

// Deletes exact profile identity from this state database, abandoning pending deliveries, returning per store counts
func (d *SessionDB) PurgeProfileState(profile string) (map[string]int, error) {
	name := pyStrip(profile)
	counts := map[string]int{}
	if name == "" {
		return counts, nil
	}
	ns := "agent:" + name + ":"
	nsLen := utf8.RuneCountInString(ns)
	ctx := context.Background()
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		existing, err := existingTables(ctx, conn)
		if err != nil {
			return err
		}
		topic, err := optionalTableColumns(ctx, conn)
		if err != nil {
			return err
		}
		set := func(key, query string, args ...any) error {
			n, err := execCount(ctx, conn, query, args...)
			counts[key] = n
			return err
		}
		if existing["gateway_routing"] {
			if err := set("gateway_routing", "DELETE FROM gateway_routing WHERE substr(session_key, 1, ?) = ?", nsLen, ns); err != nil {
				return err
			}
		}
		if existing["gateway_heartbeats"] {
			if err := set("gateway_heartbeats", "DELETE FROM gateway_heartbeats WHERE profile = ?", name); err != nil {
				return err
			}
		}
		if existing["delivery_obligations"] {
			byProfile, args := "", []any{now(), nsLen, ns}
			if topic["delivery_obligations"]["adapter_profile"] {
				byProfile, args = "adapter_profile = ? OR ", []any{now(), name, nsLen, ns}
			}
			if err := set("delivery_obligations", "UPDATE delivery_obligations SET state='abandoned', updated_at=? WHERE ("+byProfile+
				"substr(session_key, 1, ?) = ?) AND state NOT IN ('delivered', 'abandoned')", args...); err != nil {
				return err
			}
		}
		if topic["telegram_dm_topic_mode"]["profile_name"] {
			if err := set("telegram_dm_topic_mode", "DELETE FROM telegram_dm_topic_mode WHERE profile_name = ?", name); err != nil {
				return err
			}
		}
		if binding := topic["telegram_dm_topic_bindings"]; binding["session_key"] {
			byProfile, args := "", []any{nsLen, ns}
			if binding["profile_name"] {
				byProfile, args = "profile_name = ? OR ", []any{name, nsLen, ns}
			}
			if err := set("telegram_dm_topic_bindings", "DELETE FROM telegram_dm_topic_bindings WHERE "+byProfile+"substr(session_key, 1, ?) = ?", args...); err != nil {
				return err
			}
		}
		return nil
	}, nil)
	return counts, err
}
