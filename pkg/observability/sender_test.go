package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

const (
	senderInstallID = "12a73e97-4de9-4766-830d-9ca1192c0420"
	senderEndpoint  = "https://telemetry.test/v1/telemetry"
)

var senderNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// fakeTransport answers posts from a queue, 202 once it runs dry, an error item failing the post
type fakeTransport struct {
	responses []any
	calls     [][]byte
	timeouts  []time.Duration
}

func newTransport(responses ...any) *fakeTransport { return &fakeTransport{responses: responses} }

func (f *fakeTransport) post(endpoint string, payload []byte, timeout time.Duration) (Response, error) {
	f.calls = append(f.calls, payload)
	f.timeouts = append(f.timeouts, timeout)
	if len(f.responses) == 0 {
		return Response{Status: 202}, nil
	}
	item := f.responses[0]
	f.responses = f.responses[1:]
	if err, ok := item.(error); ok {
		return Response{}, err
	}
	return item.(Response), nil
}

func (f *fakeTransport) bodies(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range f.calls {
		var body map[string]any
		if err := json.Unmarshal(c, &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body)
	}
	return out
}

func status(code int) Response { return Response{Status: code} }

func retryAfter(code int, value string) Response { return Response{Status: code, RetryAfter: &value} }

func repeat(item any, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = item
	}
	return out
}

func offline() error { return &TransportError{Class: "OSError", Err: errors.New("offline")} }

