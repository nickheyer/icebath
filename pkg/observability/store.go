package observability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	packageSchemaVersion       = "icebath.shared_metrics.v4"
	storeSchemaVersion         = "2"
	busyTimeoutMS              = 250
	schemaBusyTimeoutMS        = 5000
	deferredWritesMax          = 1024
	localHistoryRetentionDays  = 30
	activeInstallStateKey      = "client_active_recorded_at"
	installSnapshotStateKey    = "install_snapshot_recorded_at"
	milestoneStatePrefix       = "milestone:"
	featureStatePrefix         = "feature:"
	dailyOnceStatePrefix       = "daily_once:"
	activeInstallInterval      = 24 * time.Hour
	createTelemetryStateSQL    = `CREATE TABLE IF NOT EXISTS telemetry_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)`
	createCounterAggregatesSQL = `CREATE TABLE IF NOT EXISTS counter_aggregates (
		period_start TEXT NOT NULL,
		metric_name TEXT NOT NULL,
		icebath_version TEXT NOT NULL,
		os_family TEXT NOT NULL,
		architecture TEXT NOT NULL,
		install_method TEXT NOT NULL,
		dimensions_json TEXT NOT NULL,
		value INTEGER NOT NULL,
		packaged_value INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (
			period_start, metric_name, icebath_version, os_family, architecture,
			install_method, dimensions_json
		)
	)`
	createPackageOutboxSQL = `CREATE TABLE IF NOT EXISTS package_outbox (
		package_id TEXT PRIMARY KEY,
		period_start TEXT NOT NULL,
		period_end TEXT NOT NULL,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		exported_at TEXT
	)`
	pendingPeriodCountSQL = `SELECT COUNT(*) AS period_count FROM (
		SELECT period_start, icebath_version, os_family, architecture, install_method
		FROM counter_aggregates WHERE value > packaged_value
		GROUP BY period_start, icebath_version, os_family, architecture, install_method
	)`
	incrementCounterSQL = `INSERT INTO counter_aggregates(
		period_start, metric_name, icebath_version, os_family, architecture,
		install_method, dimensions_json, value, packaged_value
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
	ON CONFLICT(
		period_start, metric_name, icebath_version, os_family, architecture,
		install_method, dimensions_json
	)
	DO UPDATE SET value = value + excluded.value`
	upsertStateSQL = "INSERT INTO telemetry_state(key, value) VALUES (?, ?)" +
		" ON CONFLICT(key) DO UPDATE SET value = excluded.value"
)

