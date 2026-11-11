package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Successful authority claim of a hosted room peer reservation that a newer authority outranks
type AuthorityConflictError struct{ *HostedRoomError }

func (e *AuthorityConflictError) Unwrap() error { return e.HostedRoomError }

// Python class name of the error
func (e *AuthorityConflictError) PyTypeName() string { return "AuthorityConflictError" }

const hostedJournalModeLockRetries = 8

// Identity columns of a remote run receipt in primary key order
var hostedRemoteRunIdentityColumns = []string{"room_id", "home_install_id", "authority_gateway_id", "authority_epoch",
	"member_id", "target_install_id", "target_profile", "task_id", "execution_generation"}

const hostedRemoteRunsBody = `
            room_id TEXT NOT NULL,
            home_install_id TEXT NOT NULL,
            authority_gateway_id TEXT NOT NULL,
            authority_epoch INTEGER NOT NULL CHECK (authority_epoch >= 1),
            member_id TEXT NOT NULL,
            task_id TEXT NOT NULL,
            execution_generation INTEGER NOT NULL CHECK (execution_generation >= 1),
            target_install_id TEXT NOT NULL,
            target_profile TEXT NOT NULL,
            run_id TEXT NOT NULL,
            session_id TEXT NOT NULL,
            created_at REAL NOT NULL,
            updated_at REAL NOT NULL,
            PRIMARY KEY (
                room_id, home_install_id, authority_gateway_id, authority_epoch,
                member_id, target_install_id, target_profile, task_id,
                execution_generation
            )
        `

// Hosted room tables created in this exact order on first open or migration
var hostedSchemaDDL = []string{
	`CREATE TABLE IF NOT EXISTS hosted_rooms (
            room_id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            members_json TEXT NOT NULL,
            authority_gateway_id TEXT NOT NULL,
            authority_epoch INTEGER NOT NULL DEFAULT 1 CHECK (authority_epoch >= 1),
            next_seq INTEGER NOT NULL DEFAULT 1 CHECK (next_seq >= 1),
            event_bytes INTEGER NOT NULL DEFAULT 0 CHECK (event_bytes >= 0),
            revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
            created_at REAL NOT NULL,
            updated_at REAL NOT NULL,
            disbanded_at REAL
        )`,
	`CREATE TABLE IF NOT EXISTS hosted_room_events (
            room_id TEXT NOT NULL,
            seq INTEGER NOT NULL CHECK (seq >= 1),
            event_id TEXT NOT NULL,
            kind TEXT NOT NULL,
            actor_json TEXT NOT NULL,
            authority_epoch INTEGER CHECK (authority_epoch IS NULL OR authority_epoch >= 1),
            payload_json TEXT NOT NULL,
            created_at REAL NOT NULL,
            PRIMARY KEY (room_id, seq),
            UNIQUE (room_id, event_id),
            FOREIGN KEY (room_id) REFERENCES hosted_rooms(room_id)
        )`,
	`CREATE TABLE IF NOT EXISTS hosted_room_retired_ids (
            room_id TEXT PRIMARY KEY,
            retired_at REAL NOT NULL
        )`,
	`CREATE TABLE IF NOT EXISTS hosted_room_links (
            room_id TEXT NOT NULL,
            member_id TEXT NOT NULL,
            target_url TEXT NOT NULL,
            target_profile TEXT NOT NULL,
            grant TEXT NOT NULL,
            catalog_json TEXT NOT NULL,
            cancellation_scope_id TEXT NOT NULL,
            trace_id TEXT NOT NULL,
            transport_security TEXT NOT NULL,
            status TEXT NOT NULL DEFAULT 'ready',
            updated_at REAL NOT NULL,
            PRIMARY KEY (room_id, member_id)
        )`, "CREATE TABLE IF NOT EXISTS hosted_room_remote_runs (" + hostedRemoteRunsBody + ")",
	`CREATE TABLE IF NOT EXISTS hosted_room_revoked_grants (
            scope_key TEXT PRIMARY KEY,
            expires_at REAL NOT NULL,
            revoked_before REAL NOT NULL
        )`,
	`CREATE TABLE IF NOT EXISTS hosted_room_peer_reservations (
            room_id TEXT NOT NULL,
            member_id TEXT NOT NULL,
            target_profile TEXT NOT NULL,
            authority_gateway_id TEXT NOT NULL,
            authority_epoch INTEGER NOT NULL CHECK (authority_epoch >= 1),
            expires_at REAL NOT NULL,
            revoked_at REAL,
            created_at REAL NOT NULL,
            updated_at REAL NOT NULL,
            PRIMARY KEY (room_id, member_id, target_profile)
        )`}