// Store under the test dir with one consent window opened before every package and confirmed past the test clock
func senderStore(t *testing.T) *SharedMetricsStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSharedMetricsStore(context.Background(), filepath.Join(dir, "metrics.sqlite3"), filepath.Join(dir, "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	grantConsent(t, s, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	return s
}

func storeTxn(t *testing.T, s *SharedMetricsStore, body func(conn state.SQLiteConn) error) {
	t.Helper()
	if err := s.write(context.Background(), busyTimeoutMS, body); err != nil {
		t.Fatal(err)
	}
}

func storeExec(t *testing.T, s *SharedMetricsStore, query string, args ...any) {
	t.Helper()
	err := s.connection(context.Background(), busyTimeoutMS, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(context.Background(), query, args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func storeScalar[T any](t *testing.T, s *SharedMetricsStore, query string, args ...any) T {
	t.Helper()
	var out T
	err := s.connection(context.Background(), busyTimeoutMS, func(conn state.SQLiteConn) error {
		return conn.QueryRowContext(context.Background(), query, args...).Scan(&out)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func grantConsent(t *testing.T, s *SharedMetricsStore, opened, confirmedThrough time.Time) {
	t.Helper()
	storeTxn(t, s, func(conn state.SQLiteConn) error {
		if err := ReconcileSendConsent(context.Background(), conn, true, opened); err != nil {
			return err
		}
		return ReconcileSendConsent(context.Background(), conn, true, confirmedThrough)
	})
}

func revokeConsent(t *testing.T, s *SharedMetricsStore, at time.Time) {
	t.Helper()
	storeTxn(t, s, func(conn state.SQLiteConn) error {
		return ReconcileSendConsent(context.Background(), conn, false, at)
	})
}

func clearConsent(t *testing.T, s *SharedMetricsStore) {
	t.Helper()
	storeTxn(t, s, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(context.Background(), "DELETE FROM send_consent_windows"); err != nil {
			return err
		}
		_, err := conn.ExecContext(context.Background(), "DELETE FROM consent_marks")
		return err
	})
}

func addPackage(t *testing.T, s *SharedMetricsStore, packageID, periodDay string, exported bool, installID any) string {
	t.Helper()
	payload := map[string]any{
		"schema_version": "icebath.shared_metrics.v2", "package_id": packageID, "install_id": installID,
		"period_start": periodDay + "T00:00:00Z", "period_end": periodDay + "T23:59:59Z",
		"metrics": []any{map[string]any{"name": "icebath.client.active", "type": "counter", "value": 1}},
	}
	text, _ := json.Marshal(payload)
	var exportedAt any
	if exported {
		exportedAt = periodDay + "T01:00:01Z"
	}
	storeExec(t, s, `INSERT INTO package_outbox(package_id, period_start, period_end, payload_json, created_at, exported_at)
		VALUES (?, ?, ?, ?, ?, ?)`, packageID, periodDay+"T00:00:00Z", periodDay+"T23:59:59Z", string(text),
		periodDay+"T01:00:00Z", exportedAt)
	path := filepath.Join(s.OutboxDirectory, packageID+".json")
	pretty, _ := json.MarshalIndent(payload, "", "  ")
	if err := os.WriteFile(path, pretty, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func add(t *testing.T, s *SharedMetricsStore, packageID, periodDay string) string {
	return addPackage(t, s, packageID, periodDay, true, senderInstallID)
}

type outboxRow struct {
	SendState, SentAt, NextAttemptAt, LastError, SentInstallID *string
	SendAttempts                                               int
}

func rowOf(t *testing.T, s *SharedMetricsStore, packageID string) outboxRow {
	t.Helper()
	var r outboxRow
	err := s.connection(context.Background(), busyTimeoutMS, func(conn state.SQLiteConn) error {
		return conn.QueryRowContext(context.Background(), `SELECT send_state, sent_at, send_attempts, next_attempt_at,
			last_error, sent_install_id FROM package_outbox WHERE package_id = ?`, packageID).Scan(&r.SendState,
			&r.SentAt, &r.SendAttempts, &r.NextAttemptAt, &r.LastError, &r.SentInstallID)
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func newSender(s *SharedMetricsStore, post PostFunc, now time.Time, opts ...SenderOption) *SharedMetricsSender {
	base := []SenderOption{WithPost(post), WithSleep(func(float64) {}), WithNow(func() time.Time { return now })}
	return NewSharedMetricsSender(s, senderEndpoint, append(base, opts...)...)
}

func send(s *SharedMetricsStore, f *fakeTransport, opts ...SenderOption) SendOutcome {
	return newSender(s, f.post, senderNow, opts...).SendPending(context.Background())
}

func TestSenderContractResponses(t *testing.T) {
	t.Run("202_marks_sent", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		if out := send(s, newTransport(status(202))); out.Sent != 1 {
			t.Fatalf("%+v", out)
		}
		r := rowOf(t, s, "pkg-1")
		if deref(r.SendState) != "sent" || r.SentAt == nil {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("400_is_permanent_and_never_retried", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(Response{Status: 400, Body: `{"error":"invalid_envelope"}`})
		if out := send(s, tr); out.Rejected != 1 || len(tr.calls) != 1 {
			t.Fatalf("%+v %d", out, len(tr.calls))
		}
		if deref(rowOf(t, s, "pkg-1").SendState) != "rejected" {
			t.Fatal("not rejected")
		}
		tr2 := newTransport(status(202))
		send(s, tr2)
		if len(tr2.calls) != 0 {
			t.Fatal("a rejected package was resent")
		}
	})
	for _, code := range []int{401, 403, 404, 422, 500, 503} {
		t.Run(fmt.Sprintf("unspecified_status_%d_is_retried_not_discarded", code), func(t *testing.T) {
			s := senderStore(t)
			add(t, s, "pkg-1", "2026-08-26")
			if out := send(s, newTransport(repeat(status(code), 3)...)); out.Deferred != 1 {
				t.Fatalf("%+v", out)
			}
			if deref(rowOf(t, s, "pkg-1").SendState) != "pending" {
				t.Fatal("not pending")
			}
		})
	}
	t.Run("413_is_permanent", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(status(413))
		if out := send(s, tr); out.Rejected != 1 || len(tr.calls) != 1 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("429_defers_using_retry_after", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(retryAfter(429, "120"))
		if out := send(s, tr); out.Deferred != 1 || len(tr.calls) != 1 {
			t.Fatalf("%+v", out)
		}
		r := rowOf(t, s, "pkg-1")
		if deref(r.SendState) != "pending" || deref(r.NextAttemptAt) != "2026-08-26T12:02:00Z" {
			t.Fatalf("%s %s", deref(r.SendState), deref(r.NextAttemptAt))
		}
	})
	t.Run("429_without_retry_after_still_defers", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(status(429)))
		if deref(rowOf(t, s, "pkg-1").NextAttemptAt) <= "2026-08-26T12:00:00Z" {
			t.Fatal("not deferred")
		}
	})
	t.Run("absurd_retry_after_is_clamped", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(retryAfter(429, "99999999")))
		if deref(rowOf(t, s, "pkg-1").NextAttemptAt) > "2026-08-27T12:00:00Z" {
			t.Fatal("not clamped")
		}
	})
	t.Run("5xx_retries_then_defers", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(status(503), status(503), status(503))
		if out := send(s, tr); out.Deferred != 1 || len(tr.calls) != 3 {
			t.Fatalf("%+v %d", out, len(tr.calls))
		}
		if deref(rowOf(t, s, "pkg-1").SendState) != "pending" {
			t.Fatal("not pending")
		}
	})
	t.Run("5xx_then_success_within_the_same_pass", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(status(503), status(202))
		if out := send(s, tr); out.Sent != 1 || len(tr.calls) != 2 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("transport_failure_is_retryable", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		if out := send(s, newTransport(offline(), offline(), status(202))); out.Sent != 1 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("persistent_offline_defers_without_raising", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		if out := send(s, newTransport(repeat(offline(), 3)...)); out.Deferred != 1 {
			t.Fatalf("%+v", out)
		}
		if !strings.Contains(deref(rowOf(t, s, "pkg-1").LastError), "OSError") {
			t.Fatal(deref(rowOf(t, s, "pkg-1").LastError))
		}
	})
}

func sentIDs(t *testing.T, f *fakeTransport) []string {
	var ids []string
	for _, b := range f.bodies(t) {
		ids = append(ids, b["package_id"].(string))
	}
	return ids
}

func TestSenderConsentGate(t *testing.T) {
	t.Run("packages_from_before_opt_in_are_never_sent", func(t *testing.T) {
		s := senderStore(t)
		clearConsent(t, s)
		grantConsent(t, s, time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		add(t, s, "old", "2026-08-20")
		add(t, s, "new", "2026-08-26")
		tr := newTransport(status(202))
		send(s, tr)
		if ids := sentIDs(t, tr); len(ids) != 1 || ids[0] != "new" {
			t.Fatal(ids)
		}
	})
	t.Run("a_period_straddling_opt_in_day_is_sent_whole", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "head", "2026-08-26")
		add(t, s, "tail", "2026-08-26")
		tr := newTransport(status(202), status(202))
		send(s, tr)
		if ids := sentIDs(t, tr); strings.Join(ids, ",") != "head,tail" {
			t.Fatal(ids)
		}
	})
	t.Run("opt_in_is_immortalised_as_a_window_not_a_day", func(t *testing.T) {
		s := senderStore(t)
		if n := storeScalar[int](t, s, "SELECT COUNT(*) FROM send_consent_windows WHERE closed_at IS NULL"); n != 1 {
			t.Fatal(n)
		}
		grantConsent(t, s, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		if n := storeScalar[int](t, s, "SELECT COUNT(*) FROM send_consent_windows"); n != 1 {
			t.Fatal(n)
		}
	})
	t.Run("no_consent_window_means_nothing_is_sent", func(t *testing.T) {
		s := senderStore(t)
		clearConsent(t, s)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(status(202))
		send(s, tr)
		if len(tr.calls) != 0 {
			t.Fatal("sent without consent")
		}
	})
	t.Run("unexported_packages_are_skipped", func(t *testing.T) {
		s := senderStore(t)
		addPackage(t, s, "pending-export", "2026-08-26", false, senderInstallID)
		tr := newTransport(status(202))
		send(s, tr)
		if len(tr.calls) != 0 {
			t.Fatal("sent an unexported package")
		}
	})
	t.Run("revoking_then_re_enabling_never_releases_the_off_window", func(t *testing.T) {
		s := senderStore(t)
		clearConsent(t, s)
		grantConsent(t, s, senderNow.AddDate(0, 0, -2), senderNow)
		add(t, s, "consented", "2026-08-25")
		revokeConsent(t, s, senderNow)
		for _, day := range []string{"2026-08-27", "2026-08-28", "2026-08-29"} {
			add(t, s, "refused-"+day, day)
		}
		later := senderNow.AddDate(0, 0, 5)
		grantConsent(t, s, later, later.AddDate(0, 0, 30))
		tr := newTransport(repeat(status(202), 10)...)
		newSender(s, tr.post, later).SendPending(context.Background())
		ids := sentIDs(t, tr)
		joined := strings.Join(ids, ",")
		if strings.Contains(joined, "refused") || !strings.Contains(joined, "consented") {
			t.Fatal(ids)
		}
	})
	t.Run("a_package_from_after_re_enabling_is_sent", func(t *testing.T) {
		s := senderStore(t)
		clearConsent(t, s)
		grantConsent(t, s, senderNow.AddDate(0, 0, -2), senderNow)
		revokeConsent(t, s, senderNow)
		later := senderNow.AddDate(0, 0, 5)
		grantConsent(t, s, later, later.AddDate(0, 0, 10))
		add(t, s, "after-re-optin", later.AddDate(0, 0, 1).Format("2006-01-02"))
		tr := newTransport(status(202))
		newSender(s, tr.post, later.AddDate(0, 0, 2)).SendPending(context.Background())
		if len(tr.calls) != 1 {
			t.Fatal(len(tr.calls))
		}
	})
}

func TestSenderIdentity(t *testing.T) {
	t.Run("transmitted_id_is_frozen_on_the_row", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(status(503), status(202))
		send(s, tr)
		if deref(rowOf(t, s, "pkg-1").SentInstallID) != tr.bodies(t)[0]["install_id"] {
			t.Fatal("identity not frozen")
		}
	})
	t.Run("retries_send_identical_bytes", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(status(503), status(503), status(202))
		send(s, tr)
		for _, c := range tr.calls[1:] {
			if string(c) != string(tr.calls[0]) {
				t.Fatal("resend differs")
			}
		}
	})
	t.Run("only_install_id_differs_from_the_stored_package", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		tr := newTransport(status(202))
		send(s, tr)
		sent := tr.bodies(t)[0]
		var stored map[string]any
		json.Unmarshal([]byte(storeScalar[string](t, s, "SELECT payload_json FROM package_outbox WHERE package_id = 'pkg-1'")), &stored)
		if len(sent) != len(stored) {
			t.Fatal(sent, stored)
		}
		for k, v := range stored {
			a, _ := json.Marshal(v)
			b, _ := json.Marshal(sent[k])
			if k != "install_id" && string(a) != string(b) {
				t.Fatal(k)
			}
		}
	})
}

func TestSenderOutboxIsNotAQueue(t *testing.T) {
	for _, code := range []int{202, 400} {
		s := senderStore(t)
		path := add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(status(code)))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%d: the outbox file was deleted", code)
		}
	}
	s := senderStore(t)
	add(t, s, "pkg-1", "2026-08-26")
	send(s, newTransport(status(202)))
	if n := storeScalar[int](t, s, "SELECT COUNT(*) FROM package_outbox WHERE package_id = 'pkg-1'"); n != 1 {
		t.Fatal(n)
	}
}

func TestSenderClaimingAndBounds(t *testing.T) {
	t.Run("a_sent_package_is_not_resent", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(status(202)))
		second := newTransport(status(202))
		send(s, second)
		if len(second.calls) != 0 {
			t.Fatal("resent")
		}
	})
	t.Run("a_deferred_package_is_skipped_until_due", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(retryAfter(429, "600")))
		second := newTransport(status(202))
		send(s, second)
		if len(second.calls) != 0 {
			t.Fatal("backoff ignored")
		}
	})
	t.Run("a_deferred_package_is_retried_once_due", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(retryAfter(429, "60")))
		tr := newTransport(status(202))
		newSender(s, tr.post, senderNow.Add(5*time.Minute)).SendPending(context.Background())
		if len(tr.calls) != 1 {
			t.Fatal(len(tr.calls))
		}
	})
	t.Run("attempts_are_counted", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(status(429)))
		if rowOf(t, s, "pkg-1").SendAttempts != 1 {
			t.Fatal(rowOf(t, s, "pkg-1").SendAttempts)
		}
	})
	t.Run("a_pass_is_bounded", func(t *testing.T) {
		s := senderStore(t)
		for i := 0; i < MaxPackagesPerPass+5; i++ {
			add(t, s, fmt.Sprintf("pkg-%02d", i), "2026-08-26")
		}
		if out := send(s, newTransport(repeat(status(202), 40)...)); out.Sent != MaxPackagesPerPass {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("two_concurrent_passes_do_not_double_send", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		var first, second int
		secondPost := func(string, []byte, time.Duration) (Response, error) {
			second++
			return status(202), nil
		}
		firstPost := func(string, []byte, time.Duration) (Response, error) {
			first++
			newSender(s, secondPost, senderNow).SendPending(context.Background())
			return status(202), nil
		}
		newSender(s, firstPost, senderNow).SendPending(context.Background())
		if first != 1 || second != 0 {
			t.Fatal(first, second)
		}
	})
	t.Run("a_claim_leases_the_row_long_enough_to_cover_a_worst_case_send", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		claimed, err := newSender(s, newTransport().post, senderNow).claimNext(context.Background(), senderNow, map[string]bool{})
		if err != nil || claimed == nil {
			t.Fatal(err)
		}
		worst := RequestTimeoutSeconds*MaxAttempts + 1 + 5 + 25
		if deref(rowOf(t, s, "pkg-1").NextAttemptAt) < isoformat(senderNow.Add(time.Duration(worst)*time.Second)) {
			t.Fatal("lease too short")
		}
	})
}