var (
	resourceColumns        = []string{"icebath_version", "os_family", "architecture", "install_method"}
	createConsentTablesSQL = []string{
		`CREATE TABLE IF NOT EXISTS send_consent_windows (
			opened_at TEXT NOT NULL,
			last_confirmed_at TEXT NOT NULL,
			closed_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS consent_marks (
			name TEXT PRIMARY KEY CHECK (name IN ('obs', 'data')),
			stamp TEXT NOT NULL
		)`,
	}
	migrateV1SQL = []string{
		"ALTER TABLE counter_aggregates RENAME TO counter_aggregates_v1",
		createCounterAggregatesSQL,
		`INSERT INTO counter_aggregates(
			period_start, metric_name, icebath_version, os_family, architecture,
			install_method, dimensions_json, value, packaged_value
		)
		SELECT
			period_start, metric_name, icebath_version, 'unknown', 'unknown', 'unknown',
			dimensions_json, value, packaged_value
		FROM counter_aggregates_v1`,
		"DROP TABLE counter_aggregates_v1",
	}
	// sendColumns are the additive nullable transmission bookkeeping columns of package_outbox
	sendColumns = [][2]string{
		{"sent_at", "TEXT"},
		{"send_state", "TEXT"},
		{"send_attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"next_attempt_at", "TEXT"},
		{"last_error", "TEXT"},
		{"sent_install_id", "TEXT"},
		{"claim_token", "TEXT"},
	}
	deferringStoresMu sync.Mutex
	deferringStores   []*SharedMetricsStore
	// Current UTC time, replaced by tests that move the clock
	utcNow  = func() time.Time { return time.Now().UTC() }
	newUUID = func() string { return uuid.NewString() }
)

// Writes every store's deferred rows before a hard exit, waiting on a busy store no later than the deadline
func DrainDeferringStores(ctx context.Context, deadline time.Time) {
	deferringStoresMu.Lock()
	stores := append([]*SharedMetricsStore(nil), deferringStores...)
	deferringStoresMu.Unlock()
	for _, store := range stores {
		remaining := time.Until(deadline).Milliseconds()
		if remaining <= 0 {
			return
		}
		store.DrainDeferred(ctx, int(remaining))
	}
}

var drainDeferringStores = DrainDeferringStores

// Drains each store that deferred a write with the schema wait, newest registration first like atexit
func drainDeferringStoresAtExit(ctx context.Context) {
	deferringStoresMu.Lock()
	stores := append([]*SharedMetricsStore(nil), deferringStores...)
	deferringStoresMu.Unlock()
	for i := len(stores) - 1; i >= 0; i-- {
		stores[i].DrainDeferred(ctx, schemaBusyTimeoutMS)
	}
}

// UTC isoformat with a Z suffix
func isoformat(value time.Time) string {
	return strings.ReplaceAll(pydatetime.ISOFormatUTC(value), "+00:00", "Z")
}

// Sorted compact ASCII JSON of a value
func compactJSON(value any) string {
	text, err := pyjson.Dumps(value, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		panic(err)
	}
	return text
}

// UTC calendar day of an instant as YYYY-MM-DD
func utcDate(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Creates a private directory or file and narrows its mode, a failing chmod ignored
func ensurePrivate(path string, mode os.FileMode) error {
	if mode == 0o700 {
		if err := os.MkdirAll(path, mode); err != nil {
			return err
		}
	} else if _, err := os.Stat(path); err == nil {
		now := time.Now()
		if err := os.Chtimes(path, now, now); err != nil {
			return err
		}
	} else {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		f.Close()
	}
	_ = os.Chmod(path, mode)
	return nil
}

// UnsupportedError is the ValueError the store raises for a metric, dimension set or resource off the contract
type UnsupportedError struct{ Message string }

func (e *UnsupportedError) Error() string { return e.Message }

// storeWrite is one write run inside a store write transaction
type storeWrite func(ctx context.Context, conn state.SQLiteConn) error

// RollupRow is one closed period row a rollup update records
type RollupRow struct {
	Metric      string
	Dimensions  map[string]string
	Resource    map[string]string
	PeriodStart string
}

// CounterRow is one cumulative counter the snapshot reports
type CounterRow struct {
	PeriodStart   string
	MetricName    string
	Resource      map[string]string
	Dimensions    any
	Value         int64
	PackagedValue int64
}

// SharedMetricsStore persists allowlisted counters and exports immutable delta packages
type SharedMetricsStore struct {
	DatabasePath    string
	OutboxDirectory string
	deferredMu      sync.Mutex
	deferred        []storeWrite
	drainAtExit     bool
}

// Resource dimensions of one aggregate row keyed by column, values in column order
func rowResource(values []string) map[string]string {
	resource := make(map[string]string, len(resourceColumns))
	for i, column := range resourceColumns {
		resource[column] = values[i]
	}
	return resource
}

// Opens the store under the profile's telemetry/shared_metrics, empty paths taking the defaults
func NewSharedMetricsStore(ctx context.Context, databasePath, outboxDirectory string) (*SharedMetricsStore, error) {
	root := filepath.Join(home.Dir(ctx), "telemetry", "shared_metrics")
	s := &SharedMetricsStore{DatabasePath: databasePath, OutboxDirectory: outboxDirectory}
	if s.DatabasePath == "" {
		s.DatabasePath = filepath.Join(root, "metrics.sqlite3")
	}
	if s.OutboxDirectory == "" {
		s.OutboxDirectory = filepath.Join(root, "outbox")
	}
	for _, p := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Dir(s.DatabasePath), 0o700}, {s.OutboxDirectory, 0o700}, {s.DatabasePath, 0o600}} {
		if err := ensurePrivate(p.path, p.mode); err != nil {
			return nil, err
		}
	}
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Increments the terminal model call counter for the current UTC day
func (s *SharedMetricsStore) RecordModelCall(ctx context.Context, dimensions, resource map[string]string) error {
	return s.RecordCounter(ctx, ModelRouteMetric, dimensions, resource, 1)
}

// Records this install at most once in any rolling 24 hour window
func (s *SharedMetricsStore) RecordClientActive(ctx context.Context, resource map[string]string) (bool, error) {
	return s.recordOncePerInterval(ctx, activeInstallStateKey, ClientActiveMetric, map[string]string{}, resource)
}

// Records the configuration snapshot at most once in any rolling 24 hour window
func (s *SharedMetricsStore) RecordInstallSnapshot(ctx context.Context, dimensions, resource map[string]string) (bool, error) {
	return s.recordOncePerInterval(ctx, installSnapshotStateKey, InstallSnapshotMetric, dimensions, resource)
}

// Reports whether the snapshot latch would admit a snapshot now
func (s *SharedMetricsStore) InstallSnapshotDue(ctx context.Context) (bool, error) {
	var last *time.Time
	err := s.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		value, found, err := stateValue(ctx, conn, installSnapshotStateKey)
		if err == nil && found {
			last = parseStateTimestamp(value)
		}
		return err
	})
	if err != nil {
		return false, err
	}
	now := utcNow()
	return last == nil || last.After(now) || !now.Before(last.Add(activeInstallInterval)), nil
}