// Table and the columns its DDL declares, in the order a schema check probes them
type hostedRequiredTable struct {
	table   string
	columns []string
}

var (
	hostedTableNameRE  = regexp.MustCompile(`EXISTS (\w+)`)
	hostedColumnDeclRE = regexp.MustCompile(`(?m)^\s*(\w+) (?:TEXT|INTEGER|REAL)\b`)
)

// Required columns of each table parsed from its DDL
var hostedRequiredColumns = func() []hostedRequiredTable {
	out := make([]hostedRequiredTable, 0, len(hostedSchemaDDL))
	for _, ddl := range hostedSchemaDDL {
		_, body, _ := strings.Cut(ddl, "(")
		var columns []string
		for _, match := range hostedColumnDeclRE.FindAllStringSubmatch(body, -1) {
			columns = append(columns, match[1])
		}
		out = append(out, hostedRequiredTable{hostedTableNameRE.FindStringSubmatch(ddl)[1], columns})
	}
	return out
}()

const hostedRetireFromRooms = "INSERT OR IGNORE INTO hosted_room_retired_ids (room_id, retired_at)" +
	" SELECT room_id, disbanded_at FROM hosted_rooms WHERE %s"

const hostedLiveReservationWhere = "WHERE room_id=? AND target_profile=? AND expires_at>? AND revoked_at IS NULL"

// Column a draft schema lacked, its declaration and the default literal the migration and import apply
type hostedLegacyColumn struct {
	table, column, declaration string
	defaultLiteral             *string
}

// Canonical actor JSON of the legacy system author, quoted for a SQL literal
var hostedLegacyActorJSON = strings.ReplaceAll(`{"id":"legacy","kind":"system"}`, "'", "''")

var hostedLegacyColumns = func() []hostedLegacyColumn {
	text := func(s string) *string { return &s }
	return []hostedLegacyColumn{
		{"hosted_rooms", "authority_gateway_id", "TEXT NOT NULL", text("'legacy'")},
		{"hosted_rooms", "authority_epoch", "INTEGER NOT NULL", text("1")},
		{"hosted_rooms", "event_bytes", "INTEGER NOT NULL", text("0")},
		{"hosted_room_events", "actor_json", "TEXT NOT NULL", text("'" + hostedLegacyActorJSON + "'")},
		{"hosted_room_events", "authority_epoch", "INTEGER", nil}}
}()

const hostedEventBytesBackfill = `UPDATE hosted_rooms
                  SET event_bytes=COALESCE((
                      SELECT SUM(
                          length(CAST(event_id AS BLOB)) +
                          length(CAST(kind AS BLOB)) +
                          length(CAST(actor_json AS BLOB)) +
                          length(CAST(payload_json AS BLOB))
                      )
                      FROM hosted_room_events
                      WHERE hosted_room_events.room_id=hosted_rooms.room_id
                  ), 0) WHERE %s`

