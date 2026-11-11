package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	LedgerMaxAttempts         = 3
	LedgerStaleAfterSeconds   = 24 * 60 * 60
	ledgerRetentionSeconds    = 7 * 24 * 60 * 60
	ledgerMaxRows             = 500
	FloodErrorPrefix          = "flood_control:"
	FloodRetryDefaultSeconds  = 60.0
	FloodRetryCapSeconds      = 15 * 60.0
	FloodRetrySlackSeconds    = 2.0
	ledgerDBLabel             = "state.db (delivery_ledger)"
	ledgerBusyTimeoutMS       = 10_000
	ledgerLastErrorCodePoints = 500
)

const (
	RecoveredMarker   = "♻️ Recovered reply — the gateway restarted during delivery, so this may be a duplicate:\n\n"
	ReconnectedMarker = "♻️ Recovered reply — the messaging platform reconnected after the original " +
		"delivery failed, so this may be a duplicate:\n\n"
	FloodMarker = "♻️ Recovered reply — the messaging platform's rate limit refused the original, so part of " +
		"it may already have arrived above:\n\n"
)

var (
	ledgerMu                = sync.Mutex{}
	runtimeRetryableErrors  = map[string]bool{"send_path_degraded": true}
	ledgerRetryBackoffSecs  = [LedgerMaxAttempts - 1]float64{30.0, 120.0}
	rawFloodRE              = mustPyre(`(?i)flood control exceeded.*?retry in\s+(\d+(?:\.\d+)?)`)
	ledgerNow               = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	ledgerDBPath            = func() string { return pypath.PathJoin(home.Process(), "state.db") }
	ledgerOwnerStamp        = defaultLedgerOwnerStamp
	ledgerDeadErrorClassify = ClassifyDeadError
)

// Python compiled regex for a module level pattern, panicking at init on an invalid pattern
func mustPyre(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Seconds a flood error in the platform's own wording asks for, false when absent or unreadable
func rawFloodWait(text string) (float64, bool) {
	m, err := rawFloodRE.FindStringMatch(text)
	if err != nil || m == nil {
		return 0, false
	}
	wait, err := pyint.ParseFloat(m.GroupByNumber(1).String())
	if err != nil {
		return 0, false
	}
	return wait, true
}

// Python str of a falsy error as empty, otherwise its text, stripped and lowercased
func ledgerErrorKey(errValue any) string {
	if !pyrepr.Truthy(errValue) {
		return ""
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(errValue)))
}

// True for a row only an adapter reconnect may retry with no timer and no backoff
func IsReconnectOnly(err any) bool { return runtimeRetryableErrors[ledgerErrorKey(err)] }

// True for a flood refusal in the canonical prefix form or the platform's own wording
func IsFloodError(err any) bool {
	text := ledgerErrorKey(err)
	if strings.HasPrefix(text, FloodErrorPrefix) {
		return true
	}
	_, ok := rawFloodWait(text)
	return ok
}

// Wait the platform asked for in a flood error, def when unreadable or not positive
func FloodWaitSeconds(err any, def float64) float64 {
	text := ledgerErrorKey(err)
	wait := def
	if strings.HasPrefix(text, FloodErrorPrefix) {
		if parsed, perr := pyint.ParseFloat(pystr.Strip(text[len(FloodErrorPrefix):])); perr == nil {
			wait = parsed
		}
	} else if raw, ok := rawFloodWait(text); ok {
		wait = raw
	}
	if wait > 0 {
		return wait
	}
	return def
}

// Redelivery timer sleep for a wait of seconds, capped at fifteen minutes plus two seconds of slack
func FloodRetryDelay(seconds any) float64 {
	wait, err := pyint.Float(seconds)
	if err != nil {
		wait = FloodRetryDefaultSeconds
	}
	return min(max(wait, 0.0), FloodRetryCapSeconds) + FloodRetrySlackSeconds
}

// Float of a failed row stamp, zero for a missing or junk value
func failedStamp(updatedAt any) float64 {
	if !pyrepr.Truthy(updatedAt) {
		return 0.0
	}
	f, err := pyint.Float(updatedAt)
	if err != nil {
		return 0.0
	}
	return f
}

// Earliest moment a flood refused row may be resent, its refusal stamp plus the platform's wait
func FloodNotBefore(updatedAt, lastError any) float64 {
	return failedStamp(updatedAt) + FloodWaitSeconds(lastError, FloodRetryDefaultSeconds)
}