// One telemetry_state value by key
func stateValue(ctx context.Context, conn state.SQLiteConn, key string) (string, bool, error) {
	var value string
	err := conn.QueryRowContext(ctx, "SELECT value FROM telemetry_state WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// Transactional compare and set latch shared by concurrent processes
func (s *SharedMetricsStore) recordOncePerInterval(ctx context.Context, stateKey, metricName string, dimensions, resource map[string]string) (bool, error) {
	if err := validateCounter(metricName, dimensions, resource); err != nil {
		return false, err
	}
	now := utcNow()
	recorded := false
	err := s.write(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		value, found, err := stateValue(ctx, conn, stateKey)
		if err != nil {
			return err
		}
		var last *time.Time
		if found {
			last = parseStateTimestamp(value)
		}
		if last != nil && last.After(now) {
			_, err := conn.ExecContext(ctx, "UPDATE telemetry_state SET value = ? WHERE key = ?", isoformat(now), stateKey)
			return err
		}
		if last != nil && now.Before(last.Add(activeInstallInterval)) {
			return nil
		}
		if _, err := installID(ctx, conn); err != nil {
			return err
		}
		if err := recordCounterInTransaction(ctx, conn, metricName, dimensions, resource, utcDate(now), 1); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, upsertStateSQL, stateKey, isoformat(now)); err != nil {
			return err
		}
		recorded = true
		return nil
	})
	return recorded && err == nil, err
}

// Adds amount, one for event counters or a token count for sums, for the current UTC day
func (s *SharedMetricsStore) RecordCounter(ctx context.Context, metricName string, dimensions, resource map[string]string, amount int64) error {
	if err := validateCounter(metricName, dimensions, resource); err != nil {
		return err
	}
	if amount < 1 || (amount != 1 && !SumMetrics.Has(metricName)) {
		return &UnsupportedError{"Unsupported amount for shared metric: " + metricName}
	}
	periodStart := utcDate(utcNow())
	return s.writeOrDefer(ctx, func(ctx context.Context, conn state.SQLiteConn) error {
		return recordCounterInTransaction(ctx, conn, metricName, dimensions, resource, periodStart, amount)
	})
}

// Read modify writes one JSON rollup state and records the rows it closes in one write transaction
func (s *SharedMetricsStore) UpdateRollupState(ctx context.Context, stateKey string, update func(state map[string]any) (any, []RollupRow)) error {
	return s.writeOrDefer(ctx, func(ctx context.Context, conn state.SQLiteConn) error {
		return updateRollupState(ctx, conn, stateKey, update)
	})
}

// Rollup body inside the caller's transaction, rows off the contract dropped
func updateRollupState(ctx context.Context, conn state.SQLiteConn, stateKey string, update func(map[string]any) (any, []RollupRow)) error {
	value, found, err := stateValue(ctx, conn, stateKey)
	if err != nil {
		return err
	}
	var current map[string]any
	if found {
		if decoded, err := pyjson.Loads(value); err == nil {
			if obj, ok := pyjson.Plain(decoded).(map[string]any); ok {
				current = obj
			}
		}
	}
	newState, rows := update(current)
	var valid []RollupRow
	for _, r := range rows {
		if CounterDimensionsAreValid(r.Metric, anyDims(r.Dimensions)) && ClientResourceIsValid(anyDims(r.Resource)) {
			valid = append(valid, r)
		}
	}
	if len(valid) > 0 {
		if _, err := installID(ctx, conn); err != nil {
			return err
		}
	}
	for _, r := range valid {
		if err := recordCounterInTransaction(ctx, conn, r.Metric, r.Dimensions, r.Resource, r.PeriodStart, 1); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, upsertStateSQL, stateKey, compactJSON(newState))
	return err
}

// Milestones latched so far
func (s *SharedMetricsStore) RecordedMilestones(ctx context.Context) (ValueSet, error) {
	return s.latched(ctx, milestoneStatePrefix)
}

// Records an install milestone at most once ever
func (s *SharedMetricsStore) RecordMilestone(ctx context.Context, milestone, installAgeBucket string, resource map[string]string) (bool, error) {
	dimensions := map[string]string{"install_age_bucket": installAgeBucket, "milestone": milestone}
	return s.recordOnceEver(ctx, milestoneStatePrefix+milestone, MilestoneMetric, dimensions, resource)
}