// Whether a table exists in the connected database
func hostedTableExists(ctx context.Context, conn state.SQLiteConn, table string) (bool, error) {
	var one int
	err := conn.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Rows of PRAGMA table_info for a table, empty when the table does not exist
func hostedTableInfo(ctx context.Context, conn state.SQLiteConn, table string) ([][]any, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	return hostedScanRows(rows)
}

// Column names of a table, empty when the table does not exist
func hostedTableColumns(ctx context.Context, conn state.SQLiteConn, table string) (map[string]bool, error) {
	info, err := hostedTableInfo(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	columns := make(map[string]bool, len(info))
	for _, row := range info {
		columns[pyrepr.Str(row[1])] = true
	}
	return columns, nil
}

// Every row of a result set as positional values, the rows closed
func hostedScanRows(rows *sql.Rows) ([][]any, error) {
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		for i, value := range values {
			if raw, isBytes := value.([]byte); isBytes {
				values[i] = append([]byte(nil), raw...)
			}
		}
		out = append(out, values)
	}
	return out, rows.Err()
}

// Whether every required column is in columns
func hostedHasColumns(columns map[string]bool, required []string) bool {
	for _, column := range required {
		if !columns[column] {
			return false
		}
	}
	return true
}

// Whether the remote run table carries the full authority lineage primary key
func hostedRemoteRunSchemaCurrent(ctx context.Context, conn state.SQLiteConn, columns map[string]bool) (bool, error) {
	if !hostedHasColumns(columns, hostedRequiredColumns[4].columns) {
		return false, nil
	}
	info, err := hostedTableInfo(ctx, conn, "hosted_room_remote_runs")
	if err != nil {
		return false, err
	}
	type keyed struct {
		name string
		pk   int64
	}
	var keys []keyed
	for _, row := range info {
		pk, err := pyint.Int64(row[5])
		if err != nil {
			return false, err
		}
		if pk != 0 {
			keys = append(keys, keyed{pyrepr.Str(row[1]), pk})
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].pk < keys[j].pk })
	if len(keys) != len(hostedRemoteRunIdentityColumns) {
		return false, nil
	}
	for i, key := range keys {
		if key.name != hostedRemoteRunIdentityColumns[i] {
			return false, nil
		}
	}
	return true, nil
}

// Rebuilds legacy remote run receipts behind the complete authority lineage key
func hostedMigrateRemoteRunSchema(ctx context.Context, conn state.SQLiteConn) error {
	columns, err := hostedTableColumns(ctx, conn, "hosted_room_remote_runs")
	if err != nil {
		return err
	}
	current, err := hostedRemoteRunSchemaCurrent(ctx, conn, columns)
	if err != nil || current {
		return err
	}
	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS hosted_room_remote_runs_migrating"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE hosted_room_remote_runs_migrating ("+hostedRemoteRunsBody+")"); err != nil {
		return err
	}
	if len(columns) > 0 {
		pick := func(column, fallback string) string {
			if columns[column] {
				return column
			}
			return fallback
		}
		homeColumn, gatewayColumn, epochColumn := pick("home_install_id", "'legacy'"), pick("authority_gateway_id", "'legacy'"),
			pick("authority_epoch", "1")
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`INSERT OR IGNORE INTO hosted_room_remote_runs_migrating(
                    room_id, home_install_id, authority_gateway_id,
                    authority_epoch, member_id, task_id,
                    execution_generation, target_install_id, target_profile,
                    run_id, session_id, created_at, updated_at
                )
                SELECT room_id, %s, %s, %s, member_id, task_id,
                       execution_generation, target_install_id, target_profile,
                       run_id, session_id, created_at, updated_at
                  FROM hosted_room_remote_runs`, homeColumn, gatewayColumn, epochColumn)); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "DROP TABLE hosted_room_remote_runs"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "ALTER TABLE hosted_room_remote_runs_migrating RENAME TO hosted_room_remote_runs")
	return err
}

// Adds the columns draft schemas lacked, backfilling event bytes when that column is first introduced
func hostedMigrateLegacyColumns(ctx context.Context, conn state.SQLiteConn) error {
	columns := map[string]map[string]bool{}
	for _, legacy := range hostedLegacyColumns {
		if _, read := columns[legacy.table]; !read {
			found, err := hostedTableColumns(ctx, conn, legacy.table)
			if err != nil {
				return err
			}
			columns[legacy.table] = found
		}
		if columns[legacy.table][legacy.column] {
			continue
		}
		statement := "ALTER TABLE " + legacy.table + " ADD COLUMN " + legacy.column + " " + legacy.declaration
		if legacy.defaultLiteral != nil {
			statement += " DEFAULT " + *legacy.defaultLiteral
		}
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if !columns["hosted_rooms"]["event_bytes"] {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(hostedEventBytesBackfill, "1")); err != nil {
			return err
		}
	}
	return nil
}