// Earliest resend moment of a failed row, false for a dead chat or one holding the last budgeted attempt
func RetryNotBefore(updatedAt, lastError any, attempts int) (float64, bool) {
	if IsFloodError(lastError) {
		return FloodNotBefore(updatedAt, lastError), true
	}
	text := ledgerErrorKey(lastError)
	if IsReconnectOnly(text) {
		return failedStamp(updatedAt), true
	}
	if ledgerDeadErrorClassify(text) != "" {
		return 0, false
	}
	if attempts >= LedgerMaxAttempts-1 {
		return 0, false
	}
	return failedStamp(updatedAt) + ledgerRetryBackoffSecs[attempts], true
}

// Shared state.db connection with the ledger schema, ten second busy timeout and tuple rows
func ledgerConnect(ctx context.Context) (state.SQLiteConn, error) {
	return state.OpenDB(ctx, ledgerDBPath(), ledgerDBLabel, state.WithBusyTimeoutMS(ledgerBusyTimeoutMS),
		state.WithInitialize(initializeLedgerSchema))
}

// Creates the delivery obligations table on a connection the session recovery copies into
func InitializeLedgerSchema(ctx context.Context, conn state.SQLiteConn) error {
	return initializeLedgerSchema(ctx, conn)
}

// Creates the obligations table and adds adapter_profile to a ledger written before it existed
func initializeLedgerSchema(ctx context.Context, conn state.SQLiteConn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS delivery_obligations (
            obligation_id TEXT PRIMARY KEY,
            session_key TEXT NOT NULL,
            platform TEXT NOT NULL,
            chat_id TEXT NOT NULL,
            thread_id TEXT,
            content TEXT NOT NULL,
            state TEXT NOT NULL,
            attempts INTEGER NOT NULL DEFAULT 0,
            created_at REAL NOT NULL,
            updated_at REAL NOT NULL,
            owner_pid INTEGER,
            owner_started_at INTEGER,
            last_error TEXT,
            adapter_profile TEXT
        )`); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA table_info(delivery_obligations)")
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		has = has || name == "adapter_profile"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !has {
		_, err = state.AddColumnIfMissing(ctx, conn, "delivery_obligations", "adapter_profile", "adapter_profile TEXT")
	}
	return err
}

// Runs body under the ledger lock in one transaction on a fresh connection that is always closed
func ledgerTransaction(ctx context.Context, body func(conn state.SQLiteConn) error) error {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	conn, err := ledgerConnect(ctx)
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, false, body)
}

// Start time fingerprint of pid as stored in owner_started_at, nil when unreadable
func ledgerStartTime(pid int64) (started *int64) {
	defer func() {
		if recover() != nil {
			started = nil
		}
	}()
	switch v := statusProcessStartTime(int(pid)).(type) {
	case int64:
		return &v
	case int:
		n := int64(v)
		return &n
	}
	return nil
}

// This process's pid and start time fingerprint
func defaultLedgerOwnerStamp() (int64, *int64) {
	pid := int64(os.Getpid())
	return pid, ledgerStartTime(pid)
}

// Integer of a stored owner column, false for NULL, zero or a value int() rejects
func ledgerOwnerInt(v any) (int64, bool) {
	if !pyrepr.Truthy(v) {
		return 0, false
	}
	switch x := v.(type) {
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case string, []byte:
		n, err := pyint.Of(pyrepr.Str(x))
		if err != nil || !n.IsInt64() {
			return 0, false
		}
		return n.Int64(), true
	}
	return 0, false
}

// Whether the recorded owning process still runs, by pid and start time within drift tolerance
func ledgerOwnerAlive(pid, startedAt any) bool {
	n, ok := ledgerOwnerInt(pid)
	if !ok {
		return false
	}
	current := ledgerStartTime(n)
	if current == nil {
		return ledgerPidProbe(n)
	}
	if startedAt == nil {
		return true
	}
	match, err := StartTimeFingerprintsMatch(startedAt, *current, StartTimeDriftTolerance)
	if err != nil {
		return true
	}
	return match
}

// Cross platform pid existence probe, a panicking probe meaning dead
func ledgerPidProbe(pid int64) (alive bool) {
	defer func() {
		if recover() != nil {
			alive = false
		}
	}()
	return statusPidExists(int(pid))
}

// Stable 24 hex id over session key, inbound message ref and content
func ComputeObligationID(sessionKey, messageRef, content string) string {
	sum := sha256.Sum256(pystr.EncodeUTF8Replace(sessionKey + "|" + messageRef + "|" + content))
	return hex.EncodeToString(sum[:])[:24]
}

type DeliveryObligation struct {
	ObligationID   string
	SessionKey     string
	Platform       string
	ChatID         string
	ThreadID       *string
	Content        string
	AdapterProfile *string
}

// Stored thread id, NULL for a missing or empty one
func (o DeliveryObligation) threadArg() any {
	if o.ThreadID == nil || *o.ThreadID == "" {
		return nil
	}
	return *o.ThreadID
}

// Stored adapter profile, stripped, default for a missing or empty one
func (o DeliveryObligation) profileArg() string {
	if o.AdapterProfile == nil || *o.AdapterProfile == "" {
		return "default"
	}
	return pystr.Strip(*o.AdapterProfile)
}

// Nullable SQL argument of an optional start time
func int64PtrArg(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// Records a final response as owed to the platform in state pending and prunes in the same transaction
func RecordObligation(ctx context.Context, o DeliveryObligation) error {
	now := ledgerNow()
	pid, started := ledgerOwnerStamp()
	return ledgerTransaction(ctx, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO delivery_obligations
               (obligation_id, session_key, platform, chat_id, thread_id,
                content, state, attempts, created_at, updated_at,
                owner_pid, owner_started_at, adapter_profile)
               VALUES (?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?, ?, ?)`,
			o.ObligationID, o.SessionKey, o.Platform, o.ChatID, o.threadArg(), o.Content, now, now, pid,
			int64PtrArg(started), o.profileArg()); err != nil {
			return err
		}
		return pruneLedgerUnlocked(ctx, conn, now)
	})
}

