package observability

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	RequestTimeoutSeconds  = 30
	MaxAttempts            = 3
	backoffBaseSeconds     = 1
	backoffFactor          = 5
	GzipThresholdBytes     = 4096
	MaxPackagesPerPass     = 20
	claimLeaseSeconds      = 300
	failureBackoffSeconds  = 15 * 60
	MaxSendAttempts        = 25
	MaxObsAdvanceSeconds   = 30 * 24 * 3600
	maxRetryAfterSeconds   = 86400
	senderUserAgent        = "icebath-agent-shared-metrics/1"
	responseBodyReadLimit  = 2048
	rejectedBodyErrorChars = 400
	lastErrorChars         = 500
	// ConsentGateSQL admits a package only when its whole period falls inside one recorded consent window
	ConsentGateSQL = `EXISTS (
    SELECT 1 FROM send_consent_windows w
    WHERE package_outbox.period_start >= w.opened_at
      AND package_outbox.period_end <=
          CASE WHEN w.closed_at IS NULL THEN w.last_confirmed_at
               ELSE w.closed_at END
)`
)

var permanentStatuses = map[int]bool{400: true, 413: true}

// Stamp this module wrote, read back in UTC
func parseStamp(value string) (time.Time, error) {
	parsed, aware, err := pydatetime.ParseISOFormat(value)
	if err != nil {
		return time.Time{}, err
	}
	if !aware {
		return time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(),
			parsed.Nanosecond(), time.Local).UTC(), nil
	}
	return parsed.UTC(), nil
}

// SendOutcome counts what one pass did
type SendOutcome struct {
	Sent     int
	Rejected int
	Deferred int
}

// Response is one ingest answer, its status, Retry-After header and the first bytes of its body
type Response struct {
	Status     int
	RetryAfter *string
	Body       string
}

// PostFunc posts one package body, failing only on transport errors
type PostFunc func(endpoint string, payload []byte, timeout time.Duration) (Response, error)

// TransportError is a transport failure carrying the Python exception class name it reports as
type TransportError struct {
	Class string
	Err   error
}

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Posts one package, gzipped above the threshold with a zero mtime, an HTTP error status being a response
func post(endpoint string, payload []byte, timeout time.Duration) (Response, error) {
	body := payload
	gzipped := len(payload) > GzipThresholdBytes
	if gzipped {
		body = gzipCompress(payload)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, &TransportError{Class: "ValueError", Err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", senderUserAgent)
	if gzipped {
		request.Header.Set("Content-Encoding", "gzip")
	}
	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return Response{}, transportError(err)
	}
	defer response.Body.Close()
	head, err := io.ReadAll(io.LimitReader(response.Body, responseBodyReadLimit))
	if err != nil {
		return Response{}, transportError(err)
	}
	out := Response{Status: response.StatusCode, Body: pystr.DecodeUTF8Replace(head)}
	if values := response.Header.Values("Retry-After"); len(values) > 0 {
		first := values[0]
		out.RetryAfter = &first
	}
	return out, nil
}

// Python exception class a urllib transport failure raises for a Go client error
func transportError(err error) *TransportError {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &TransportError{Class: "TimeoutError", Err: errors.New("timed out")}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return &TransportError{Class: "URLError", Err: fmt.Errorf("<urlopen error %v>", urlErr.Err)}
	}
	return &TransportError{Class: "OSError", Err: err}
}

// Gzip of a payload with a zero header mtime so two sends are byte identical
func gzipCompress(payload []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.ModTime = time.Unix(0, 0)
	w.Write(payload)
	w.Close()
	return buf.Bytes()
}