// Creates or migrates the hosted room schema, refusing a store the migration left incomplete
func hostedInitializeSchema(ctx context.Context, conn state.SQLiteConn) error {
	for _, statement := range hostedSchemaDDL {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := hostedMigrateLegacyColumns(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(hostedRetireFromRooms, "disbanded_at IS NOT NULL")); err != nil {
		return err
	}
	if err := hostedMigrateRemoteRunSchema(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_hosted_room_events_cursor ON hosted_room_events(room_id, seq)"); err != nil {
		return err
	}
	current, err := hostedSchemaIsCurrent(ctx, conn)
	if err != nil {
		return err
	}
	if !current {
		return hostedRoomError("hosted room schema migration did not complete")
	}
	return nil
}

// Whether every table carries its required columns, the remote run key and the event cursor index
func hostedSchemaIsCurrent(ctx context.Context, conn state.SQLiteConn) (bool, error) {
	actual := make([]map[string]bool, len(hostedRequiredColumns))
	for i, required := range hostedRequiredColumns {
		columns, err := hostedTableColumns(ctx, conn, required.table)
		if err != nil {
			return false, err
		}
		actual[i] = columns
	}
	for i, required := range hostedRequiredColumns {
		if !hostedHasColumns(actual[i], required.columns) {
			return false, nil
		}
		if required.table == "hosted_room_remote_runs" {
			current, err := hostedRemoteRunSchemaCurrent(ctx, conn, actual[i])
			if err != nil || !current {
				return false, err
			}
		}
	}
	var one int
	err := conn.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='index' AND name='idx_hosted_room_events_cursor'").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Hosted room coordination database of the active install, the shared root store for a profile home
func HostedRoomsDefaultDBPath(ctx context.Context) string {
	dir := home.Dir(ctx)
	if filepath.Base(filepath.Dir(dir)) == "profiles" {
		dir = filepath.Dir(filepath.Dir(dir))
	}
	return filepath.Join(dir, "shared-state.db")
}

// Whether the store serves rooms, its schema current and the pre isolation import settled
func hostedStoreReady(ctx context.Context, conn state.SQLiteConn, dbPath string) (bool, error) {
	current, err := hostedSchemaIsCurrent(ctx, conn)
	if err != nil || !current {
		return false, err
	}
	return hostedLegacySettled(ctx, conn, dbPath)
}

// Creates or migrates the schema, then copies the pre isolation rows in
func hostedInitializeStore(ctx context.Context, conn state.SQLiteConn, dbPath string) error {
	if err := hostedInitializeSchema(ctx, conn); err != nil {
		return err
	}
	return hostedImportLegacyRooms(ctx, conn, dbPath)
}

// Opens the shared room store with WAL and foreign keys, initializing it in one IMMEDIATE transaction when not ready
func hostedRoomsConnect(ctx context.Context, dbPath string) (state.SQLiteConn, error) {
	return state.OpenDB(ctx, dbPath, "shared-state.db (hosted_rooms)", state.WithBusyTimeoutMS(10_000), state.WithForeignKeys(),
		state.WithWALLockRetries(hostedJournalModeLockRetries), state.WithInitialize(func(ctx context.Context, conn state.SQLiteConn) error {
			ready, err := hostedStoreReady(ctx, conn, dbPath)
			if err != nil || ready {
				return err
			}
			if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				return err
			}
			if err := hostedInitializeStore(ctx, conn, dbPath); err != nil {
				if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
					return rollbackErr
				}
				return err
			}
			_, err = conn.ExecContext(ctx, "COMMIT")
			return err
		}))
}

// Runs body in one transaction on a fresh store connection, committed on success and always closed
func hostedTransaction(ctx context.Context, dbPath string, immediate bool, body func(conn state.SQLiteConn) error) error {
	conn, err := hostedRoomsConnect(ctx, dbPath)
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, immediate, body)
}

const hostedLegacySourceName = "state.db"

const hostedLegacyMarkerTable = "hosted_room_legacy_imports"

// Liveness tables never copied by the pre isolation import
var hostedLegacySkipTables = map[string]bool{"hosted_room_driver_leases": true}

// Legacy sources this process could not import, retried on the next process start
var hostedLegacyFailed = struct {
	sync.Mutex
	sources map[string]bool
}{sources: map[string]bool{}}

// Pre isolation store of a database, empty when the database has no predecessor
func hostedLegacySourcePath(dbPath string) string {
	if filepath.Base(dbPath) == "shared-state.db" {
		return filepath.Join(filepath.Dir(dbPath), hostedLegacySourceName)
	}
	return ""
}