// Features whose first use is latched so far
func (s *SharedMetricsStore) RecordedFeatures(ctx context.Context) (ValueSet, error) {
	return s.latched(ctx, featureStatePrefix)
}

// Records a feature's first use at most once per install ever
func (s *SharedMetricsStore) RecordFeatureAdoption(ctx context.Context, feature, daysBucket string, resource map[string]string) (bool, error) {
	dimensions := map[string]string{"days_since_install_bucket": daysBucket, "feature": feature}
	return s.recordOnceEver(ctx, featureStatePrefix+feature, FeatureAdoptionMetric, dimensions, resource)
}

// Counts at most once per UTC day per dimension set, the surface attributing the first one that day
func (s *SharedMetricsStore) RecordCounterOncePerDay(ctx context.Context, metricName string, dimensions, resource map[string]string) (bool, error) {
	if err := validateCounter(metricName, dimensions, resource); err != nil {
		return false, err
	}
	today := utcDate(utcNow())
	keyDims := map[string]any{}
	for k, v := range dimensions {
		if k != "surface" {
			keyDims[k] = v
		}
	}
	key := dailyOnceStatePrefix + metricName + ":" + compactJSON(keyDims)
	recorded := false
	err := s.write(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		res, err := conn.ExecContext(ctx, "INSERT INTO telemetry_state(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE"+
			" SET value = excluded.value WHERE telemetry_state.value != excluded.value", key, today)
		if err != nil {
			return err
		}
		if changed, err := res.RowsAffected(); err != nil || changed == 0 {
			return err
		}
		if _, err := installID(ctx, conn); err != nil {
			return err
		}
		if err := recordCounterInTransaction(ctx, conn, metricName, dimensions, resource, today, 1); err != nil {
			return err
		}
		recorded = true
		return nil
	})
	return recorded && err == nil, err
}

// Keys latched under a state prefix with the prefix removed
func (s *SharedMetricsStore) latched(ctx context.Context, prefix string) (ValueSet, error) {
	out := ValueSet{}
	err := s.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT key FROM telemetry_state WHERE key LIKE ?", prefix+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			if len(key) >= len(prefix) {
				out[key[len(prefix):]] = struct{}{}
			}
		}
		return rows.Err()
	})
	return out, err
}

// Records one counter the first time its state key is inserted, never again
func (s *SharedMetricsStore) recordOnceEver(ctx context.Context, stateKey, metricName string, dimensions, resource map[string]string) (bool, error) {
	if err := validateCounter(metricName, dimensions, resource); err != nil {
		return false, err
	}
	now := utcNow()
	recorded := false
	err := s.write(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		res, err := conn.ExecContext(ctx, "INSERT INTO telemetry_state(key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING",
			stateKey, isoformat(now))
		if err != nil {
			return err
		}
		if inserted, err := res.RowsAffected(); err != nil || inserted == 0 {
			return err
		}
		if _, err := installID(ctx, conn); err != nil {
			return err
		}
		if err := recordCounterInTransaction(ctx, conn, metricName, dimensions, resource, utcDate(now), 1); err != nil {
			return err
		}
		recorded = true
		return nil
	})
	return recorded && err == nil, err
}

// Rejects a metric, dimension set or resource the contract does not allow
func validateCounter(metricName string, dimensions, resource map[string]string) error {
	if !CounterMetrics.Has(metricName) {
		return &UnsupportedError{"Unsupported shared metric: " + metricName}
	}
	if !CounterDimensionsAreValid(metricName, anyDims(dimensions)) {
		return &UnsupportedError{"Unsupported dimensions for shared metric: " + metricName}
	}
	if resource == nil || !ClientResourceIsValid(anyDims(resource)) {
		return &UnsupportedError{"Unsupported shared-metrics client resource"}
	}
	return nil
}

// Upserts one counter row inside the caller's transaction
func recordCounterInTransaction(ctx context.Context, conn state.SQLiteConn, metricName string, dimensions, resource map[string]string, periodStart string, amount int64) error {
	args := []any{periodStart, metricName}
	for _, column := range resourceColumns {
		args = append(args, resource[column])
	}
	args = append(args, compactJSON(anyDims(dimensions)), amount)
	_, err := conn.ExecContext(ctx, incrementCounterSQL, args...)
	return err
}

