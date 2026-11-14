package observability

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

var windowsT0 = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func wdt(days, hours int) time.Time {
	return windowsT0.Add(time.Duration(days)*24*time.Hour + time.Duration(hours)*time.Hour)
}

func wts(days, hours int) string { return isoformat(wdt(days, hours)) }

func windowsStore(t *testing.T) *SharedMetricsStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSharedMetricsStore(context.Background(), filepath.Join(dir, "m.db"), filepath.Join(dir, "o"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func addWindowed(t *testing.T, s *SharedMetricsStore, pid, start, end string) {
	t.Helper()
	storeTxn(t, s, func(conn state.SQLiteConn) error {
		ctx := context.Background()
		if _, err := conn.ExecContext(ctx, "INSERT INTO package_outbox(package_id, period_start, period_end,"+
			" payload_json, created_at, exported_at) VALUES (?, ?, ?, ?, ?, ?)", pid, start, end,
			fmt.Sprintf(`{"package_id": %q}`, pid), end, end); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO consent_marks(name, stamp) VALUES ('data', ?)
			ON CONFLICT(name) DO UPDATE SET stamp = MAX(stamp, excluded.stamp)`, end)
		return err
	})
}

func observe(t *testing.T, s *SharedMetricsStore, sendEnabled bool, when time.Time) {
	t.Helper()
	storeTxn(t, s, func(conn state.SQLiteConn) error {
		return ReconcileSendConsent(context.Background(), conn, sendEnabled, when)
	})
}

func eligible(t *testing.T, s *SharedMetricsStore) []string {
	t.Helper()
	ids := storeColumn(t, s, "SELECT package_id FROM package_outbox WHERE "+ConsentGateSQL)
	sort.Strings(ids)
	return ids
}

func windows(t *testing.T, s *SharedMetricsStore) []string {
	t.Helper()
	var out []string
	err := s.connection(context.Background(), busyTimeoutMS, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(context.Background(), "SELECT opened_at, last_confirmed_at, closed_at"+
			" FROM send_consent_windows ORDER BY opened_at")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var opened, confirmed string
			var closed *string
			rows.Scan(&opened, &confirmed, &closed)
			out = append(out, opened+"|"+confirmed+"|"+deref(closed))
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func withPrefix(ids []string, prefix string) []string {
	var out []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	return out
}

func TestRefusedWindowIsNeverReleased(t *testing.T) {
	t.Run("on_off_on_with_realistic_interleaving", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(0, 0))
		for n := 0; n < 5; n++ {
			addWindowed(t, s, fmt.Sprintf("d%02d", n), wts(n, 0), wts(n+1, 0))
			observe(t, s, true, wdt(n+1, 0))
		}
		observe(t, s, false, wdt(5, 0))
		for n := 5; n < 10; n++ {
			addWindowed(t, s, fmt.Sprintf("d%02d", n), wts(n, 0), wts(n+1, 0))
		}
		observe(t, s, true, wdt(10, 0))
		for n := 10; n < 15; n++ {
			addWindowed(t, s, fmt.Sprintf("d%02d", n), wts(n, 0), wts(n+1, 0))
			observe(t, s, true, wdt(n+1, 0))
		}
		got := strings.Join(eligible(t, s), ",")
		if got != "d00,d01,d02,d03,d04,d10,d11,d12,d13,d14" {
			t.Fatal(got)
		}
	})
	t.Run("hand_edit_with_a_90_day_silent_gap", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(0, 0))
		addWindowed(t, s, "consented", wts(0, 1), wts(0, 2))
		observe(t, s, true, wdt(0, 6))
		for n := 1; n < 90; n += 10 {
			addWindowed(t, s, fmt.Sprintf("REFUSED-d%d", n), wts(n, 0), wts(n, 1))
		}
		observe(t, s, false, wdt(90, 0))
		observe(t, s, true, wdt(91, 0))
		observe(t, s, true, wdt(92, 0))
		ids := eligible(t, s)
		if len(withPrefix(ids, "REFUSED")) != 0 || len(withPrefix(ids, "consented")) != 1 {
			t.Fatal(ids)
		}
	})
}

func TestConsentClockAdversaries(t *testing.T) {
	poison := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Run("forward_poison_then_revoke_releases_nothing", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(0, 0))
		observe(t, s, true, poison)
		observe(t, s, false, wdt(1, 0))
		for n := 2; n < 10; n++ {
			addWindowed(t, s, fmt.Sprintf("REFUSED-%d", n), wts(n, 0), wts(n, 2))
		}
		if leaked := withPrefix(eligible(t, s), "REFUSED"); len(leaked) != 0 {
			t.Fatal(leaked)
		}
	})
	t.Run("forward_poison_cannot_wedge_consent_forever", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(0, 0))
		observe(t, s, true, poison)
		horizon := MaxObsAdvanceSeconds / 86400
		if stamp := storeScalar[string](t, s, "SELECT stamp FROM consent_marks WHERE name = 'obs'"); stamp > wts(horizon, 0) {
			t.Fatal(stamp)
		}
		addWindowed(t, s, "post-glitch", wts(horizon+1, 0), wts(horizon+1, 4))
		observe(t, s, true, wdt(horizon+2, 0))
		if len(withPrefix(eligible(t, s), "post-glitch")) != 1 {
			t.Fatal("consent wedged after a forward glitch")
		}
	})
	t.Run("rollback_at_re_enable_releases_nothing", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(0, 0))
		observe(t, s, true, wdt(5, 0))
		observe(t, s, false, wdt(5, 0))
		for n := 1; n < 4; n++ {
			addWindowed(t, s, fmt.Sprintf("REFUSED-%d", n), wts(5, n), wts(5, n+1))
		}
		observe(t, s, true, wdt(-12, 0))
		observe(t, s, true, wdt(-11, 0))
		if during := withPrefix(eligible(t, s), "REFUSED"); len(during) != 0 {
			t.Fatal(during)
		}
		observe(t, s, true, wdt(20, 0))
		observe(t, s, true, wdt(21, 0))
		if after := withPrefix(eligible(t, s), "REFUSED"); len(after) != 0 {
			t.Fatal(after)
		}
	})
	t.Run("recovery_does_not_wedge_future_sending", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(0, 0))
		observe(t, s, false, wdt(5, 0))
		observe(t, s, true, wdt(-12, 0))
		observe(t, s, true, wdt(20, 0))
		addWindowed(t, s, "post-recovery", wts(21, 0), wts(21, 4))
		observe(t, s, true, wdt(22, 0))
		if len(withPrefix(eligible(t, s), "post-recovery")) != 1 {
			t.Fatal(eligible(t, s))
		}
	})
}

func TestConsentSubDayGranularity(t *testing.T) {
	s := windowsStore(t)
	observe(t, s, true, wdt(0, 0))
	observe(t, s, true, wdt(10, 9))
	observe(t, s, false, wdt(10, 9))
	observe(t, s, true, wdt(10, 18))
	observe(t, s, true, wdt(11, 2))
	addWindowed(t, s, "halfday", wts(10, 0), wts(11, 0))
	if len(withPrefix(eligible(t, s), "halfday")) != 0 {
		t.Fatal("a partly refused day was released")
	}
}

func TestConsentReconcilerProperties(t *testing.T) {
	t.Run("idempotent_under_replay", func(t *testing.T) {
		s := windowsStore(t)
		for i := 0; i < 4; i++ {
			observe(t, s, true, wdt(0, 0))
		}
		observe(t, s, false, wdt(2, 0))
		for i := 0; i < 5; i++ {
			observe(t, s, false, wdt(3, 0))
		}
		observe(t, s, true, wdt(4, 0))
		for i := 0; i < 3; i++ {
			observe(t, s, true, wdt(5, 0))
		}
		if w := windows(t, s); len(w) != 2 {
			t.Fatal(w)
		}
	})
	t.Run("the_observation_mark_is_monotonic", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(5, 0))
		observe(t, s, true, wdt(0, 0))
		if stamp := storeScalar[string](t, s, "SELECT stamp FROM consent_marks WHERE name = 'obs'"); stamp != wts(5, 0) {
			t.Fatal(stamp)
		}
	})
	t.Run("the_real_package_writer_advances_the_data_mark", func(t *testing.T) {
		s := windowsStore(t)
		yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		storeTxn(t, s, func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(context.Background(), "INSERT INTO counter_aggregates(period_start, metric_name,"+
				" icebath_version, os_family, architecture, install_method, dimensions_json, value, packaged_value)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", yesterday, "icebath.client.active", "0.0.0-test", "macos", "arm64",
				"git", "{}", 1, 0)
			return err
		})
		if len(mustDue(t, s)) == 0 {
			t.Fatal("yesterday's period was not exported")
		}
		if stamp := storeScalar[string](t, s, "SELECT stamp FROM consent_marks WHERE name = 'data'"); stamp < yesterday {
			t.Fatal(stamp)
		}
	})
	t.Run("the_gate_is_read_only", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(0, 0))
		before := strings.Join(windows(t, s), ";")
		for i := 0; i < 10; i++ {
			eligible(t, s)
		}
		if after := strings.Join(windows(t, s), ";"); after != before {
			t.Fatal(before, after)
		}
	})
	t.Run("no_window_fails_closed", func(t *testing.T) {
		s := windowsStore(t)
		addWindowed(t, s, "orphan", wts(0, 0), wts(1, 0))
		if ids := eligible(t, s); len(ids) != 0 {
			t.Fatal(ids)
		}
	})
	t.Run("fresh_package_waits_one_heartbeat_then_releases", func(t *testing.T) {
		s := windowsStore(t)
		observe(t, s, true, wdt(0, 0))
		addWindowed(t, s, "fresh", wts(0, 1), wts(0, 2))
		if ids := eligible(t, s); len(ids) != 0 {
			t.Fatal(ids)
		}
		observe(t, s, true, wdt(0, 3))
		if ids := strings.Join(eligible(t, s), ","); ids != "fresh" {
			t.Fatal(ids)
		}
	})
}