// Whether the import for this database was recorded, given up on for this process, or never applies
func hostedLegacySettled(ctx context.Context, conn state.SQLiteConn, dbPath string) (bool, error) {
	source := hostedLegacySourcePath(dbPath)
	hostedLegacyFailed.Lock()
	failed := hostedLegacyFailed.sources[source]
	hostedLegacyFailed.Unlock()
	if source == "" || failed {
		return true, nil
	}
	exists, err := hostedTableExists(ctx, conn, hostedLegacyMarkerTable)
	if err != nil || !exists {
		return false, err
	}
	var one int
	err = conn.QueryRowContext(ctx, "SELECT 1 FROM "+hostedLegacyMarkerTable+" WHERE source=?", hostedLegacySourceName).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Target column and source expression pairs copying one legacy table, target only columns taking their migration default
func hostedSelectExpressions(ctx context.Context, legacy, target state.SQLiteConn, name string) ([][2]string, error) {
	info, err := hostedTableInfo(ctx, legacy, name)
	if err != nil {
		return nil, err
	}
	sourceColumns := map[string]bool{}
	var pairs [][2]string
	targetColumns, err := hostedTableColumns(ctx, target, name)
	if err != nil {
		return nil, err
	}
	for _, row := range info {
		column := pyrepr.Str(row[1])
		sourceColumns[column] = true
		if targetColumns[column] {
			pairs = append(pairs, [2]string{column, column})
		}
	}
	for _, legacyColumn := range hostedLegacyColumns {
		if legacyColumn.defaultLiteral == nil || legacyColumn.table != name {
			continue
		}
		if targetColumns[legacyColumn.column] && !sourceColumns[legacyColumn.column] {
			pairs = append(pairs, [2]string{legacyColumn.column, *legacyColumn.defaultLiteral})
		}
	}
	return pairs, nil
}

// Copies every room the target lacks with all its child rows, returning the number of rooms copied
func hostedCopyRows(ctx context.Context, target state.SQLiteConn, source string, only map[string]bool) (int, error) {
	existingRows, err := target.QueryContext(ctx, "SELECT room_id FROM hosted_rooms")
	if err != nil {
		return 0, err
	}
	scanned, err := hostedScanRows(existingRows)
	if err != nil {
		return 0, err
	}
	existing := map[string]bool{}
	for _, row := range scanned {
		existing[pyrepr.Str(row[0])] = true
	}
	legacy, err := state.ConnectSQLite("file:"+source+"?mode=ro", state.SQLiteConfig{Timeout: 10, URI: true})
	if err != nil {
		return 0, err
	}
	defer legacy.Close()
	nameRows, err := legacy.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name GLOB 'hosted_room*'")
	if err != nil {
		return 0, err
	}
	scannedNames, err := hostedScanRows(nameRows)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(scannedNames))
	for _, row := range scannedNames {
		names = append(names, pyrepr.Str(row[0]))
	}
	sort.SliceStable(names, func(i, j int) bool {
		if (names[i] != "hosted_rooms") != (names[j] != "hosted_rooms") {
			return names[i] == "hosted_rooms"
		}
		return names[i] < names[j]
	})
	var copiedRooms []string
	for _, name := range names {
		if hostedLegacySkipTables[name] || name == hostedLegacyMarkerTable || strings.HasSuffix(name, "_next") || strings.HasSuffix(name, "_migrating") {
			continue
		}
		exists, err := hostedTableExists(ctx, target, name)
		if err != nil {
			return 0, err
		}
		if !exists {
			var ddl any
			if err := legacy.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&ddl); err != nil {
				return 0, err
			}
			if _, err := target.ExecContext(ctx, pyrepr.Str(ddl)); err != nil {
				return 0, err
			}
		}
		pairs, err := hostedSelectExpressions(ctx, legacy, target, name)
		if err != nil {
			return 0, err
		}
		if len(pairs) == 0 {
			continue
		}
		columns := make([]string, len(pairs))
		expressions := make([]string, len(pairs))
		roomIndex := -1
		for i, pair := range pairs {
			columns[i], expressions[i] = pair[0], pair[1]
			if pair[0] == "room_id" && roomIndex < 0 {
				roomIndex = i
			}
		}
		sourceRows, err := legacy.QueryContext(ctx, "SELECT "+strings.Join(expressions, ", ")+" FROM "+name)
		if err != nil {
			return 0, err
		}
		rows, err := hostedScanRows(sourceRows)
		if err != nil {
			return 0, err
		}
		verb := "INSERT OR IGNORE"
		if roomIndex >= 0 {
			kept := rows[:0]
			for _, row := range rows {
				if room := pyrepr.Str(row[roomIndex]); !existing[room] && (only == nil || only[room]) {
					kept = append(kept, row)
				}
			}
			rows, verb = kept, "INSERT"
		} else if only != nil {
			continue
		}
		if name == "hosted_rooms" {
			copiedRooms = copiedRooms[:0]
			for _, row := range rows {
				copiedRooms = append(copiedRooms, pyrepr.Str(row[roomIndex]))
			}
		}
		statement := verb + " INTO " + name + " (" + strings.Join(columns, ", ") + ") VALUES (" +
			strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ") + ")"
		for _, row := range rows {
			if _, err := target.ExecContext(ctx, statement, row...); err != nil {
				return 0, err
			}
		}
	}
	if len(copiedRooms) > 0 {
		legacyColumns, err := hostedTableColumns(ctx, legacy, "hosted_rooms")
		if err != nil {
			return 0, err
		}
		if !legacyColumns["event_bytes"] {
			args := make([]any, len(copiedRooms))
			for i, room := range copiedRooms {
				args[i] = room
			}
			where := "room_id IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(copiedRooms)), ", ") + ")"
			if _, err := target.ExecContext(ctx, fmt.Sprintf(hostedEventBytesBackfill, where), args...); err != nil {
				return 0, err
			}
		}
	}
	return len(copiedRooms), nil
}

