package observability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	// PendingInstallsDirname holds installer receipts waiting for a consented start
	PendingInstallsDirname = "pending_installs"
	// RecordedInstallsDirname holds the latches of receipts already counted
	RecordedInstallsDirname = "recorded_installs"
	recordedInstallsKeep    = 64
	// MaxReceiptAgeSeconds is the age past which a receipt is deleted unreported
	MaxReceiptAgeSeconds = 7 * 86_400
)

var (
	receiptIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	receiptKeys      = []string{"failed_stage", "failure_class", "finished_at", "id", "installer", "outcome", "started_at"}
)

// Directory of a home's parked installer receipts
func PendingInstallsDir(h string) string {
	return filepath.Join(h, "telemetry", "shared_metrics", PendingInstallsDirname)
}

// Removes every parked installer receipt of a home, ignoring errors
func PurgePendingInstalls(ctx context.Context, h string) { _ = os.RemoveAll(PendingInstallsDir(h)) }

var purgePendingInstalls = PurgePendingInstalls

// Positive non bool integer epoch of a receipt value
func receiptEpoch(value any) (*big.Int, bool) {
	var n *big.Int
	switch v := value.(type) {
	case int:
		n = big.NewInt(int64(v))
	case int64:
		n = big.NewInt(v)
	case *big.Int:
		n = v
	default:
		return nil, false
	}
	if n.Sign() <= 0 {
		return nil, false
	}
	return n, true
}

// Install run dimensions of one receipt, nil when anything is outside the contract
func InstallRunFields(receipt any) map[string]string {
	d, ok := asDict(receipt)
	if !ok || len(d) != len(receiptKeys) {
		return nil
	}
	for _, key := range receiptKeys {
		if _, has := d[key]; !has {
			return nil
		}
	}
	id, ok := d["id"].(string)
	if !ok || !receiptIDPattern.MatchString(id) {
		return nil
	}
	started, okStart := receiptEpoch(d["started_at"])
	finished, okEnd := receiptEpoch(d["finished_at"])
	if !okStart || !okEnd || finished.Cmp(started) < 0 {
		return nil
	}
	durationMS, _ := new(big.Float).SetInt(new(big.Int).Mul(new(big.Int).Sub(finished, started), big.NewInt(1000))).Float64()
	raw := map[string]any{"duration_bucket": UpdateDurationBucket(durationMS), "failed_stage": d["failed_stage"],
		"failure_class": d["failure_class"], "installer": d["installer"], "outcome": d["outcome"]}
	if !CounterDimensionsAreValid(InstallRunMetric, raw) {
		return nil
	}
	fields := map[string]string{}
	for k, v := range raw {
		fields[k] = v.(string)
	}
	succeeded := fields["outcome"] == "success"
	if succeeded != (fields["failed_stage"] == "none") || succeeded != (fields["failure_class"] == "none") {
		return nil
	}
	return fields
}

// Latch path that marks a receipt id as counted in a home
func recordedLatch(h, receiptID string) string {
	return filepath.Join(h, "telemetry", "shared_metrics", RecordedInstallsDirname, receiptID)
}

// Latches a receipt id after its row is saved and prunes all but the newest latches
func markRecorded(latch string) {
	dir := filepath.Dir(latch)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return
	}
	now := time.Now()
	if f, err := os.OpenFile(latch, os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
		_ = f.Close()
		_ = os.Chtimes(latch, now, now)
	} else {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type aged struct {
		path  string
		mtime time.Time
	}
	var all []aged
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return
		}
		all = append(all, aged{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].mtime.After(all[j].mtime) })
	for _, stale := range all[min(len(all), recordedInstallsKeep):] {
		_ = os.Remove(stale.path)
	}
}

// Today's package period start in the store's stamp format
func todayStart() string {
	now := utcNow()
	return isoformat(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC))
}

// Reports whether a row recorded now lands in a package the user agreed to send or keep local
func ConsentedDay(ctx context.Context, h string) bool {
	raw, err := config.ReadRawConfigReadonly(ctx, home.Dir(ctx))
	if err != nil {
		panic(err)
	}
	var cfg any = map[string]any{}
	if len(raw) > 0 {
		cfg = raw
	}
	send := ResolveSendConfig(cfg)
	if !send.Enabled {
		return false
	}
	if !send.Send {
		return true
	}
	root := filepath.Join(h, "telemetry", "shared_metrics")
	store, err := NewSharedMetricsStore(ctx, filepath.Join(root, "metrics.sqlite3"), filepath.Join(root, "outbox"))
	if err != nil {
		return false
	}
	found := false
	err = store.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		var one int
		err := conn.QueryRowContext(ctx,
			"SELECT 1 FROM send_consent_windows WHERE closed_at IS NULL AND opened_at <= ? LIMIT 1",
			todayStart()).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return err == nil && found
}

// Renames a receipt so exactly one reporter owns it without reading it first
func claimReceipt(path string) string {
	if strings.HasSuffix(filepath.Base(path), reportingSuffix) && claimerAlive(path) {
		return ""
	}
	base := strings.SplitN(filepath.Base(path), ".json", 2)[0]
	claimed := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.json.%d%s", base, os.Getpid(), reportingSuffix))
	if err := os.Rename(path, claimed); err != nil {
		return ""
	}
	return claimed
}

// Reports whether a receipt finished longer ago than the maximum receipt age
func staleReceipt(receipt map[string]any) bool {
	finished, _ := receiptEpoch(receipt["finished_at"])
	f, _ := new(big.Float).SetInt(finished).Float64()
	return float64(utcNow().UnixNano())/1e9-f > MaxReceiptAgeSeconds
}

// Records each parked installer receipt once on a consented day, never failing
func ReportPendingInstalls(ctx context.Context, h string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Pending install receipts not reported", "error", r)
		}
	}()
	dir := PendingInstallsDir(h)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return
	}
	if !relayInstrumentationEnabled(ctx) {
		return
	}
	reportable := ConsentedDay(ctx, h)
	entries, err := os.ReadDir(dir)
	if err != nil {
		panic(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	for _, name := range names {
		if strings.HasPrefix(name, ".") || !strings.Contains(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		claimed := claimReceipt(path)
		if claimed == "" {
			continue
		}
		var receipt any
		if record, ok := readRecord(claimed); ok {
			receipt = record
		}
		fields := InstallRunFields(receipt)
		d, isDict := asDict(receipt)
		if fields == nil || !isDict || staleReceipt(d) {
			SettleClaim(claimed, path, true)
			continue
		}
		latch := recordedLatch(h, d["id"].(string))
		if _, err := os.Stat(latch); err == nil {
			SettleClaim(claimed, path, true)
			continue
		}
		if !reportable {
			SettleClaim(claimed, path, false)
			continue
		}
		saved := EmitSaved(ctx, []Mark{{Name: InstallRunMark, Data: fields}}) == 1
		if saved {
			markRecorded(latch)
		}
		SettleClaim(claimed, path, saved)
	}
}

var reportPendingInstalls = ReportPendingInstalls

// Whether the inherited relay turn may create instrumentation, replaced by tests
var relayInstrumentationEnabled = tools.RelayInstrumentationEnabled