// Commits every pending delta package, one per period, then exports the outbox
func (s *SharedMetricsStore) CreateAndExportPackage(ctx context.Context) ([]string, error) {
	var count int
	err := s.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		return conn.QueryRowContext(ctx, pendingPeriodCountSQL).Scan(&count)
	})
	if err != nil {
		return nil, err
	}
	for i := 0; i < count; i++ {
		payload, err := createPackageFn(s, ctx)
		if err != nil {
			return nil, err
		}
		if payload == nil {
			break
		}
	}
	return s.exportAndPrune(ctx)
}

// Reports whether an error is the sqlite3 OperationalError class
func isSQLiteOperational(err error) bool {
	class, ok := state.SQLiteClassOf(err)
	return ok && class == state.SQLiteOperationalError
}

// Reports whether an OperationalError names lock contention
func isContention(err error) bool {
	text := state.ErrText(err)
	return strings.Contains(text, "locked") || strings.Contains(text, "busy")
}

// Runs write after deferred ones in one transaction, keeping them in memory when another writer holds the store
func (s *SharedMetricsStore) writeOrDefer(ctx context.Context, write storeWrite) error {
	s.deferredMu.Lock()
	pending := append(append([]storeWrite(nil), s.deferred...), write)
	s.deferred = nil
	s.deferredMu.Unlock()
	err := s.write(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		for _, item := range pending {
			if err := item(ctx, conn); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil || !isSQLiteOperational(err) {
		return err
	}
	busy := isContention(err)
	s.deferredMu.Lock()
	kept := pending
	if !busy {
		kept = pending[:len(pending)-1]
	}
	s.deferred = lastWrites(append(append([]storeWrite(nil), kept...), s.deferred...))
	register := !s.drainAtExit
	s.drainAtExit = true
	s.deferredMu.Unlock()
	if register {
		deferringStoresMu.Lock()
		deferringStores = append(deferringStores, s)
		deferringStoresMu.Unlock()
	}
	if !busy {
		return err
	}
	return nil
}

// Newest deferred writes within the in memory bound
func lastWrites(writes []storeWrite) []storeWrite {
	if len(writes) > deferredWritesMax {
		return writes[len(writes)-deferredWritesMax:]
	}
	return writes
}

// Writes the deferred writes with a longer busy wait, keeping them when the store is still contended
func (s *SharedMetricsStore) DrainDeferred(ctx context.Context, busyTimeout int) {
	s.deferredMu.Lock()
	pending := s.deferred
	s.deferred = nil
	s.deferredMu.Unlock()
	if len(pending) == 0 {
		return
	}
	err := s.write(ctx, busyTimeout, func(conn state.SQLiteConn) error {
		for _, item := range pending {
			if err := item(ctx, conn); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return
	}
	if isSQLiteOperational(err) && isContention(err) {
		s.deferredMu.Lock()
		s.deferred = lastWrites(append(append([]storeWrite(nil), pending...), s.deferred...))
		s.deferredMu.Unlock()
		return
	}
	slog.Warn(fmt.Sprintf("Unable to write %d deferred shared-metrics updates", len(pending)), "error", err)
}

// Creates pending packages at most once per UTC day, then exports them
func (s *SharedMetricsStore) CreateAndExportPackageIfDue(ctx context.Context) ([]string, error) {
	s.DrainDeferred(ctx, busyTimeoutMS)
	if err := s.createPendingPackagesIfDue(ctx); err != nil {
		return nil, err
	}
	return s.exportAndPrune(ctx)
}

// Exports pending packages, a failing prune logged without failing the export
func (s *SharedMetricsStore) exportAndPrune(ctx context.Context) ([]string, error) {
	exported, err := s.exportPendingPackages(ctx)
	if err != nil {
		return nil, err
	}
	if err := pruneExpiredHistoryFn(s, ctx, time.Time{}); err != nil {
		slog.Warn("Unable to prune expired shared-metrics history", "error", err)
	}
	return exported, nil
}

// Cumulative counters for tests and local inspection
func (s *SharedMetricsStore) CounterSnapshot(ctx context.Context) ([]CounterRow, error) {
	var out []CounterRow
	err := s.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, `SELECT period_start, metric_name, icebath_version, os_family, architecture,
			install_method, dimensions_json, value, packaged_value
			FROM counter_aggregates
			ORDER BY period_start, icebath_version, os_family, architecture, install_method,
				metric_name, dimensions_json`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r CounterRow
			var dimsJSON string
			resourceValues := make([]string, len(resourceColumns))
			if err := rows.Scan(&r.PeriodStart, &r.MetricName, &resourceValues[0], &resourceValues[1], &resourceValues[2], &resourceValues[3],
				&dimsJSON, &r.Value, &r.PackagedValue); err != nil {
				return err
			}
			r.Resource = rowResource(resourceValues)
			decoded, err := pyjson.Loads(dimsJSON)
			if err != nil {
				return err
			}
			r.Dimensions = pyjson.Plain(decoded)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// Runs body on a fresh connection with the busy timeout, autocommitting each statement
func (s *SharedMetricsStore) connection(ctx context.Context, busyTimeout int, body func(conn state.SQLiteConn) error) error {
	conn, err := state.ConnectSQLite(s.DatabasePath, state.SQLiteConfig{Timeout: float64(busyTimeout) / 1000})
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeout)); err != nil {
		return err
	}
	return body(conn)
}

// Runs body on a fresh connection inside one IMMEDIATE write transaction
func (s *SharedMetricsStore) write(ctx context.Context, busyTimeout int, body func(conn state.SQLiteConn) error) error {
	return s.connection(ctx, busyTimeout, func(conn state.SQLiteConn) error {
		return state.WriteTxn(ctx, conn, func() error { return body(conn) })
	})
}

// Creates or upgrades the schema, serialized across processes
func (s *SharedMetricsStore) ensureSchema(ctx context.Context) error {
	return s.write(ctx, schemaBusyTimeoutMS, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, createTelemetryStateSQL); err != nil {
			return err
		}
		var version sql.NullString
		err := conn.QueryRowContext(ctx, "SELECT value FROM telemetry_state WHERE key = 'schema_version'").Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		schemaVersion, known := version.String, err == nil
		if known && schemaVersion == "1" {
			for _, statement := range migrateV1SQL {
				if _, err := conn.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			schemaVersion = storeSchemaVersion
		}
		if known && schemaVersion != storeSchemaVersion {
			return fmt.Errorf("Unsupported shared-metrics store schema version: %s", schemaVersion)
		}
		for _, statement := range []string{createCounterAggregatesSQL, createPackageOutboxSQL} {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		existing := map[string]bool{}
		rows, err := conn.QueryContext(ctx, "SELECT name FROM pragma_table_info('package_outbox')")
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			existing[name] = true
		}
		rows.Close()
		for _, column := range sendColumns {
			if !existing[column[0]] {
				if _, err := state.AddColumnIfMissing(ctx, conn, "package_outbox", column[0], column[0]+" "+column[1]); err != nil {
					return err
				}
			}
		}
		for _, statement := range createConsentTablesSQL {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, "INSERT INTO telemetry_state(key, value) VALUES ('schema_version', ?)"+
			" ON CONFLICT(key) DO UPDATE SET value = excluded.value", storeSchemaVersion)
		return err
	})
}

// Install identity, created on first use
func installID(ctx context.Context, conn state.SQLiteConn) (string, error) {
	const query = "SELECT value FROM telemetry_state WHERE key = 'install_id'"
	var value string
	err := conn.QueryRowContext(ctx, query).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO telemetry_state(key, value) VALUES ('install_id', ?)",
			newUUID()); err != nil {
			return "", err
		}
		err = conn.QueryRowContext(ctx, query).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("Unable to create the shared-metrics install identity")
		}
	}
	return value, err
}