// Copies the pre isolation rows in once under a savepoint and records the marker, never failing the open
func hostedImportLegacyRooms(ctx context.Context, conn state.SQLiteConn, dbPath string) error {
	source := hostedLegacySourcePath(dbPath)
	if source == "" {
		return nil
	}
	settled, err := hostedLegacySettled(ctx, conn, dbPath)
	if err != nil || settled {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SAVEPOINT legacy_import"); err != nil {
		return err
	}
	copied := 0
	if info, statErr := os.Stat(source); statErr == nil && info.Mode().IsRegular() {
		copied, err = hostedCopyRows(ctx, conn, source, nil)
	}
	if err != nil {
		if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK TO legacy_import"); rollbackErr != nil {
			return rollbackErr
		}
		if _, releaseErr := conn.ExecContext(ctx, "RELEASE legacy_import"); releaseErr != nil {
			return releaseErr
		}
		hostedLegacyFailed.Lock()
		hostedLegacyFailed.sources[source] = true
		hostedLegacyFailed.Unlock()
		slog.Warn(fmt.Sprintf("hosted rooms: could not import the pre-isolation %s (%v); will retry on the next start", source, err))
		return nil
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+hostedLegacyMarkerTable+
		" (source TEXT PRIMARY KEY, imported_at REAL NOT NULL, rooms INTEGER NOT NULL)"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO "+hostedLegacyMarkerTable+" (source, imported_at, rooms) VALUES (?, ?, ?)",
		hostedLegacySourceName, hostedClock(nil), copied); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "RELEASE legacy_import"); err != nil {
		return err
	}
	if copied > 0 {
		slog.Info(fmt.Sprintf("hosted rooms: imported %d pre-isolation Group Chat(s) from %s", copied, source))
	}
	return nil
}

// Given time, or the current wall clock when nil
func hostedClock(now *float64) float64 {
	if now != nil {
		return *now
	}
	return float64(time.Now().UnixNano()) / 1e9
}

// Text of each claim key, empty for a falsy value
func hostedClaimValues(claims *pyjson.Object, keys ...string) []string {
	out := make([]string, len(keys))
	for i, key := range keys {
		if value := claims.GetOr(key, nil); pyrepr.Truthy(value) {
			out[i] = pyrepr.Str(value)
		}
	}
	return out
}