func TestSenderLeases(t *testing.T) {
	ctx := context.Background()
	t.Run("a_slow_multi_package_pass_does_not_lose_its_lease", func(t *testing.T) {
		s := senderStore(t)
		for i := 0; i < 3; i++ {
			add(t, s, fmt.Sprintf("pkg-%d", i), "2026-08-26")
		}
		clock := senderNow
		var firstPosts, secondPosts []string
		postFn := func(_ string, payload []byte, _ time.Duration) (Response, error) {
			var body map[string]any
			json.Unmarshal(payload, &body)
			pid := body["package_id"].(string)
			firstPosts = append(firstPosts, pid)
			clock = clock.Add(96 * time.Second)
			held := rowOf(t, s, pid)
			if held.NextAttemptAt != nil && *held.NextAttemptAt <= isoformat(clock) && deref(held.SendState) != "sent" {
				secondPosts = append(secondPosts, pid)
			}
			return status(202), nil
		}
		NewSharedMetricsSender(s, senderEndpoint, WithPost(postFn), WithSleep(func(float64) {}),
			WithNow(func() time.Time { return clock })).SendPending(ctx)
		if strings.Join(firstPosts, ",") != "pkg-0,pkg-1,pkg-2" || len(secondPosts) != 0 {
			t.Fatal(firstPosts, secondPosts)
		}
	})
	t.Run("a_re_eligible_head_row_does_not_starve_the_tail", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "aaa-head", "2026-08-26")
		for i := 0; i < 5; i++ {
			add(t, s, fmt.Sprintf("zzz-%d", i), "2026-08-26")
		}
		storeExec(t, s, "UPDATE package_outbox SET created_at = '2026-08-26T00:00:00Z' WHERE package_id = 'aaa-head'")
		var posts []string
		postFn := func(_ string, payload []byte, _ time.Duration) (Response, error) {
			var body map[string]any
			json.Unmarshal(payload, &body)
			pid := body["package_id"].(string)
			posts = append(posts, pid)
			if pid == "aaa-head" {
				return retryAfter(429, "1"), nil
			}
			return status(202), nil
		}
		NewSharedMetricsSender(s, senderEndpoint, WithPost(postFn), WithSleep(func(float64) {}),
			WithNow(func() time.Time { return senderNow.Add(time.Duration(30*len(posts)) * time.Second) })).SendPending(ctx)
		delivered := map[string]bool{}
		for _, p := range posts {
			if strings.HasPrefix(p, "zzz") {
				delivered[p] = true
			}
		}
		if len(delivered) != 5 {
			t.Fatal(delivered)
		}
	})
	t.Run("a_poisoned_package_is_abandoned_eventually", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		attempts := 0
		postFn := func(string, []byte, time.Duration) (Response, error) {
			attempts++
			return status(503), nil
		}
		for i := 0; i < 60; i++ {
			newSender(s, postFn, senderNow.Add(time.Duration(i)*time.Hour)).SendPending(ctx)
		}
		if n := rowOf(t, s, "pkg-1").SendAttempts; n > MaxSendAttempts || attempts >= 100 {
			t.Fatal(n, attempts)
		}
	})
	t.Run("a_lapsed_claimant_yields_even_before_anyone_reclaims", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		var posts []string
		clock := senderNow
		a := NewSharedMetricsSender(s, senderEndpoint, WithSleep(func(float64) {}),
			WithPost(func(string, []byte, time.Duration) (Response, error) {
				posts = append(posts, "A")
				return status(202), nil
			}), WithNow(func() time.Time { return clock }))
		claimed, err := a.claimNext(ctx, senderNow, map[string]bool{})
		if err != nil || claimed == nil || claimed.skip {
			t.Fatal(err)
		}
		clock = senderNow.Add(400 * time.Second)
		result, err := a.sendOne(ctx, claimed)
		if err != nil || len(posts) != 0 || result != "deferred" || deref(rowOf(t, s, "pkg-1").SendState) != "pending" {
			t.Fatal(err, posts, result)
		}
	})
	t.Run("renewal_extends_the_lease_across_the_post", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		clock := senderNow
		sender := NewSharedMetricsSender(s, senderEndpoint, WithSleep(func(float64) {}),
			WithPost(newTransport().post), WithNow(func() time.Time { return clock }))
		claimed, err := sender.claimNext(ctx, senderNow, map[string]bool{})
		if err != nil || claimed == nil {
			t.Fatal(err)
		}
		before := deref(rowOf(t, s, "pkg-1").NextAttemptAt)
		clock = senderNow.Add(100 * time.Second)
		if !sender.renewClaim(ctx, "pkg-1", &claimed.claimToken) {
			t.Fatal("renewal refused")
		}
		after := deref(rowOf(t, s, "pkg-1").NextAttemptAt)
		if after <= before || after != senderNow.Add(400*time.Second).Format("2006-01-02T15:04:05Z") {
			t.Fatal(before, after)
		}
	})
	t.Run("a_lapsed_claimant_resuming_after_reclaim_cannot_double_post", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		var posts []string
		poster := func(name string) PostFunc {
			return func(string, []byte, time.Duration) (Response, error) {
				posts = append(posts, name)
				return status(202), nil
			}
		}
		a := newSender(s, poster("A"), senderNow)
		claimedA, err := a.claimNext(ctx, senderNow, map[string]bool{})
		if err != nil || claimedA == nil || claimedA.skip {
			t.Fatal(err)
		}
		if out := newSender(s, poster("B"), senderNow.Add(400*time.Second)).SendPending(ctx); out.Sent != 1 {
			t.Fatalf("%+v", out)
		}
		result, _ := a.sendOne(ctx, claimedA)
		if strings.Join(posts, ",") != "B" || result != "deferred" || deref(rowOf(t, s, "pkg-1").SendState) != "sent" {
			t.Fatal(posts, result)
		}
	})
	t.Run("a_lapsed_claimants_backoff_cannot_clobber_the_new_claim", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		a := newSender(s, newTransport(offline(), offline(), offline()).post, senderNow)
		claimedA, err := a.claimNext(ctx, senderNow, map[string]bool{})
		if err != nil || claimedA == nil || claimedA.skip {
			t.Fatal(err)
		}
		later := senderNow.Add(400 * time.Second)
		claimedB, err := newSender(s, newTransport().post, later).claimNext(ctx, later, map[string]bool{})
		if err != nil || claimedB == nil || claimedB.skip {
			t.Fatal(err)
		}
		leaseB := deref(rowOf(t, s, "pkg-1").NextAttemptAt)
		if result, _ := a.sendOne(ctx, claimedA); result != "deferred" {
			t.Fatal(result)
		}
		if got := deref(rowOf(t, s, "pkg-1").NextAttemptAt); got != leaseB {
			t.Fatal(got, leaseB)
		}
	})
	t.Run("an_expired_lease_is_reclaimed", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(offline(), offline(), offline()))
		tr := newTransport(status(202))
		newSender(s, tr.post, senderNow.Add(2*time.Hour)).SendPending(ctx)
		if len(tr.calls) != 1 {
			t.Fatal(len(tr.calls))
		}
	})
	t.Run("a_lapsed_sender_cannot_resurrect_a_sent_package", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "pkg-1", "2026-08-26")
		send(s, newTransport(status(202)))
		if err := newSender(s, newTransport().post, senderNow).deferPackage(ctx, "pkg-1", 600, "stale", nil); err != nil {
			t.Fatal(err)
		}
		if deref(rowOf(t, s, "pkg-1").SendState) != "sent" {
			t.Fatal("a lapsed pass overwrote a completed send")
		}
	})
}

