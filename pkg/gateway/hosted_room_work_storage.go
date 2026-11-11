package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Creates or migrates the evidence schema and guards, joining an open transaction through a savepoint
func workStorageInitialize(ctx context.Context, conn state.SQLiteConn) error {
	inTransaction, err := state.InTransaction(conn)
	if err != nil {
		return err
	}
	if !inTransaction {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		if err := workStorageInitializeLocked(ctx, conn); err != nil {
			if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
				return rollbackErr
			}
			return err
		}
		_, err := conn.ExecContext(ctx, "COMMIT")
		return err
	}
	if _, err := conn.ExecContext(ctx, "SAVEPOINT work_record_initialize"); err != nil {
		return err
	}
	if err := workStorageInitializeLocked(ctx, conn); err != nil {
		if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK TO work_record_initialize"); rollbackErr != nil {
			return rollbackErr
		}
		if _, releaseErr := conn.ExecContext(ctx, "RELEASE work_record_initialize"); releaseErr != nil {
			return releaseErr
		}
		return err
	}
	_, err = conn.ExecContext(ctx, "RELEASE work_record_initialize")
	return err
}

// Insert statement of the named columns into table
func workInsertSQL(table string, fields []string) string {
	return "INSERT INTO " + table + " (" + strings.Join(fields, ",") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(fields)), ",") + ")"
}

// Creates the evidence tables, rebuilding pre producer scoped tables with their rows classified, then the guards
func workStorageInitializeLocked(ctx context.Context, conn state.SQLiteConn) error {
	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+workInvalidTable+` (
        evidence_id INTEGER PRIMARY KEY, source_table TEXT NOT NULL,
        room_id TEXT NOT NULL, revision INTEGER NOT NULL, digest TEXT NOT NULL, record_json TEXT NOT NULL,
        target_install_id TEXT, route_generation TEXT, status TEXT,
        disposition TEXT NOT NULL CHECK(disposition='invalid'))`); err != nil {
		return err
	}
	tables := []string{WorkSourceTable, WorkPendingTable}
	for _, table := range tables {
		pending := table == WorkPendingTable
		parent := "hosted_rooms"
		if table == WorkTargetTable {
			parent = "hosted_room_replicas"
		}
		columns, err := hostedTableColumns(ctx, conn, table)
		if err != nil {
			return err
		}
		legacy := len(columns) > 0 && !columns["producer_gateway_id"]
		var rows, triggers []*pyjson.Object
		if legacy {
			if rows, err = state.QueryObjects(ctx, conn, "SELECT * FROM "+table); err != nil {
				return err
			}
			if triggers, err = state.QueryObjects(ctx, conn, "SELECT sql FROM sqlite_master WHERE type='trigger' AND tbl_name=?", table); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "DROP TABLE "+table); err != nil {
				return err
			}
		}
		extra, key := "", ""
		if pending {
			extra, key = "target_install_id TEXT NOT NULL, route_generation TEXT NOT NULL, status TEXT NOT NULL,", ",target_install_id"
		}
		if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+table+` (
            room_id TEXT NOT NULL, revision INTEGER NOT NULL, digest TEXT NOT NULL, record_json TEXT NOT NULL,
            producer_gateway_id TEXT NOT NULL, producer_epoch INTEGER NOT NULL,
            disposition TEXT NOT NULL CHECK(disposition IN ('current','superseded_authority','historical','invalid')),
            `+extra+` PRIMARY KEY(room_id,producer_gateway_id,producer_epoch`+key+`),
            FOREIGN KEY(room_id) REFERENCES `+parent+`(room_id) ON DELETE CASCADE)`); err != nil {
			return err
		}
		for _, row := range rows {
			parentRow, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM "+parent+" WHERE room_id=?", row.Vals["room_id"])
			if err != nil {
				return err
			}
			if parentRow == nil {
				fields := []string{"source_table", "room_id", "revision", "digest", "record_json", "disposition"}
				values := []any{table, row.Vals["room_id"], row.Vals["revision"], row.Vals["digest"], row.Vals["record_json"], "invalid"}
				if pending {
					fields = append(fields, "target_install_id", "route_generation", "status")
					values = append(values, row.Vals["target_install_id"], row.Vals["route_generation"], row.Vals["status"])
				}
				if _, err := conn.ExecContext(ctx, workInsertSQL(workInvalidTable, fields), values...); err != nil {
					return err
				}
				continue
			}
			producer, epoch, disposition := "", int64(0), "invalid"
			if record, err := workLegacyRecord(row); err == nil {
				producer, epoch, disposition = record["home_install_id"].(string), 1, "current"
			}
			fields := []string{"room_id", "revision", "digest", "record_json", "producer_gateway_id", "producer_epoch", "disposition"}
			values := []any{row.Vals["room_id"], row.Vals["revision"], row.Vals["digest"], row.Vals["record_json"], producer, epoch, disposition}
			if pending {
				fields = append(fields, "target_install_id", "route_generation", "status")
				values = append(values, row.Vals["target_install_id"], row.Vals["route_generation"], row.Vals["status"])
			}
			projected := map[string]any{}
			for i, field := range fields {
				projected[field] = values[i]
			}
			if values[6], err = workScopeDisposition(ctx, conn, table, projected); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, workInsertSQL(table, fields), values...); err != nil {
				return err
			}
		}
		for _, trigger := range triggers {
			if _, err := conn.ExecContext(ctx, hrStr(trigger, "sql")); err != nil {
				return err
			}
		}
	}
	if err := workGuards(ctx, conn, tables); err != nil {
		return err
	}
	return workInvalidGuards(ctx, conn)
}