// Python int of a claim value, zero for a falsy one
func hostedClaimInt(claims *pyjson.Object, key string) (int64, error) {
	value := claims.GetOr(key, nil)
	if !pyrepr.Truthy(value) {
		return 0, nil
	}
	n, err := pyint.Of(value)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Python float of a claim value, zero for a falsy one
func hostedClaimFloat(claims *pyjson.Object, key string) (float64, error) {
	value := claims.GetOr(key, nil)
	if !pyrepr.Truthy(value) {
		return 0, nil
	}
	return pyint.Float(value)
}

// Stable non secret key of one room, home, target and profile scope
func hostedRoomGrantScopeKey(claims *pyjson.Object) (string, error) {
	keys := []string{"room_id", "home_install_id", "authority_gateway_id", "authority_epoch", "member_id", "target_install_id", "target_profile"}
	values := hostedClaimValues(claims, keys...)
	fields := make(map[string]any, len(keys))
	for i, key := range keys {
		if values[i] == "" {
			return "", hostedRoomError("room grant scope is incomplete")
		}
		fields[key] = values[i]
	}
	encoded, err := roomCompactJSON(fields, true)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

// Revokes every grant issued at or before now for one exact room scope
func RevokeRoomGrantScope(ctx context.Context, dbPath string, claims *pyjson.Object, expiresAt float64, now *float64) error {
	scopeKey, err := hostedRoomGrantScopeKey(claims)
	if err != nil {
		return err
	}
	timestamp := hostedClock(now)
	if expiresAt <= timestamp {
		return nil
	}
	values := hostedClaimValues(claims, "room_id", "member_id", "target_profile", "authority_gateway_id")
	epoch, err := hostedClaimInt(claims, "authority_epoch")
	if err != nil {
		return err
	}
	return hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_revoked_grants WHERE expires_at<=?", timestamp); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_revoked_grants(
                   scope_key, expires_at, revoked_before
               ) VALUES (?, ?, ?)
               ON CONFLICT(scope_key) DO UPDATE SET
                   expires_at=MAX(hosted_room_revoked_grants.expires_at,
                                  excluded.expires_at),
                   revoked_before=MAX(hosted_room_revoked_grants.revoked_before,
                                      excluded.revoked_before)`, scopeKey, expiresAt, timestamp); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `UPDATE hosted_room_peer_reservations SET revoked_at=?, updated_at=? WHERE room_id=?
                AND member_id=? AND target_profile=? AND authority_gateway_id=?
                AND authority_epoch=?`, timestamp, timestamp, values[0], values[1], values[2], values[3], epoch)
		return err
	})
}

// Validated room, member, target profile, authority gateway and positive authority epoch of a grant
type hostedReservation struct {
	roomID, memberID, targetProfile, gatewayID string
	epoch                                      int64
}

// Reservation identity of a grant's claims, refused when any part is invalid
func hostedReservationClaims(claims *pyjson.Object) (hostedReservation, error) {
	var r hostedReservation
	var err error
	if r.roomID, err = roomIdentifier(claims.GetOr("room_id", nil), roomIdentifierSpec{label: "room_id", maxChars: 128}, hostedRoomError); err != nil {
		return r, err
	}
	if r.memberID, err = hostedActorID(claims.GetOr("member_id", nil), "member_id"); err != nil {
		return r, err
	}
	if r.targetProfile, err = hostedActorID(claims.GetOr("target_profile", nil), "target_profile"); err != nil {
		return r, err
	}
	if r.gatewayID, err = hostedActorID(claims.GetOr("authority_gateway_id", nil), "authority_gateway_id"); err != nil {
		return r, err
	}
	if r.epoch, err = hostedClaimInt(claims, "authority_epoch"); err != nil {
		return r, err
	}
	if r.epoch < 1 {
		return r, hostedRoomError("authority_epoch must be positive")
	}
	return r, nil
}

// Whether a newer epoch, or the same epoch under another gateway, outranks this claim
func hostedReservationSuperseded(rowGateway any, rowEpoch any, gatewayID string, epoch int64) (bool, error) {
	stored, err := pyint.Int64(rowEpoch)
	if err != nil {
		return false, err
	}
	return stored > epoch || (stored == epoch && pyrepr.Str(rowGateway) != gatewayID), nil
}

// Fences direct Desktop prompts for a room before the first peer run is admitted
func ReservePeerRoom(ctx context.Context, dbPath string, claims *pyjson.Object, expiresAt float64, now *float64) error {
	timestamp := hostedClock(now)
	if expiresAt <= timestamp {
		return hostedRoomError("peer room reservation must expire in the future")
	}
	r, err := hostedReservationClaims(claims)
	if err != nil {
		return err
	}
	conflict := &AuthorityConflictError{&HostedRoomError{"peer room reservation authority changed"}}
	return hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_peer_reservations WHERE expires_at<=?", timestamp); err != nil {
			return err
		}
		live, err := conn.QueryContext(ctx, "SELECT authority_gateway_id, authority_epoch\n                FROM hosted_room_peer_reservations "+
			hostedLiveReservationWhere, r.roomID, r.targetProfile, timestamp)
		if err != nil {
			return err
		}
		authorityRows, err := hostedScanRows(live)
		if err != nil {
			return err
		}
		for _, row := range authorityRows {
			superseded, err := hostedReservationSuperseded(row[0], row[1], r.gatewayID, r.epoch)
			if err != nil {
				return err
			}
			if superseded {
				return conflict
			}
		}
		if _, err := conn.ExecContext(ctx, `UPDATE hosted_room_peer_reservations SET revoked_at=?, updated_at=? WHERE room_id=?
                AND target_profile=? AND authority_epoch<? AND revoked_at IS NULL`, timestamp, timestamp, r.roomID, r.targetProfile, r.epoch); err != nil {
			return err
		}
		existing, err := conn.QueryContext(ctx, `SELECT authority_gateway_id, authority_epoch FROM hosted_room_peer_reservations
                WHERE room_id=? AND member_id=? AND target_profile=?`, r.roomID, r.memberID, r.targetProfile)
		if err != nil {
			return err
		}
		existingRows, err := hostedScanRows(existing)
		if err != nil {
			return err
		}
		if len(existingRows) > 0 {
			superseded, err := hostedReservationSuperseded(existingRows[0][0], existingRows[0][1], r.gatewayID, r.epoch)
			if err != nil {
				return err
			}
			if superseded {
				return conflict
			}
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO hosted_room_peer_reservations(
                   room_id, member_id, target_profile, authority_gateway_id,
                   authority_epoch, expires_at, revoked_at, created_at, updated_at
               ) VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?)
               ON CONFLICT(room_id, member_id, target_profile) DO UPDATE SET
                   authority_gateway_id=excluded.authority_gateway_id,
                   authority_epoch=excluded.authority_epoch,
                   expires_at=MAX(hosted_room_peer_reservations.expires_at,
                                  excluded.expires_at),
                   revoked_at=NULL,
                   updated_at=excluded.updated_at`, r.roomID, r.memberID, r.targetProfile, r.gatewayID, r.epoch, expiresAt, timestamp, timestamp)
		return err
	})
}

