package cron

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// IncidentStates lists the lifecycle states, closed being the only terminal one
var IncidentStates = []string{"detected", "alerted", "resolved", "closed"}

var failureTypeOrder = []struct {
	kind     string
	patterns []string
}{
	{"rate_limit", []string{`\b429\b`, "rate limit", "usage limit", "quota"}},
	{"timeout", []string{"timeout", "timed out"}},
	{"auth", []string{`\b401\b`, "unauthorized", "authentication", "auth"}},
	{"delivery", []string{"delivery", "deliver", "delivering"}},
	{"config", []string{"config", "configuration", "validation"}},
	{"script", []string{"script", "no_agent"}},
	{"agent", []string{"agent", "model", "provider", "inference"}},
}

const (
	maxIncidentErrorChars   = 500
	maxSignatureErrorChars  = 200
	incidentDurationPattern = `\b\d+(?:\.\d+)?\s*(?:ms|s|secs?|seconds?|m|mins?|minutes?|h|hrs?|hours?)\b`
)

var (
	incidentDurationRe   = mustPyRe(incidentDurationPattern)
	incidentWhitespaceRe = mustPyRe(`\s+`)
	incidentsMu          sync.Mutex
	// Incident database a test points elsewhere, after the store executions override
	incidentsExecutionsFile string
)

// Incident is one cron_incidents row
type Incident struct {
	ID          string
	JobID       string
	ErrorSig    string
	State       string
	FailureType string
	FirstSeenAt string
	LastSeenAt  string
	AckedAt     *string
	AlertedAt   *string
	ClosedAt    *string
	Error       string
	OutputFile  *string
}

// IncidentStore is the per home incident table living in the executions database
type IncidentStore struct {
	s    *Store
	path string
}

// Incident table of the store, sharing the executions ledger override so both stay in one database
func openIncidents(s *Store) *IncidentStore {
	path := s.executionsFile
	if path == "" {
		path = incidentsExecutionsFile
	}
	if path == "" {
		path = filepath.Join(s.home, "cron", "executions.db")
	}
	return &IncidentStore{s: s, path: path}
}