// Adopts an unowned attempting reply a killed process persisted, unless the same reply was ledgered since the turn start
func RecordCrashLeftReply(ctx context.Context, o DeliveryObligation, since float64) error {
	now := ledgerNow()
	return ledgerTransaction(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO delivery_obligations
               (obligation_id, session_key, platform, chat_id, thread_id,
                content, state, attempts, created_at, updated_at,
                owner_pid, owner_started_at, adapter_profile)
               SELECT ?, ?, ?, ?, ?, ?, 'attempting', 0, ?, ?, NULL, NULL, ?
               WHERE NOT EXISTS (SELECT 1 FROM delivery_obligations
                                 WHERE session_key = ? AND content = ? AND created_at >= ?)`,
			o.ObligationID, o.SessionKey, o.Platform, o.ChatID, o.threadArg(), o.Content, now, now, o.profileArg(),
			o.SessionKey, o.Content, since)
		return err
	})
}

// Moves the obligation to attempting right before the send await
func MarkAttempting(ctx context.Context, obligationID string) error {
	return updateLedgerState(ctx, obligationID, "attempting", "")
}

// Moves the obligation to delivered after a successful send
func MarkDelivered(ctx context.Context, obligationID string) error {
	return updateLedgerState(ctx, obligationID, "delivered", "")
}

// Moves the obligation to failed with the rejection text
func MarkFailed(ctx context.Context, obligationID, errText string) error {
	return updateLedgerState(ctx, obligationID, "failed", errText)
}

// Stored last_error, its first 500 code points, NULL when empty
func ledgerErrorArg(errText string) any {
	if errText == "" {
		return nil
	}
	return pystr.Slice(errText, 0, ledgerLastErrorCodePoints)
}

// Returns this process's unsent runtime claim to failed without spending an attempt
func ReleaseRuntimeClaim(ctx context.Context, obligationID, errText string) (bool, error) {
	pid, started := ledgerOwnerStamp()
	if started == nil {
		return false, nil
	}
	released := false
	err := ledgerTransaction(ctx, func(conn state.SQLiteConn) error {
		res, err := conn.ExecContext(ctx, `UPDATE delivery_obligations
               SET state='failed', attempts=CASE
                       WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
                   updated_at=?, last_error=?
               WHERE obligation_id=? AND state='attempting'
                 AND owner_pid IS ? AND owner_started_at IS ?`,
			ledgerNow(), ledgerErrorArg(errText), obligationID, pid, *started)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		released = n > 0
		return err
	})
	return released, err
}

// Sets state, stamp and last error of one obligation
func updateLedgerState(ctx context.Context, obligationID, st, errText string) error {
	return ledgerTransaction(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, `UPDATE delivery_obligations
               SET state=?, updated_at=?, last_error=?
               WHERE obligation_id=?`, st, ledgerNow(), ledgerErrorArg(errText), obligationID)
		return err
	})
}

// Row claimed or adopted for redelivery, Marker empty when the runner's restart marker applies
type ClaimedObligation struct {
	ObligationID    string
	SessionKey      string
	Platform        string
	ChatID          string
	ThreadID        *string
	Content         string
	NeedsMarker     bool
	Marker          string
	Profile         string
	RuntimeRecovery bool
	LastError       string
	Attempts        int
	Adopted         bool
	NotBefore       float64
}

// Claimed row naming its own cause, flood or runtime reconnect marker, attempts counting this claim
func claimedLedgerRow(r ledgerRow, profile string, needsMarker, runtime, flood bool, lastError string) ClaimedObligation {
	marker := ""
	if flood {
		marker = FloodMarker
	} else if runtime {
		marker = ReconnectedMarker
	}
	c := ClaimedObligation{ObligationID: r.oid, SessionKey: r.sessionKey, Platform: r.platform, ChatID: r.chatID,
		ThreadID: r.threadID, Content: r.content, NeedsMarker: needsMarker, Profile: profile, RuntimeRecovery: runtime,
		LastError: lastError, Attempts: r.attempts + 1}
	if needsMarker {
		c.Marker = marker
	}
	return c
}

// One delivery_obligations row as the sweeps read it
type ledgerRow struct {
	oid, sessionKey, platform, chatID, content, state string
	threadID                                          *string
	attempts                                          int
	createdAt, updatedAt                              float64
	ownerPid, ownerStartedAt                          any
	adapterProfile                                    *string
	lastError                                         string
}

// Platform and adapter identity a multiplexed boot can send on, NullProfile for a legacy row without one
type LedgerTarget struct {
	Platform    string
	Profile     string
	NullProfile bool
}

// Reads every row of query into memory so updates can run on the same connection
func readLedgerRows(ctx context.Context, conn state.SQLiteConn, query string, args ...any) ([]ledgerRow, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		var thread, profile, lastErr *string
		if err := rows.Scan(&r.oid, &r.sessionKey, &r.platform, &r.chatID, &thread, &r.content, &r.state,
			&r.attempts, &r.createdAt, &r.ownerPid, &r.ownerStartedAt, &profile, &lastErr, &r.updatedAt); err != nil {
			return nil, err
		}
		r.threadID, r.adapterProfile = thread, profile
		if lastErr != nil {
			r.lastError = *lastErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const ledgerRowColumns = `obligation_id, session_key, platform, chat_id, thread_id,
                      content, state, attempts, created_at,
                      owner_pid, owner_started_at, adapter_profile, last_error, updated_at
               FROM delivery_obligations`

// Profile of a row, default for a legacy row without one
func (r ledgerRow) profileOrDefault() string {
	if r.adapterProfile == nil || *r.adapterProfile == "" {
		return "default"
	}
	return *r.adapterProfile
}

// Optional float override of the sweep clock
func ledgerNowOr(now *float64) float64 {
	if now != nil {
		return *now
	}
	return ledgerNow()
}

// Claims undelivered rows of dead owners for redelivery, abandoning exhausted ones and adopting flood rows still waiting
func SweepRecoverable(ctx context.Context, now *float64, deliverablePlatforms map[string]bool, deliverableTargets map[LedgerTarget]bool) ([]ClaimedObligation, error) {
	at := ledgerNowOr(now)
	pid, started := ledgerOwnerStamp()
	claimed := []ClaimedObligation{}
	err := ledgerTransaction(ctx, func(conn state.SQLiteConn) error {
		rows, err := readLedgerRows(ctx, conn, "SELECT "+ledgerRowColumns+" WHERE state IN ('pending', 'attempting', 'failed')")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if ledgerOwnerAlive(r.ownerPid, r.ownerStartedAt) {
				continue
			}
			if r.attempts >= LedgerMaxAttempts || (at-r.createdAt) > LedgerStaleAfterSeconds {
				if _, err := conn.ExecContext(ctx, `UPDATE delivery_obligations
                       SET state='abandoned', updated_at=? WHERE obligation_id=?`, at, r.oid); err != nil {
					return err
				}
				continue
			}
			target := LedgerTarget{Platform: r.platform, NullProfile: r.adapterProfile == nil}
			if r.adapterProfile != nil {
				target.Profile = *r.adapterProfile
			}
			if (deliverablePlatforms != nil && !deliverablePlatforms[r.platform]) ||
				(deliverableTargets != nil && !deliverableTargets[target]) {
				continue
			}
			floodRow := r.state == "failed" && IsFloodError(r.lastError)
			if floodRow && at < FloodNotBefore(r.updatedAt, r.lastError) {
				res, err := conn.ExecContext(ctx, `UPDATE delivery_obligations
                       SET owner_pid=?, owner_started_at=?,
                           adapter_profile=COALESCE(adapter_profile, 'default')
                       WHERE obligation_id=? AND (owner_pid IS ? OR owner_pid=?)`,
					pid, int64PtrArg(started), r.oid, r.ownerPid, r.ownerPid)
				if err != nil {
					return err
				}
				if n, err := res.RowsAffected(); err != nil {
					return err
				} else if n > 0 {
					claimed = append(claimed, ClaimedObligation{ObligationID: r.oid, SessionKey: r.sessionKey,
						Platform: r.platform, ChatID: r.chatID, ThreadID: r.threadID, Content: r.content,
						Profile: r.profileOrDefault(), Attempts: r.attempts, Adopted: true,
						NotBefore: FloodNotBefore(r.updatedAt, r.lastError)})
				}
				continue
			}
			clearError := 0
			if floodRow {
				clearError = 1
			}
			res, err := conn.ExecContext(ctx, `UPDATE delivery_obligations
                   SET owner_pid=?, owner_started_at=?, attempts=attempts+1, updated_at=?,
                       adapter_profile=COALESCE(adapter_profile, 'default'), state='attempting',
                       last_error=CASE WHEN ? THEN NULL ELSE last_error END
                   WHERE obligation_id=? AND (owner_pid IS ? OR owner_pid=?)`,
				pid, int64PtrArg(started), at, clearError, r.oid, r.ownerPid, r.ownerPid)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n > 0 {
				claimed = append(claimed, claimedLedgerRow(r, r.profileOrDefault(),
					r.state != "pending" || r.attempts > 0, false, floodRow, ""))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// Whether a stored owner column holds exactly want
func ledgerOwnerIs(v any, want int64) bool {
	n, ok := v.(int64)
	return ok && n == want
}

// Claims this process's failed rows of one adapter that are past their retry deadline, every claim marked
func SweepFailedForRuntime(ctx context.Context, platform string, now *float64, profile *string) ([]ClaimedObligation, error) {
	at := ledgerNowOr(now)
	pid, started := ledgerOwnerStamp()
	if started == nil {
		return []ClaimedObligation{}, nil
	}
	expected := "default"
	if profile != nil && *profile != "" && *profile != "default" {
		expected = *profile
	}
	claimed := []ClaimedObligation{}
	err := ledgerTransaction(ctx, func(conn state.SQLiteConn) error {
		rows, err := readLedgerRows(ctx, conn, "SELECT "+ledgerRowColumns+" WHERE state='failed' AND platform=?", platform)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.adapterProfile == nil || *r.adapterProfile != expected || !ledgerOwnerIs(r.ownerPid, pid) ||
				!ledgerOwnerIs(r.ownerStartedAt, *started) {
				continue
			}
			due, ok := RetryNotBefore(r.updatedAt, r.lastError, r.attempts)
			if !ok {
				continue
			}
			if r.attempts >= LedgerMaxAttempts || (at-r.createdAt) > LedgerStaleAfterSeconds {
				if _, err := conn.ExecContext(ctx, `UPDATE delivery_obligations
                       SET state='abandoned', updated_at=?
                       WHERE obligation_id=? AND state='failed'
                         AND owner_pid IS ? AND owner_started_at IS ?`, at, r.oid, r.ownerPid, r.ownerStartedAt); err != nil {
					return err
				}
				continue
			}
			if at < due {
				continue
			}
			res, err := conn.ExecContext(ctx, `UPDATE delivery_obligations
                   SET state='attempting', attempts=attempts+1, updated_at=?, last_error=NULL
                   WHERE obligation_id=? AND state='failed'
                     AND owner_pid IS ? AND owner_started_at IS ?`, at, r.oid, r.ownerPid, r.ownerStartedAt)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n > 0 {
				claimed = append(claimed, claimedLedgerRow(r, *r.adapterProfile, true, true, IsFloodError(r.lastError),
					r.lastError))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// Earliest redelivery deadline of this process's waiting failed rows for one adapter identity
type PendingRetry struct {
	Platform  string
	Profile   string
	NotBefore float64
}

// This process's failed rows awaiting a timer, one entry per adapter identity with its earliest deadline
func PendingRetries(ctx context.Context, now *float64) ([]PendingRetry, error) {
	at := ledgerNowOr(now)
	pid, started := ledgerOwnerStamp()
	if started == nil {
		return []PendingRetry{}, nil
	}
	type row struct {
		platform             string
		profile              *string
		updatedAt, createdAt float64
		lastError            *string
		attempts             int
	}
	var rows []row
	err := ledgerTransaction(ctx, func(conn state.SQLiteConn) error {
		rs, err := conn.QueryContext(ctx, `SELECT platform, adapter_profile, updated_at, last_error, attempts, created_at
               FROM delivery_obligations
               WHERE state='failed' AND owner_pid IS ? AND owner_started_at IS ?`, pid, *started)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.platform, &r.profile, &r.updatedAt, &r.lastError, &r.attempts, &r.createdAt); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	})
	if err != nil {
		return nil, err
	}
	earliest := map[[2]string]float64{}
	for _, r := range rows {
		lastError := ""
		if r.lastError != nil {
			lastError = *r.lastError
		}
		if IsReconnectOnly(lastError) {
			continue
		}
		due, ok := RetryNotBefore(r.updatedAt, lastError, r.attempts)
		if !ok || r.attempts >= LedgerMaxAttempts || (at-r.createdAt) > LedgerStaleAfterSeconds {
			continue
		}
		profile := "default"
		if r.profile != nil && *r.profile != "" {
			profile = *r.profile
		}
		key := [2]string{r.platform, profile}
		if prev, seen := earliest[key]; !seen || due < prev {
			earliest[key] = due
		}
	}
	keys := make([][2]string, 0, len(earliest))
	for k := range earliest {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	out := make([]PendingRetry, 0, len(keys))
	for _, k := range keys {
		out = append(out, PendingRetry{Platform: k[0], Profile: k[1], NotBefore: earliest[k]})
	}
	return out, nil
}

// Retention deletes on the caller's open connection inside its transaction
func pruneLedgerUnlocked(ctx context.Context, conn state.SQLiteConn, now float64) error {
	if _, err := conn.ExecContext(ctx, `DELETE FROM delivery_obligations
           WHERE state IN ('delivered', 'abandoned') AND updated_at < ?`, now-ledgerRetentionSeconds); err != nil {
		return err
	}
	var total int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM delivery_obligations").Scan(&total); err != nil {
		return err
	}
	if total > ledgerMaxRows {
		_, err := conn.ExecContext(ctx, `DELETE FROM delivery_obligations WHERE obligation_id IN (
                 SELECT obligation_id FROM delivery_obligations
                 ORDER BY CASE state
                            WHEN 'delivered' THEN 0
                            WHEN 'abandoned' THEN 1
                            ELSE 2
                          END, updated_at ASC
                 LIMIT ?)`, total-ledgerMaxRows)
		return err
	}
	return nil
}

// Reads the gateway.delivery_ledger gate, on by default and on when config is unreadable
func LedgerEnabled(ctx context.Context, cfg map[string]any) bool {
	if cfg == nil {
		tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
		if err != nil {
			return true
		}
		cfg = tree
	}
	var value any = true
	if section := cfg["gateway"]; pyrepr.Truthy(section) {
		m, ok := section.(map[string]any)
		if !ok {
			return true
		}
		if v, present := m["delivery_ledger"]; present {
			value = v
		}
	}
	if s, ok := value.(string); ok {
		switch pystr.Lower(pystr.Strip(s)) {
		case "false", "0", "no", "off":
			return false
		}
		return true
	}
	return pyrepr.Truthy(value)
}