// First row of one read in its own store transaction, nil when it returns none
func hostedReadOne(ctx context.Context, dbPath, query string, args ...any) ([]any, error) {
	var out []any
	err := hostedTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		scanned, err := hostedScanRows(rows)
		if err == nil && len(scanned) > 0 {
			out = scanned[0]
		}
		return err
	})
	return out, err
}

// Whether a grant matches the target's current live reservation
func PeerRoomGrantIsCurrent(ctx context.Context, dbPath string, claims *pyjson.Object, now *float64) (bool, error) {
	timestamp := hostedClock(now)
	r, err := hostedReservationClaims(claims)
	if err != nil {
		return false, err
	}
	row, err := hostedReadOne(ctx, dbPath, `SELECT 1 FROM hosted_room_peer_reservations WHERE room_id=? AND member_id=?
            AND target_profile=? AND authority_gateway_id=? AND authority_epoch=?
            AND expires_at>? AND revoked_at IS NULL LIMIT 1`, r.roomID, r.memberID, r.targetProfile, r.gatewayID, r.epoch, timestamp)
	return row != nil, err
}

// Whether a grant predates its exact scope's revocation fence
func RoomGrantIsRevoked(ctx context.Context, dbPath string, claims *pyjson.Object, now *float64) (bool, error) {
	timestamp := hostedClock(now)
	scopeKey, err := hostedRoomGrantScopeKey(claims)
	if err != nil {
		return false, err
	}
	issuedAt, err := hostedClaimFloat(claims, "issued_at")
	if err != nil {
		return false, err
	}
	row, err := hostedReadOne(ctx, dbPath, `SELECT revoked_before FROM hosted_room_revoked_grants
            WHERE scope_key=? AND expires_at>?`, scopeKey, timestamp)
	if err != nil || row == nil {
		return false, err
	}
	revokedBefore, err := pyint.Float(row[0])
	if err != nil {
		return false, err
	}
	return issuedAt <= revokedBefore, nil
}