// Version one record a pre producer scoped row holds, an error when it is not exactly its own scope
func workLegacyRecord(row *pyjson.Object) (map[string]any, error) {
	decoded, err := roomLoads(hrStr(row, "record_json"))
	if err != nil {
		return nil, err
	}
	record, err := ValidateWorkRecord(decoded)
	if err != nil {
		return nil, err
	}
	version, _ := hostedStrictInt(record["version"])
	if version != 1 || record["room_id"] != row.Vals["room_id"] || !pyjson.Equal(record["revision"], row.Vals["revision"]) || record["digest"] != row.Vals["digest"] {
		return nil, workRecordError("legacy work scope conflicts")
	}
	if _, ok := record["home_install_id"].(string); !ok {
		return nil, workRecordError("legacy work scope conflicts")
	}
	return record, nil
}

// Read only authority projection of a row disposition
func workScopeDisposition(ctx context.Context, conn state.SQLiteConn, table string, row map[string]any) (string, error) {
	disposition, _ := row["disposition"].(string)
	if disposition != "current" {
		return disposition, nil
	}
	current, err := state.QueryRowObject(ctx, conn, "SELECT authority_gateway_id,authority_epoch FROM hosted_rooms WHERE room_id=?", row["room_id"])
	if err != nil {
		return "", err
	}
	if current == nil || (current.Vals["authority_gateway_id"] == row["producer_gateway_id"] && pyjson.Equal(current.Vals["authority_epoch"], row["producer_epoch"])) {
		return "current", nil
	}
	if table == WorkPendingTable && row["status"] != "acked" {
		return "superseded_authority", nil
	}
	return "historical", nil
}