func insertRawPackage(t *testing.T, s *SharedMetricsStore, payloadJSON string) {
	t.Helper()
	storeExec(t, s, `INSERT INTO package_outbox(package_id, period_start, period_end, payload_json, created_at, exported_at)
		VALUES ('bad', '2026-08-26T00:00:00Z', '2026-08-26T23:59:59Z', ?, '2026-08-26T00:00:00Z', '2026-08-26T01:00:00Z')`,
		payloadJSON)
}

func TestSenderResilience(t *testing.T) {
	t.Run("a_corrupt_row_does_not_stop_the_pass", func(t *testing.T) {
		s := senderStore(t)
		add(t, s, "good", "2026-08-26")
		insertRawPackage(t, s, "not json")
		if out := send(s, newTransport(repeat(status(202), 5)...)); out.Sent < 1 {
			t.Fatalf("%+v", out)
		}
	})
	for _, payload := range []string{`["a", "list"]`, "null", `"a string"`, "42", `{"no_install_id": true}`,
		`{"install_id": ""}`, `{"install_id": null}`} {
		t.Run("valid_json_that_is_not_a_usable_package_is_skipped_"+payload, func(t *testing.T) {
			s := senderStore(t)
			insertRawPackage(t, s, payload)
			add(t, s, "good", "2026-08-26")
			tr := newTransport(repeat(status(202), 5)...)
			if out := send(s, tr); out.Sent != 1 {
				t.Fatalf("%+v", out)
			}
			if ids := sentIDs(t, tr); strings.Join(ids, ",") != "good" {
				t.Fatal(ids)
			}
			if deref(rowOf(t, s, "bad").SendState) != "rejected" {
				t.Fatal("bad row not rejected")
			}
		})
	}
	t.Run("send_pending_never_raises_on_a_broken_database", func(t *testing.T) {
		s := senderStore(t)
		if err := os.WriteFile(s.DatabasePath, []byte("this is not a database"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out := send(s, newTransport(status(202))); out.Sent != 0 {
			t.Fatalf("%+v", out)
		}
	})
}

func TestSenderConsentRevocation(t *testing.T) {
	ctx := context.Background()
	t.Run("revoking_consent_mid_pass_stops_further_sends", func(t *testing.T) {
		s := senderStore(t)
		for i := 0; i < 4; i++ {
			add(t, s, fmt.Sprintf("pkg-%d", i), "2026-08-26")
		}
		consented := true
		posts := 0
		postFn := func(string, []byte, time.Duration) (Response, error) {
			posts++
			consented = false
			return status(202), nil
		}
		out := newSender(s, postFn, senderNow, WithConsentCheck(func() (bool, error) { return consented, nil })).SendPending(ctx)
		if posts != 1 || out.Sent != 1 {
			t.Fatal(posts, out)
		}
	})
	for name, check := range map[string]func() (bool, error){
		"no_send_at_all_when_consent_is_already_false": func() (bool, error) { return false, nil },
		"an_unreadable_consent_check_fails_closed":     func() (bool, error) { return false, errors.New("config unreadable") },
	} {
		t.Run(name, func(t *testing.T) {
			s := senderStore(t)
			add(t, s, "pkg-1", "2026-08-26")
			tr := newTransport(status(202))
			newSender(s, tr.post, senderNow, WithConsentCheck(check)).SendPending(ctx)
			if len(tr.calls) != 0 {
				t.Fatal("sent without consent")
			}
		})
	}
}

type capturedRequest struct {
	data    []byte
	headers http.Header
}

func captureRequest(t *testing.T, payload []byte) capturedRequest {
	t.Helper()
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.data, _ = io.ReadAll(r.Body)
		got.headers = r.Header.Clone()
		w.WriteHeader(202)
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	response, err := post(srv.URL, payload, 5*time.Second)
	if err != nil || response.Status != 202 || response.Body != "{}" {
		t.Fatal(err, response)
	}
	return got
}

func TestSenderCompression(t *testing.T) {
	large, _ := json.Marshal(map[string]any{"filler": strings.Repeat("x", 20000)})
	t.Run("large_payloads_are_gzipped", func(t *testing.T) {
		got := captureRequest(t, large)
		if !bytes.HasPrefix(got.data, []byte{0x1f, 0x8b}) || got.headers.Get("Content-Encoding") != "gzip" {
			t.Fatal("not gzipped")
		}
		if got.headers.Get("User-Agent") != "icebath-agent-shared-metrics/1" || got.headers.Get("Content-Type") != "application/json" {
			t.Fatal(got.headers)
		}
	})
	t.Run("gzip_is_deterministic_across_time", func(t *testing.T) {
		first := captureRequest(t, large).data
		time.Sleep(1100 * time.Millisecond)
		if second := captureRequest(t, large).data; !bytes.Equal(first, second) {
			t.Fatal("gzip output changed between seconds")
		}
	})
	t.Run("small_payloads_are_sent_plain", func(t *testing.T) {
		payload := []byte(`{"small": true}`)
		got := captureRequest(t, payload)
		if !bytes.Equal(got.data, payload) || got.headers.Get("Content-Encoding") != "" {
			t.Fatal("small payload altered")
		}
	})
	t.Run("http_error_status_is_a_response_with_retry_after", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			w.Write([]byte("slow down"))
		}))
		defer srv.Close()
		response, err := post(srv.URL, []byte("{}"), 5*time.Second)
		if err != nil || response.Status != 429 || deref(response.RetryAfter) != "7" || response.Body != "slow down" {
			t.Fatal(err, response)
		}
	})
}