// Retry-After seconds clamped to one second through one day, the default when absent or unparsable
func retryAfterSeconds(value *string, def int) (int, error) {
	if value == nil {
		return def, nil
	}
	seconds, ok := pyhttpx.RetryAfterSeconds(http.Header{"Retry-After": {*value}})
	if !ok {
		return def, nil
	}
	if math.IsInf(seconds, 0) || math.IsNaN(seconds) {
		return 0, errors.New("cannot convert float infinity to integer")
	}
	return max(1, min(int(math.Trunc(math.Min(seconds, maxRetryAfterSeconds+1))), maxRetryAfterSeconds)), nil
}

// Reconciles the consent window table with the observed send setting inside the caller's write transaction
func ReconcileSendConsent(ctx context.Context, conn state.SQLiteConn, sendEnabled bool, now time.Time) error {
	if now.IsZero() {
		now = utcNow()
	}
	stamp := isoformat(now)
	rawStamp := stamp
	var previous string
	err := conn.QueryRowContext(ctx, "SELECT stamp FROM consent_marks WHERE name = 'obs'").Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		parsed, err := parseStamp(previous)
		if err != nil {
			return err
		}
		ceiling := isoformat(parsed.Add(MaxObsAdvanceSeconds * time.Second))
		stamp = min(stamp, ceiling)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO consent_marks(name, stamp) VALUES ('obs', ?)
        ON CONFLICT(name) DO UPDATE SET stamp = MAX(stamp, excluded.stamp)`, stamp); err != nil {
		return err
	}
	marks := map[string]string{}
	rows, err := conn.QueryContext(ctx, "SELECT name, stamp FROM consent_marks")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			rows.Close()
			return err
		}
		marks[name] = value
	}
	rows.Close()
	obs := marks["obs"]
	var openRow int64
	err = conn.QueryRowContext(ctx, "SELECT rowid FROM send_consent_windows WHERE closed_at IS NULL").Scan(&openRow)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	open := err == nil
	switch {
	case sendEnabled && !open:
		opened := obs
		if data, ok := marks["data"]; ok && data > opened {
			opened = data
		}
		_, err = conn.ExecContext(ctx, "INSERT INTO send_consent_windows(opened_at, last_confirmed_at) VALUES (?, ?)",
			opened, opened)
	case sendEnabled:
		_, err = conn.ExecContext(ctx, "UPDATE send_consent_windows SET last_confirmed_at = MAX(last_confirmed_at, ?)"+
			" WHERE rowid = ?", obs, openRow)
	case open:
		_, err = conn.ExecContext(ctx, "UPDATE send_consent_windows SET closed_at = MIN(last_confirmed_at, ?)"+
			" WHERE rowid = ?", rawStamp, openRow)
	default:
		err = nil
	}
	return err
}

// SharedMetricsSender sends exported packages one bounded pass at a time
type SharedMetricsSender struct {
	store       *SharedMetricsStore
	endpoint    string
	post        PostFunc
	sleep       func(seconds float64)
	now         func() time.Time
	maxAttempts int
	// Called before every package, nil when consent was established out of band
	consentCheck func() (bool, error)
}

// SenderOption overrides one sender collaborator
type SenderOption func(*SharedMetricsSender)

// Replaces the HTTP poster
func WithPost(fn PostFunc) SenderOption { return func(s *SharedMetricsSender) { s.post = fn } }

// Replaces the backoff sleep
func WithSleep(fn func(float64)) SenderOption { return func(s *SharedMetricsSender) { s.sleep = fn } }

// Replaces the clock
func WithNow(fn func() time.Time) SenderOption { return func(s *SharedMetricsSender) { s.now = fn } }

// Replaces the in pass attempts per package
func WithMaxAttempts(n int) SenderOption { return func(s *SharedMetricsSender) { s.maxAttempts = n } }

// Sets the per package consent check
func WithConsentCheck(fn func() (bool, error)) SenderOption {
	return func(s *SharedMetricsSender) { s.consentCheck = fn }
}

// Builds a sender for one store and endpoint
func NewSharedMetricsSender(store *SharedMetricsStore, endpoint string, opts ...SenderOption) *SharedMetricsSender {
	s := &SharedMetricsSender{
		store: store, endpoint: endpoint, post: post, now: func() time.Time { return utcNow() },
		sleep:       func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) },
		maxAttempts: MaxAttempts,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// One store connection inside a write transaction
func (s *SharedMetricsSender) write(ctx context.Context, body func(conn state.SQLiteConn) error) error {
	return s.store.write(ctx, busyTimeoutMS, body)
}

// sendClaim is one package claimed for sending, or a skip marker for a row rejected during the claim
type sendClaim struct {
	packageID   string
	payloadJSON string
	derived     string
	claimToken  string
	skip        bool
}

// Claims exactly one eligible consented package right before it is sent, excluding those this pass finished
func (s *SharedMetricsSender) claimNext(ctx context.Context, now time.Time, seen map[string]bool) (*sendClaim, error) {
	var out *sendClaim
	err := s.write(ctx, func(conn state.SQLiteConn) error {
		stamp := isoformat(now)
		leaseUntil := now.Add(claimLeaseSeconds * time.Second)
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		exclusion := ""
		if len(ids) > 0 {
			exclusion = " AND package_id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
		}
		args := []any{stamp, MaxSendAttempts}
		for _, id := range ids {
			args = append(args, id)
		}
		var packageID string
		var payload, sentID sql.NullString
		err := conn.QueryRowContext(ctx, `SELECT package_id, payload_json, sent_install_id
			FROM package_outbox
			WHERE exported_at IS NOT NULL
			  AND (send_state IS NULL OR send_state = 'pending')
			  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
			  AND `+ConsentGateSQL+`
			  AND send_attempts < ?
			  `+exclusion+`
			ORDER BY created_at, package_id
			LIMIT 1`, args...).Scan(&packageID, &payload, &sentID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		derived := sentID.String
		if derived == "" {
			frozen, ok, err := freezeIdentity(ctx, conn, packageID, payload)
			if err != nil {
				return err
			}
			if !ok {
				out = &sendClaim{packageID: packageID, skip: true}
				return nil
			}
			derived = frozen
		}
		token := newUUID()
		if _, err := conn.ExecContext(ctx, `UPDATE package_outbox
			SET send_state = 'pending',
			    send_attempts = send_attempts + 1,
			    next_attempt_at = ?,
			    claim_token = ?
			WHERE package_id = ?`, isoformat(leaseUntil), token, packageID); err != nil {
			return err
		}
		out = &sendClaim{packageID: packageID, payloadJSON: payload.String, derived: derived, claimToken: token}
		return nil
	})
	return out, err
}

// Records the transmitted install id on the row, or rejects a row whose payload cannot carry one
func freezeIdentity(ctx context.Context, conn state.SQLiteConn, packageID string, payloadJSON sql.NullString) (string, bool, error) {
	reason := ""
	installID := ""
	var payload any
	var err error
	if !payloadJSON.Valid {
		err = errors.New("the JSON object must be str, bytes or bytearray, not NoneType")
	} else {
		payload, err = pyjson.Loads(payloadJSON.String)
	}
	if err != nil {
		reason = "unreadable payload"
	} else if obj, ok := payload.(*pyjson.Object); !ok {
		reason = fmt.Sprintf("payload is %s, expected object", pyrepr.TypeName(payload))
	} else if id, ok := obj.Vals["install_id"].(string); !ok || pystr.Strip(id) == "" {
		reason = "payload has no usable install_id"
	} else {
		installID = id
	}
	if reason != "" {
		slog.Warn(fmt.Sprintf("Shared-metrics package %s cannot be sent (%s)", packageID, reason))
		_, err := conn.ExecContext(ctx, `UPDATE package_outbox
                SET send_state = 'rejected', last_error = ?
                WHERE package_id = ?`, reason, packageID)
		return "", false, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE package_outbox SET sent_install_id = ? WHERE package_id = ?",
		installID, packageID); err != nil {
		return "", false, err
	}
	return installID, true, nil
}

// Exact bytes to send, the stored package with the frozen install id, sorted with two space indent
func senderBody(payloadJSON, transmittedID string) ([]byte, error) {
	decoded, err := pyjson.Loads(payloadJSON)
	if err != nil {
		return nil, err
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("cannot convert %s to a dict", pyrepr.TypeName(decoded))
	}
	payload := pyjson.NewObject()
	for _, k := range obj.Keys {
		payload.Set(k, obj.Vals[k])
	}
	payload.Set("install_id", transmittedID)
	text, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.SortKeys(true))
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// sendColumn is one column assignment of a send state write
type sendColumn struct {
	name  string
	value any
}

// Writes send state for one package unless another process finished it, compare and set on a claim token
func (s *SharedMetricsSender) mark(ctx context.Context, packageID string, token *string, columns ...sendColumn) error {
	assignments := make([]string, len(columns))
	params := make([]any, 0, len(columns)+2)
	for i, c := range columns {
		assignments[i] = c.name + " = ?"
		params = append(params, c.value)
	}
	params = append(params, packageID)
	predicate := " AND (send_state IS NULL OR send_state = 'pending')"
	if token != nil {
		predicate += " AND claim_token = ?"
		params = append(params, *token)
	}
	return s.write(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE package_outbox SET "+strings.Join(assignments, ", ")+
			" WHERE package_id = ?"+predicate, params...)
		return err
	})
}

// Reasserts ownership and extends the lease in one compare and set, false when superseded, lapsed or failing
func (s *SharedMetricsSender) renewClaim(ctx context.Context, packageID string, token *string) bool {
	if token == nil {
		return false
	}
	now := s.now()
	leaseUntil := now.Add(claimLeaseSeconds * time.Second)
	renewed := false
	err := s.write(ctx, func(conn state.SQLiteConn) error {
		res, err := conn.ExecContext(ctx, `UPDATE package_outbox
			SET next_attempt_at = ?
			WHERE package_id = ?
			  AND claim_token = ?
			  AND (send_state IS NULL OR send_state = 'pending')
			  AND next_attempt_at > ?`, isoformat(leaseUntil), packageID, *token, isoformat(now))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		renewed = n == 1
		return err
	})
	if err != nil {
		slog.Warn("Unable to renew shared-metrics claim", "error", err)
		return false
	}
	return renewed
}

// First n code points of a string
func headChars(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Leaves a package pending until at least one second from now with its reason
func (s *SharedMetricsSender) deferPackage(ctx context.Context, packageID string, delaySeconds int, reason string, token *string) error {
	retryAt := s.now().Add(time.Duration(max(1, delaySeconds)) * time.Second)
	return s.mark(ctx, packageID, token, sendColumn{"send_state", "pending"},
		sendColumn{"next_attempt_at", isoformat(retryAt)}, sendColumn{"last_error", headChars(reason, lastErrorChars)})
}

// Tries one package and returns sent, rejected or deferred
func (s *SharedMetricsSender) sendOne(ctx context.Context, pkg *sendClaim) (string, error) {
	var token *string
	if pkg.claimToken != "" {
		token = &pkg.claimToken
	}
	body, err := senderBody(pkg.payloadJSON, pkg.derived)
	if err != nil {
		return "", err
	}
	deferred := func(delay int, reason string) (string, error) {
		return "deferred", s.deferPackage(ctx, pkg.packageID, delay, reason, token)
	}
	for attempt := 1; attempt <= s.maxAttempts; attempt++ {
		if !s.renewClaim(ctx, pkg.packageID, token) {
			slog.Info(fmt.Sprintf("Shared-metrics claim on %s superseded or expired; yielding", pkg.packageID))
			return "deferred", nil
		}
		var reason string
		response, err := s.post(s.endpoint, body, RequestTimeoutSeconds*time.Second)
		if err != nil {
			reason = exceptionText(err)
		} else {
			if response.Status == 202 {
				return "sent", s.mark(ctx, pkg.packageID, token, sendColumn{"send_state", "sent"},
					sendColumn{"sent_at", isoformat(s.now())}, sendColumn{"last_error", nil})
			}
			if permanentStatuses[response.Status] {
				slog.Warn(fmt.Sprintf("Telemetry package %s rejected with HTTP %d; not retrying", pkg.packageID, response.Status))
				return "rejected", s.mark(ctx, pkg.packageID, token, sendColumn{"send_state", "rejected"},
					sendColumn{"last_error", fmt.Sprintf("HTTP %d: %s", response.Status, headChars(response.Body, rejectedBodyErrorChars))})
			}
			if response.Status == 429 {
				delay, err := retryAfterSeconds(response.RetryAfter, failureBackoffSeconds)
				if err != nil {
					return "", err
				}
				return deferred(delay, "rate limited")
			}
			reason = fmt.Sprintf("HTTP %d", response.Status)
		}
		if attempt >= s.maxAttempts {
			return deferred(failureBackoffSeconds, reason)
		}
		s.sleep(backoff(attempt))
	}
	return deferred(failureBackoffSeconds, "attempts exhausted")
}

// Python type name and message of a transport failure
func exceptionText(err error) string {
	var te *TransportError
	if errors.As(err, &te) {
		return te.Class + ": " + te.Err.Error()
	}
	return "OSError: " + err.Error()
}

// Full jitter backoff of 1, 5 and 25 seconds
func backoff(attempt int) float64 {
	return rand.Float64() * backoffBaseSeconds * math.Pow(backoffFactor, float64(attempt-1))
}

// Runs one bounded pass, rechecking consent before each package, never failing
func (s *SharedMetricsSender) SendPending(ctx context.Context) SendOutcome {
	var outcome SendOutcome
	seen := map[string]bool{}
	for i := 0; i < MaxPackagesPerPass; i++ {
		if !s.stillConsented() {
			slog.Info("Shared-metrics sending disabled mid-pass; stopping")
			s.reconcile(ctx, false)
			break
		}
		pkg, err := s.claimNext(ctx, s.now(), seen)
		if err != nil {
			slog.Warn("Unable to select shared-metrics packages", "error", err)
			break
		}
		if pkg == nil {
			break
		}
		seen[pkg.packageID] = true
		result := "rejected"
		if !pkg.skip {
			result, err = s.sendOneGuarded(ctx, pkg)
			if err != nil {
				slog.Warn("Unable to send shared-metrics package", "error", err)
				result = "deferred"
			}
		}
		switch result {
		case "sent":
			outcome.Sent++
		case "rejected":
			outcome.Rejected++
		default:
			outcome.Deferred++
		}
	}
	return outcome
}

// Sends one package turning a panic in a collaborator into an error
func (s *SharedMetricsSender) sendOneGuarded(ctx context.Context, pkg *sendClaim) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = "", fmt.Errorf("%v", r)
		}
	}()
	return s.sendOne(ctx, pkg)
}

// Runs the single consent writer from within a pass
func (s *SharedMetricsSender) reconcile(ctx context.Context, sendEnabled bool) {
	err := s.write(ctx, func(conn state.SQLiteConn) error {
		return ReconcileSendConsent(ctx, conn, sendEnabled, s.now())
	})
	if err != nil {
		slog.Warn("Unable to reconcile shared-metrics consent", "error", err)
	}
}

// Rereads send consent, failing closed when it cannot be established
func (s *SharedMetricsSender) stillConsented() (ok bool) {
	if s.consentCheck == nil {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("Unable to confirm shared-metrics send consent; stopping", "error", r)
			ok = false
		}
	}()
	consented, err := s.consentCheck()
	if err != nil {
		slog.Warn("Unable to confirm shared-metrics send consent; stopping", "error", err)
		return false
	}
	return consented
}