// Installs the byte and row budget, immutability and authority transition triggers
func workGuards(ctx context.Context, conn state.SQLiteConn, tables []string) error {
	total, count := workUsageSQL(append(append([]string{}, tables...), workInvalidTable))
	exec := func(query string) error {
		_, err := conn.ExecContext(ctx, query)
		return err
	}
	for _, table := range tables {
		for _, operation := range []string{"INSERT", "UPDATE"} {
			key := "room_id=NEW.room_id AND producer_gateway_id=NEW.producer_gateway_id AND producer_epoch=NEW.producer_epoch"
			if table == WorkPendingTable {
				key += " AND target_install_id=NEW.target_install_id"
			}
			oldSize, oldCount := workRowSizeSQL(table, "OLD."), "1"
			if operation == "INSERT" {
				oldSize = "COALESCE((SELECT " + workRowSizeSQL(table, "") + " FROM " + table + " WHERE " + key + "),0)"
				oldCount = "(SELECT COUNT(*) FROM " + table + " WHERE " + key + ")"
			}
			newSize := workRowSizeSQL(table, "NEW.")
			lower := strings.ToLower(operation)
			for _, version := range []string{"v2", "v3", "v4"} {
				if err := exec("DROP TRIGGER IF EXISTS trg_" + table + "_budget_" + version + "_" + lower); err != nil {
					return err
				}
			}
			if err := exec(fmt.Sprintf(`CREATE TRIGGER trg_%s_budget_v4_%s
                BEFORE %s ON %s WHEN (
                    ((%s)>(%s) AND (%s)-(%s)+(%s)>%d)
                    OR (%s=0 AND (%s>%d OR %s+1>%d)))
                BEGIN SELECT RAISE(ABORT, 'work record storage is full'); END`, table, lower, operation, table,
				newSize, oldSize, total, oldSize, newSize, workMaxStoreBytes, oldCount, total, workMaxStoreBytes, count, workMaxStoreRows)); err != nil {
				return err
			}
		}
		if err := exec("CREATE TRIGGER IF NOT EXISTS trg_" + table + `_immutable_v2
            BEFORE UPDATE ON ` + table + ` WHEN
                NEW.room_id!=OLD.room_id OR NEW.producer_gateway_id!=OLD.producer_gateway_id
                OR NEW.producer_epoch!=OLD.producer_epoch
                OR (OLD.disposition!='current' AND (NEW.record_json!=OLD.record_json OR NEW.revision!=OLD.revision
                    OR NEW.digest!=OLD.digest OR NEW.disposition!=OLD.disposition))
            BEGIN SELECT RAISE(ABORT, 'historical work record is immutable'); END`); err != nil {
			return err
		}
		if err := exec("CREATE TRIGGER IF NOT EXISTS trg_" + table + "_delete_v2 BEFORE DELETE ON " + table + `
            WHEN OLD.disposition!='current' AND EXISTS (SELECT 1 FROM hosted_rooms
                WHERE room_id=OLD.room_id AND disbanded_at IS NULL)
            BEGIN SELECT RAISE(ABORT, 'historical work record is immutable'); END`); err != nil {
			return err
		}
		extra := ""
		if table == WorkPendingTable {
			extra = " AND p.target_install_id=NEW.target_install_id"
		}
		if err := exec("CREATE TRIGGER IF NOT EXISTS trg_" + table + `_replace_v2
            BEFORE INSERT ON ` + table + ` WHEN EXISTS (SELECT 1 FROM ` + table + ` p WHERE p.room_id=NEW.room_id
                AND p.producer_gateway_id=NEW.producer_gateway_id AND p.producer_epoch=NEW.producer_epoch
                ` + extra + ` AND p.disposition!='current')
            BEGIN SELECT RAISE(ABORT, 'historical work record is immutable'); END`); err != nil {
			return err
		}
	}
	if err := exec("CREATE TRIGGER IF NOT EXISTS trg_work_pending_outcome_v2 BEFORE UPDATE ON " + WorkPendingTable + `
        WHEN OLD.disposition!='current' AND (NEW.status!=OLD.status OR NEW.route_generation!=OLD.route_generation
            OR NEW.target_install_id!=OLD.target_install_id)
        BEGIN SELECT RAISE(ABORT, 'historical delivery outcome is immutable'); END`); err != nil {
		return err
	}
	return exec(`CREATE TRIGGER IF NOT EXISTS trg_work_authority_transition_v2
        AFTER UPDATE OF authority_gateway_id,authority_epoch ON hosted_rooms BEGIN
        UPDATE ` + WorkSourceTable + ` SET disposition='historical' WHERE room_id=NEW.room_id AND disposition='current'
            AND (producer_gateway_id!=NEW.authority_gateway_id OR producer_epoch!=NEW.authority_epoch);
        UPDATE ` + WorkPendingTable + ` SET disposition=CASE WHEN status='acked' THEN 'historical' ELSE 'superseded_authority' END
            WHERE room_id=NEW.room_id AND disposition='current'
            AND (producer_gateway_id!=NEW.authority_gateway_id OR producer_epoch!=NEW.authority_epoch);
        END`)
}

// Columns whose bytes a retained row is charged for
func workRetainedFields(table string) []string {
	fields := []string{"room_id", "revision", "digest", "record_json"}
	if table == workInvalidTable {
		fields = append(fields, "source_table")
	} else {
		fields = append(fields, "producer_gateway_id", "producer_epoch")
	}
	if table == WorkPendingTable || table == workInvalidTable {
		fields = append(fields, "target_install_id", "route_generation", "status")
	}
	return fields
}

// SQL byte size of one retained row, reserving the longest disposition up front
func workRowSizeSQL(table, prefix string) string {
	return "20+" + strings.Join(mapStrings(workRetainedFields(table), func(k string) string {
		return "COALESCE(length(CAST(" + prefix + k + " AS BLOB)),0)"
	}), "+")
}

// SQL total bytes and row count across the evidence tables
func workUsageSQL(tables []string) (string, string) {
	totals := mapStrings(tables, func(t string) string { return "(SELECT COALESCE(SUM(" + workRowSizeSQL(t, "") + "),0) FROM " + t + ")" })
	counts := mapStrings(tables, func(t string) string { return "(SELECT COUNT(*) FROM " + t + ")" })
	return strings.Join(totals, " + "), strings.Join(counts, " + ")
}