// Aware timestamp of a state value in UTC, nil when unparseable or naive
func parseStateTimestamp(value string) *time.Time {
	parsed, aware, err := pydatetime.ParseISOFormat(value)
	if err != nil || !aware {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

// Packages every pending period unless a package was already committed today
func (s *SharedMetricsStore) createPendingPackagesIfDue(ctx context.Context) error {
	now := utcNow()
	return s.write(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		var one int
		err := conn.QueryRowContext(ctx, "SELECT 1 FROM package_outbox WHERE substr(created_at, 1, 10) >= ? LIMIT 1",
			utcDate(now)).Scan(&one)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for {
			payload, err := s.createPackageInTransaction(ctx, conn, now)
			if err != nil || payload == nil {
				return err
			}
		}
	})
}

// Packages the oldest pending period in its own transaction
func (s *SharedMetricsStore) createPackage(ctx context.Context) (map[string]any, error) {
	var payload map[string]any
	err := s.write(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		var err error
		payload, err = s.createPackageInTransaction(ctx, conn, utcNow())
		return err
	})
	return payload, err
}

// packagedRow is one pending counter row of the period being packaged
type packagedRow struct {
	metricName, dimensionsJSON string
	value, packagedValue       int64
}

// Commits the oldest pending period and resource as one outbox package, nil when nothing is pending
func (s *SharedMetricsStore) createPackageInTransaction(ctx context.Context, conn state.SQLiteConn, now time.Time) (map[string]any, error) {
	var periodValue sql.NullString
	resourceValues := make([]string, 4)
	err := conn.QueryRowContext(ctx, `SELECT period_start, icebath_version, os_family, architecture, install_method
		FROM counter_aggregates WHERE value > packaged_value
		ORDER BY period_start, icebath_version, os_family, architecture, install_method
		LIMIT 1`).Scan(&periodValue, &resourceValues[0], &resourceValues[1], &resourceValues[2], &resourceValues[3])
	if errors.Is(err, sql.ErrNoRows) || (err == nil && periodValue.String == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resource := rowResource(resourceValues)
	args := []any{periodValue.String}
	for _, v := range resourceValues {
		args = append(args, v)
	}
	rows, err := conn.QueryContext(ctx, `SELECT metric_name, dimensions_json, value, packaged_value FROM counter_aggregates
		WHERE period_start = ? AND icebath_version = ? AND os_family = ?
		  AND architecture = ? AND install_method = ? AND value > packaged_value
		ORDER BY metric_name, dimensions_json`, args...)
	if err != nil {
		return nil, err
	}
	var pending []packagedRow
	for rows.Next() {
		var r packagedRow
		if err := rows.Scan(&r.metricName, &r.dimensionsJSON, &r.value, &r.packagedValue); err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	periodStart, err := pydatetime.FromISOFormat(periodValue.String)
	if err != nil {
		return nil, err
	}
	periodStart = time.Date(periodStart.Year(), periodStart.Month(), periodStart.Day(), periodStart.Hour(),
		periodStart.Minute(), periodStart.Second(), periodStart.Nanosecond(), time.UTC)
	if !ClientResourceIsValid(anyDims(resource)) {
		return nil, &UnsupportedError{"Unsupported shared-metrics client resource"}
	}
	metrics, err := packageMetrics(pending)
	if err != nil {
		return nil, err
	}
	if len(metrics) == 0 {
		if err := markPackaged(ctx, conn, pending, periodValue.String, resourceValues); err != nil {
			return nil, err
		}
		return s.createPackageInTransaction(ctx, conn, now)
	}
	id, err := installID(ctx, conn)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"schema_version": packageSchemaVersion,
		"package_id":     newUUID(),
		"install_id":     id,
		"period_start":   isoformat(periodStart),
		"period_end":     isoformat(periodStart.Add(24 * time.Hour)),
		"generated_at":   isoformat(now),
		"resource":       anyDims(resource),
		"metrics":        metrics,
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO package_outbox( package_id, period_start, period_end, payload_json,"+
		" created_at ) VALUES (?, ?, ?, ?, ?)", payload["package_id"], payload["period_start"], payload["period_end"],
		compactJSON(payload), payload["generated_at"]); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO consent_marks(name, stamp) VALUES ('data', ?)"+
		" ON CONFLICT(name) DO UPDATE SET stamp = MAX(stamp, excluded.stamp)", payload["period_end"]); err != nil {
		return nil, err
	}
	if err := markPackaged(ctx, conn, pending, periodValue.String, resourceValues); err != nil {
		return nil, err
	}
	return payload, nil
}

// Advances packaged_value to value for every packaged row
func markPackaged(ctx context.Context, conn state.SQLiteConn, rows []packagedRow, periodValue string, resourceValues []string) error {
	for _, row := range rows {
		if _, err := conn.ExecContext(ctx, `UPDATE counter_aggregates SET packaged_value = value
			WHERE period_start = ? AND metric_name = ? AND icebath_version = ?
			  AND os_family = ? AND architecture = ? AND install_method = ?
			  AND dimensions_json = ?`, periodValue, row.metricName, resourceValues[0], resourceValues[1],
			resourceValues[2], resourceValues[3], row.dimensionsJSON); err != nil {
			return err
		}
	}
	return nil
}

// Package metrics, merging rows a rebucket folded onto the same dimensions
func packageMetrics(rows []packagedRow) ([]any, error) {
	type key struct{ name, dims string }
	merged := map[key]map[string]any{}
	var order []key
	for _, row := range rows {
		metric, err := packageMetric(row)
		if err != nil {
			return nil, err
		}
		if metric == nil {
			continue
		}
		k := key{metric["name"].(string), compactJSON(metric["dimensions"])}
		if existing, ok := merged[k]; ok {
			existing["value"] = existing["value"].(int64) + metric["value"].(int64)
			continue
		}
		merged[k] = metric
		order = append(order, k)
	}
	out := make([]any, 0, len(order))
	for _, k := range order {
		out = append(out, merged[k])
	}
	return out, nil
}

// One package metric, unknown shapes failing closed and drifted values rebucketed or left out
func packageMetric(row packagedRow) (map[string]any, error) {
	decoded, err := pyjson.Loads(row.dimensionsJSON)
	if err != nil {
		return nil, err
	}
	dimensions, ok := pyjson.Plain(decoded).(map[string]any)
	if !ok || !CounterDimensionsShapeIsKnown(row.metricName, dimensions) {
		return nil, &UnsupportedError{"Unsupported dimensions for shared metric: " + row.metricName}
	}
	if !CounterDimensionsAreValid(row.metricName, dimensions) {
		conformed, err := ConformCounterDimensions(row.metricName, dimensions)
		if err != nil {
			return nil, err
		}
		if conformed == nil {
			slog.Info(fmt.Sprintf("Shared-metric %s row holds a retired value with no bucket; not packaged", row.metricName))
			return nil, nil
		}
		slog.Info(fmt.Sprintf("Shared-metric %s row drifted out of the contract since it was recorded; "+
			"packaged with bucketed dimensions %s", row.metricName, compactJSON(conformed)))
		dimensions = conformed
	}
	return map[string]any{"name": row.metricName, "type": "counter", "dimensions": dimensions,
		"value": row.value - row.packagedValue}, nil
}

// Writes every unexported committed package to the outbox and stamps it exported
func (s *SharedMetricsStore) exportPendingPackages(ctx context.Context) ([]string, error) {
	type outboxRow struct{ id, payload string }
	var pending []outboxRow
	err := s.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT package_id, payload_json FROM package_outbox"+
			" WHERE exported_at IS NULL ORDER BY created_at, package_id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r outboxRow
			if err := rows.Scan(&r.id, &r.payload); err != nil {
				return err
			}
			pending = append(pending, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	exported := []string{}
	for _, row := range pending {
		path := filepath.Join(s.OutboxDirectory, row.id+".json")
		decoded, err := pyjson.Loads(row.payload)
		if err != nil {
			return nil, err
		}
		if err := writePackageFile(ctx, path, decoded); err != nil {
			return nil, err
		}
		err = s.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(ctx, "UPDATE package_outbox SET exported_at = ?"+
				" WHERE package_id = ? AND exported_at IS NULL", isoformat(utcNow()), row.id)
			return err
		})
		if err != nil {
			return nil, err
		}
		exported = append(exported, path)
	}
	return exported, nil
}