// Creates the incidents table, the late alerted_at column and its indexes
func initializeIncidentsSchema(ctx context.Context, conn state.SQLiteConn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cron_incidents (
             id            TEXT PRIMARY KEY,
             job_id        TEXT NOT NULL,
             error_sig     TEXT NOT NULL,
             state         TEXT NOT NULL,
             failure_type  TEXT NOT NULL DEFAULT 'unknown',
             first_seen_at TEXT NOT NULL,
             last_seen_at  TEXT NOT NULL,
             acked_at      TEXT,
             alerted_at    TEXT,
             closed_at     TEXT,
             error         TEXT NOT NULL,
             output_file   TEXT
           )`); err != nil {
		return err
	}
	if _, err := state.AddColumnIfMissing(ctx, conn, "cron_incidents", "alerted_at", "alerted_at TEXT"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_cron_incidents_job ON cron_incidents(job_id)"); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_cron_incidents_state ON cron_incidents(state)")
	return err
}

// Runs body in one transaction on the incident database under the incident lock
func (i *IncidentStore) transaction(body func(ctx context.Context, conn state.SQLiteConn) error) error {
	ctx := context.Background()
	incidentsMu.Lock()
	defer incidentsMu.Unlock()
	if err := ensureCronDir(filepath.Dir(i.path)); err != nil {
		return err
	}
	conn, err := state.OpenDB(ctx, i.path, "cron/executions.db", state.WithSynchronousFull(), state.WithInitialize(initializeIncidentsSchema))
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, false, func(conn state.SQLiteConn) error { return body(ctx, conn) })
}

// Collapses whitespace runs, strips and lowercases an error for signing
func normalizeIncidentError(errText string) string {
	collapsed, err := incidentWhitespaceRe.Replace(errText, " ", -1, -1)
	if err != nil {
		collapsed = errText
	}
	return pystr.Lower(pystr.Strip(collapsed))
}

// First n code points of text
func headRunes(text string, n int) string {
	count := 0
	for idx := range text {
		if count == n {
			return text[:idx]
		}
		count++
	}
	return text
}

// Error text scrubbed of secrets and bounded to the stored length
func (i *IncidentStore) redactIncidentError(errText string) string {
	func() {
		defer func() { _ = recover() }()
		errText = tools.RedactSensitiveTextWith(&tools.Env{Home: i.s.home}, errText, tools.RedactOptions{Force: true})
	}()
	return headRunes(errText, maxIncidentErrorChars)
}

// Dedup key stable for the same job and normalized error prefix with measured durations masked
func incidentErrorSignature(jobID, errText string) string {
	masked, err := incidentDurationRe.Replace(normalizeIncidentError(errText), "#s", -1, -1)
	if err != nil {
		masked = normalizeIncidentError(errText)
	}
	sum := sha256.Sum256([]byte(jobID + headRunes(masked, maxSignatureErrorChars)))
	return hex.EncodeToString(sum[:])[:12]
}

// Incident id of a job and signature
func incidentID(jobID, sig string) string { return headRunes(jobID, 6) + "_" + sig }

// Failure kind from error keywords, unknown by default
func classifyFailureType(errText string) string {
	text := normalizeIncidentError(errText)
	if text == "" {
		return "unknown"
	}
	for _, entry := range failureTypeOrder {
		for _, pattern := range entry.patterns {
			if strings.HasPrefix(pattern, `\b`) && strings.HasSuffix(pattern, `\b`) {
				if ok, _ := mustPyRe(pattern).MatchString(text); ok {
					return entry.kind
				}
			} else if strings.Contains(text, pattern) {
				return entry.kind
			}
		}
	}
	return "unknown"
}

// Records or refreshes the incident of a job and error, reopening a resolved one
func (i *IncidentStore) UpsertIncident(jobID, errText string, jobName, outputFile *string) (string, bool, error) {
	return i.UpsertIncidentTyped(jobID, errText, jobName, nil, outputFile)
}

// Upsert with an explicit failure type, classified from the error text when nil or empty
func (i *IncidentStore) UpsertIncidentTyped(jobID, errText string, jobName, failureType, outputFile *string) (string, bool, error) {
	sig := incidentErrorSignature(jobID, errText)
	stored := i.redactIncidentError(errText)
	id := incidentID(jobID, sig)
	now := FormatISO(i.s.clk.now())
	kind := classifyFailureType(errText)
	if failureType != nil && *failureType != "" {
		kind = *failureType
	}
	isNew := false
	err := i.transaction(func(ctx context.Context, conn state.SQLiteConn) error {
		row, err := queryRecord(ctx, conn, "SELECT id, state FROM cron_incidents WHERE id=?", id)
		if err != nil {
			return err
		}
		if row != nil {
			isNew = row["state"] == "resolved"
			_, err := conn.ExecContext(ctx, `UPDATE cron_incidents
                   SET last_seen_at=?, error=?, output_file=?,
                       state=CASE WHEN state='resolved' THEN 'detected' ELSE state END,
                       alerted_at=CASE WHEN state='resolved' THEN NULL ELSE alerted_at END,
                       closed_at=CASE WHEN state='resolved' THEN NULL ELSE closed_at END
                   WHERE id=?`, now, stored, nullable(outputFile), id)
			return err
		}
		isNew = true
		_, err = conn.ExecContext(ctx, `INSERT INTO cron_incidents
               (id, job_id, error_sig, state, failure_type,
                first_seen_at, last_seen_at, error, output_file)
               VALUES (?, ?, ?, 'detected', ?, ?, ?, ?, ?)`, id, jobID, sig, kind, now, now, stored, nullable(outputFile))
		return err
	})
	if err != nil {
		return "", false, err
	}
	return id, isNew, nil
}

// True for a known lifecycle state
func validIncidentState(st string) bool {
	for _, known := range IncidentStates {
		if known == st {
			return true
		}
	}
	return false
}

// Moves an incident to a state, alerted always restamping alerted_at, closed being terminal
func (i *IncidentStore) SetIncidentState(id, st string) (bool, error) {
	if !validIncidentState(st) {
		return false, nil
	}
	now := FormatISO(i.s.clk.now())
	changed := false
	err := i.transaction(func(ctx context.Context, conn state.SQLiteConn) error {
		row, err := queryRecord(ctx, conn, "SELECT state FROM cron_incidents WHERE id=?", id)
		if err != nil || row == nil || row["state"] == "closed" {
			return err
		}
		switch {
		case st == "alerted":
			_, err = conn.ExecContext(ctx, "UPDATE cron_incidents SET state='alerted', alerted_at=? WHERE id=?", now, id)
		case row["state"] == st:
			return nil
		case st == "closed":
			_, err = conn.ExecContext(ctx, `UPDATE cron_incidents
                   SET state='closed', closed_at=?, acked_at=?
                   WHERE id=? AND state != 'closed'`, now, now, id)
		default:
			_, err = conn.ExecContext(ctx, "UPDATE cron_incidents SET state=? WHERE id=?", st, id)
		}
		changed = err == nil
		return err
	})
	return changed, err
}

// Closes an incident on operator acknowledgement, false when missing or already closed
func (i *IncidentStore) AckIncident(id string) (bool, error) { return i.SetIncidentState(id, "closed") }

// Resolves every open incident of a job after a clean run and returns how many changed
func (i *IncidentStore) CloseIncidentsForRecoveredJob(jobID string) (int, error) {
	now := FormatISO(i.s.clk.now())
	var n int64
	err := i.transaction(func(ctx context.Context, conn state.SQLiteConn) error {
		var err error
		n, err = rowsChanged(conn.ExecContext(ctx, `UPDATE cron_incidents SET state='resolved', closed_at=?
               WHERE job_id=? AND state IN ('detected', 'alerted')`, now, jobID))
		return err
	})
	return int(n), err
}

// Optional state filter clause and its arguments
func incidentStateFilter(st *string) (string, []any) {
	if st == nil {
		return "", nil
	}
	return " WHERE state=?", []any{*st}
}

// Incident of a scanned record
func incidentFromRecord(rec map[string]any) *Incident {
	opt := func(key string) *string {
		if v, ok := rec[key].(string); ok {
			return &v
		}
		return nil
	}
	return &Incident{ID: recText(rec, "id"), JobID: recText(rec, "job_id"), ErrorSig: recText(rec, "error_sig"),
		State: recText(rec, "state"), FailureType: recText(rec, "failure_type"), FirstSeenAt: recText(rec, "first_seen_at"),
		LastSeenAt: recText(rec, "last_seen_at"), AckedAt: opt("acked_at"), AlertedAt: opt("alerted_at"),
		ClosedAt: opt("closed_at"), Error: recText(rec, "error"), OutputFile: opt("output_file")}
}

// Incidents newest activity instant first, optionally filtered by state
func (i *IncidentStore) ListIncidents(st *string) ([]*Incident, error) {
	if st != nil && !validIncidentState(*st) {
		return []*Incident{}, nil
	}
	where, args := incidentStateFilter(st)
	out := []*Incident{}
	err := i.transaction(func(ctx context.Context, conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT * FROM cron_incidents"+where+" ORDER BY julianday(last_seen_at) DESC, last_seen_at DESC, id DESC", args...)
		if err != nil {
			return err
		}
		recs, err := scanRecords(rows)
		for _, rec := range recs {
			out = append(out, incidentFromRecord(rec))
		}
		return err
	})
	return out, err
}

// One incident by id, nil when absent
func (i *IncidentStore) GetIncident(id string) (*Incident, error) {
	var inc *Incident
	err := i.transaction(func(ctx context.Context, conn state.SQLiteConn) error {
		rec, err := queryRecord(ctx, conn, "SELECT * FROM cron_incidents WHERE id=?", id)
		if rec != nil {
			inc = incidentFromRecord(rec)
		}
		return err
	})
	return inc, err
}

// Number of incidents, optionally in one state, zero for an unknown state
func (i *IncidentStore) CountIncidents(st *string) (int, error) {
	if st != nil && !validIncidentState(*st) {
		return 0, nil
	}
	where, args := incidentStateFilter(st)
	var n sql.NullInt64
	err := i.transaction(func(ctx context.Context, conn state.SQLiteConn) error {
		return conn.QueryRowContext(ctx, "SELECT COUNT(*) AS n FROM cron_incidents"+where, args...).Scan(&n)
	})
	return int(n.Int64), err
}