// Validated record of a stored row whose metadata matches its scope, otherwise InvalidStoredWorkRecord
func workValidateStored(row *pyjson.Object) (map[string]any, error) {
	fail := func() (map[string]any, error) { return nil, invalidStoredWorkRecord("stored work evidence is invalid") }
	decoded, err := roomLoads(hrStr(row, "record_json"))
	if err != nil {
		return fail()
	}
	record, err := ValidateWorkRecord(decoded)
	if err != nil {
		return fail()
	}
	scope := workScope(record)
	if row.Vals["disposition"] == "invalid" || scope[0] != row.Vals["room_id"] || scope[1] != row.Vals["producer_gateway_id"] ||
		!pyjson.Equal(scope[2], row.Vals["producer_epoch"]) || !pyjson.Equal(record["revision"], row.Vals["revision"]) || record["digest"] != row.Vals["digest"] {
		return fail()
	}
	return record, nil
}

// Validates the consumed row, invalidating only its unchanged current version
func workValidateStoredLocked(ctx context.Context, conn state.SQLiteConn, table string, row *pyjson.Object) (map[string]any, error) {
	record, err := workValidateStored(row)
	var invalid *InvalidStoredWorkRecord
	if err == nil || !errors.As(err, &invalid) {
		return record, err
	}
	if row.Vals["disposition"] == "current" {
		where := strings.Join(mapStrings(row.Keys, func(k string) string { return `"` + k + `" IS ?` }), " AND ")
		args := make([]any, len(row.Keys))
		for i, key := range row.Keys {
			args[i] = row.Vals[key]
		}
		if _, execErr := conn.ExecContext(ctx, "SAVEPOINT work_record_invalidate"); execErr != nil {
			return nil, execErr
		}
		if _, execErr := conn.ExecContext(ctx, "UPDATE "+table+" SET disposition='invalid' WHERE "+where, args...); execErr != nil {
			if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK TO work_record_invalidate"); rollbackErr != nil {
				return nil, rollbackErr
			}
			if _, releaseErr := conn.ExecContext(ctx, "RELEASE work_record_invalidate"); releaseErr != nil {
				return nil, releaseErr
			}
			return nil, execErr
		}
		if _, execErr := conn.ExecContext(ctx, "RELEASE work_record_invalidate"); execErr != nil {
			return nil, execErr
		}
	}
	return nil, err
}

// Producer scope of a record, room id, authority gateway and epoch
func workScope(record map[string]any) []any {
	authority, _ := record["authority"].(map[string]any)
	return []any{record["room_id"], authority["gateway_id"], authority["epoch"]}
}

// Upserts the current evidence row of a record within the store budget
func workSaveLocked(ctx context.Context, conn state.SQLiteConn, table string, record map[string]any, targetInstallID, routeGeneration *string) error {
	data, err := WorkEncode(record)
	if err != nil {
		return err
	}
	fields := []string{"room_id", "producer_gateway_id", "producer_epoch", "revision", "digest", "record_json", "disposition"}
	values := append(workScope(record), record["revision"], record["digest"], data, "current")
	keys := append([]string{}, fields[:3]...)
	if targetInstallID != nil {
		fields = append(fields, "target_install_id", "route_generation", "status")
		values = append(values, *targetInstallID, *routeGeneration, "pending")
		keys = append(keys, "target_install_id")
	}
	if err := workBudget(ctx, conn, table, fields, values); err != nil {
		return err
	}
	keySet := map[string]bool{}
	for _, key := range keys {
		keySet[key] = true
	}
	var updates []string
	for _, field := range fields {
		if !keySet[field] {
			updates = append(updates, field+"=excluded."+field)
		}
	}
	_, err = conn.ExecContext(ctx, workInsertSQL(table, fields)+" ON CONFLICT("+strings.Join(keys, ",")+") DO UPDATE SET "+strings.Join(updates, ","), values...)
	return err
}

// Makes the opaque invalid evidence table immutable to every ordinary writer
func workInvalidGuards(ctx context.Context, conn state.SQLiteConn) error {
	for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
		if _, err := conn.ExecContext(ctx, "CREATE TRIGGER IF NOT EXISTS trg_work_invalid_"+strings.ToLower(operation)+`
            BEFORE `+operation+` ON `+workInvalidTable+`
            BEGIN SELECT RAISE(ABORT, 'invalid work evidence is immutable'); END`); err != nil {
			return err
		}
	}
	return nil
}