// Atomic owner only compact sorted JSON write of one package file
var writePackageFile = func(ctx context.Context, path string, payload any) error {
	text, err := pyjson.Dumps(payload, pyjson.SortKeys(true), pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return pyfs.AtomicWriteTextMode(ctx, path, text, "."+stem+"_", 0o600)
}

// Removes exported local history past the retention window, a zero now meaning the current time
func (s *SharedMetricsStore) pruneExpiredHistory(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = utcNow()
	}
	cutoff := now.Add(-localHistoryRetentionDays * 24 * time.Hour)
	cutoffTimestamp := isoformat(cutoff)
	var ids []string
	err := s.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT package_id FROM package_outbox"+
			" WHERE exported_at IS NOT NULL AND exported_at < ?"+
			" ORDER BY exported_at, package_id", cutoffTimestamp)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	var removable []string
	for _, id := range ids {
		if err := os.Remove(filepath.Join(s.OutboxDirectory, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn(fmt.Sprintf("Unable to prune expired shared-metrics package %s", id), "error", err)
			continue
		}
		removable = append(removable, id)
	}
	return s.write(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "UPDATE package_outbox SET payload_json = ''"+
			" WHERE exported_at IS NOT NULL AND send_state IN ('sent', 'rejected')"+
			" AND payload_json != ''"); err != nil {
			return err
		}
		for _, id := range removable {
			if _, err := conn.ExecContext(ctx, "DELETE FROM package_outbox"+
				" WHERE package_id = ? AND exported_at IS NOT NULL AND exported_at < ?", id, cutoffTimestamp); err != nil {
				return err
			}
		}
		_, err := conn.ExecContext(ctx, `DELETE FROM counter_aggregates
			WHERE period_start < ? AND value = packaged_value AND NOT EXISTS (
				SELECT 1 FROM package_outbox WHERE exported_at IS NULL
				  AND substr(package_outbox.period_start, 1, 10)
				      = counter_aggregates.period_start
			)`, utcDate(cutoff))
		return err
	})
}

// Package and prune steps the export path calls, replaced by tests that interleave or fail them
var (
	createPackageFn       = (*SharedMetricsStore).createPackage
	pruneExpiredHistoryFn = (*SharedMetricsStore).pruneExpiredHistory
)
