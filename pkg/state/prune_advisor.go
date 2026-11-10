package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	PruneGiB = 1 << 30
	// Low is under 2 GiB free, or under 10 percent free while also under 20 GiB
	pruneLowFreeFloor      = 2 * PruneGiB
	pruneLowFreePercent    = 10.0
	pruneLowFreeHeadroom   = 20 * PruneGiB
	AutoPruneSnoozeSeconds = 7 * 86400
	autoPruneSnoozeFile    = ".auto_prune_prompt.json"
	AutoPruneEstimateFile  = ".auto_prune_estimate.json"
	AutoPruneTotalBytesTTL = 7 * 86400
	AutoPruneCLIAdviceTTL  = 86400
	AutoPruneNoticeKey     = "sessions.auto_prune"
	autoPruneSaveFailed    = "Could not save sessions.auto_prune; check disk space and config.yaml permissions."
)

var (
	autoPruneDiskUsage = pyfs.DiskUsage
	autoPruneAdvise    = AutoPruneAdvise
	autoPruneSpawn     = func(name string, fn func()) { go fn() }
)

// Size of state.db and the free and total bytes of its filesystem
type DiskSnapshot struct {
	DBBytes, FreeBytes, TotalBytes int64
}

// Reports a disk under 2 GiB free, or under 10 percent free while also under 20 GiB
func (d DiskSnapshot) Low() bool {
	if d.TotalBytes <= 0 {
		return false
	}
	freePct := float64(d.FreeBytes) * 100.0 / float64(d.TotalBytes)
	return d.FreeBytes < pruneLowFreeFloor || (freePct < pruneLowFreePercent && d.FreeBytes < pruneLowFreeHeadroom)
}

// Low disk snapshot with the reclaim estimate a prune of the retention window would give back
type PruneAdvice struct {
	Disk                            DiskSnapshot
	RetentionDays                   int64
	PrunableSessions, TotalSessions int64
	ReclaimBytes                    int64
	EstimateOK                      bool
	VacuumAfterPrune                bool
}

// Reports a prune freeing at least 1 GiB or enough to lift the disk out of the low band
func (a PruneAdvice) Helps() bool {
	if a.PrunableSessions == 0 {
		return false
	}
	lifted := DiskSnapshot{a.Disk.DBBytes - a.ReclaimBytes, a.Disk.FreeBytes + a.ReclaimBytes, a.Disk.TotalBytes}
	return a.ReclaimBytes >= PruneGiB || !lifted.Low()
}

// Home the advisor reads, the context home when none is given
func pruneAdvisorHome(ctx context.Context, h string) string {
	if h != "" {
		return h
	}
	return home.Dir(ctx)
}

// Seconds since the epoch as a float like time.time
func pruneNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// State.db size plus free and total bytes of its filesystem, nil when either is unreadable
func AutoPruneDiskSnapshot(ctx context.Context, h string) *DiskSnapshot {
	db := filepath.Join(pruneAdvisorHome(ctx, h), "state.db")
	st, err := os.Stat(db)
	if err != nil {
		return nil
	}
	total, _, free, err := autoPruneDiskUsage(filepath.Dir(db))
	if err != nil {
		return nil
	}
	return &DiskSnapshot{DBBytes: st.Size(), FreeBytes: int64(free), TotalBytes: int64(total)}
}

// Sessions block of the home's effective user config, false when the file cannot be read
func autoPruneSessions(ctx context.Context, h string) (map[string]any, bool) {
	cfg, err := config.LoadUserEffectiveFailClosed(filepath.Join(pruneAdvisorHome(ctx, h), "config.yaml"), os.LookupEnv)
	if err != nil {
		slog.Debug(fmt.Sprintf("auto-prune advice: config unreadable: %s", err))
		return nil, false
	}
	sessions, ok := cfg["sessions"].(map[string]any)
	if !ok {
		return map[string]any{}, true
	}
	return sessions, true
}

// Retention days coerced the way maintenance coerces them, an unusable value reading as ninety
func autoPruneRetentionDays(sessions map[string]any) (int64, error) {
	raw, ok := sessions["retention_days"]
	if !ok {
		return 90, nil
	}
	n, err := pyint.Of(raw)
	var conv *pyint.Error
	if errors.As(err, &conv) && (conv.Kind == "TypeError" || conv.Kind == "ValueError") {
		return 90, nil
	}
	if err != nil {
		return 0, err
	}
	if n.Sign() < 0 {
		return 90, nil
	}
	if !n.IsInt64() {
		return 1<<63 - 1, nil
	}
	return n.Int64(), nil
}

// Reports a home whose user and managed config both leave sessions.auto_prune unset
func AutoPruneUndecided(ctx context.Context, h string) bool {
	sessions, ok := autoPruneSessions(ctx, h)
	if !ok {
		return false
	}
	_, set := sessions["auto_prune"]
	return !set
}

// Path of the later snooze record of a home
func autoPruneSnoozePath(ctx context.Context, h string) string {
	return filepath.Join(pruneAdvisorHome(ctx, h), autoPruneSnoozeFile)
}

// Reports a later answer whose snooze window has not passed, now defaulting to the clock
func AutoPruneSnoozed(ctx context.Context, h string, now float64) bool {
	data, err := os.ReadFile(autoPruneSnoozePath(ctx, h))
	if err != nil {
		return false
	}
	doc, err := pyjson.LoadsBytes(trimUTF8BOM(data))
	if err != nil {
		return false
	}
	obj, ok := doc.(*pyjson.Object)
	if !ok {
		return false
	}
	until, err := pyint.Float(obj.GetOr("snoozed_until", int64(0)))
	if err != nil {
		return false
	}
	if now == 0 {
		now = pruneNow()
	}
	return now < until
}

// Records a later answer that holds for the given seconds
func AutoPruneSnooze(ctx context.Context, h string, seconds float64) error {
	record := pyjson.NewObject()
	record.Set("snoozed_until", pruneNow()+seconds)
	return config.AtomicJSONWrite(ctx, autoPruneSnoozePath(ctx, h), record)
}

// Text with a leading UTF-8 byte order mark removed, the utf-8-sig decode
func trimUTF8BOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// Snapshot of a low disk on a home that neither decided nor snoozed, nil otherwise
func AutoPruneShouldAsk(ctx context.Context, h string) *DiskSnapshot {
	snap := AutoPruneDiskSnapshot(ctx, h)
	if snap == nil || !snap.Low() {
		return nil
	}
	if !AutoPruneUndecided(ctx, h) || AutoPruneSnoozed(ctx, h, 0) {
		return nil
	}
	return snap
}

// Truncated quotient of a times b over c computed exactly, the int of a Python true division
func exactScaled(a, b, c int64) int64 {
	q := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(a), big.NewInt(b)), big.NewInt(c))
	f, _ := q.Float64()
	return int64(f)
}

// Integer column of a one row aggregate, zero when the row is absent
func pruneRowInt(row *pyjson.Object, key string) int64 {
	if row == nil {
		return 0
	}
	v, _ := row.Get(key)
	n, err := pyint.Of(v)
	if err != nil || !n.IsInt64() {
		return 0
	}
	return n.Int64()
}

// Reclaim figures of a read only store, an error when the store could not be read
func autoPruneEstimate(ctx context.Context, h string, retention int64, snap *DiskSnapshot) (prunable, total, prunableBytes, totalBytes int64, err error) {
	db, err := Open(filepath.Join(pruneAdvisorHome(ctx, h), "state.db"), Options{ReadOnly: true})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer db.Close()
	days := float64(retention)
	where, params, err := pruneWhere(&days, "", PruneFilters{}, true)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	counts, err := db.readOne(ctx, "SELECT (SELECT COUNT(*) FROM sessions s WHERE "+where+") AS prunable, (SELECT COUNT(*) FROM sessions) AS total", params...)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	prunable, total = pruneRowInt(counts, "prunable"), pruneRowInt(counts, "total")
	if prunable == 0 {
		return prunable, total, 0, 0, nil
	}
	rowBytes, err := autoPruneRowBytesSQL(ctx, db)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	share, err := db.readOne(ctx, "SELECT COALESCE(SUM("+rowBytes+"), 0) AS n FROM messages m WHERE m.session_id IN (SELECT s.id FROM sessions s WHERE "+where+")", params...)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	prunableBytes = pruneRowInt(share, "n")
	cached, ok := autoPruneCachedTotalBytes(ctx, h, snap.DBBytes)
	if ok {
		totalBytes = cached
	} else {
		all, err := db.readOne(ctx, "SELECT COALESCE(SUM("+rowBytes+"), 0) AS n FROM messages m")
		if err != nil {
			return 0, 0, 0, 0, err
		}
		totalBytes = pruneRowInt(all, "n")
		autoPruneStoreEstimate(ctx, h, "total_bytes", totalBytes, "total_at_db_bytes", snap.DBBytes)
	}
	return prunable, total, prunableBytes, max(totalBytes, prunableBytes), nil
}

// Low disk advice with the reclaim estimate, opening state.db read only, nil when the gate stays closed
func AutoPruneAdvise(ctx context.Context, h string) (*PruneAdvice, error) {
	snap := AutoPruneShouldAsk(ctx, h)
	if snap == nil {
		return nil, nil
	}
	sessions, _ := autoPruneSessions(ctx, h)
	if sessions == nil {
		sessions = map[string]any{}
	}
	retention, err := autoPruneRetentionDays(sessions)
	if err != nil {
		return nil, err
	}
	estimateOK := true
	prunable, total, prunableBytes, totalBytes, err := autoPruneEstimate(ctx, h, retention, snap)
	if err != nil {
		slog.Debug(fmt.Sprintf("auto-prune advice: reclaim estimate unavailable: %s", err))
		prunable, total, prunableBytes, totalBytes, estimateOK = 0, 0, 0, 0, false
	}
	reclaim := int64(0)
	if totalBytes != 0 {
		reclaim = exactScaled(snap.DBBytes, prunableBytes, totalBytes)
	}
	advice := &PruneAdvice{Disk: *snap, RetentionDays: retention, PrunableSessions: prunable, TotalSessions: total,
		ReclaimBytes: reclaim, EstimateOK: estimateOK, VacuumAfterPrune: vacuumAfterPrune(sessions)}
	if estimateOK {
		stored := pyjson.NewObject()
		for _, kv := range [][2]any{{"retention_days", retention}, {"prunable", prunable}, {"total", total},
			{"db_bytes", snap.DBBytes}, {"reclaim_bytes", reclaim}} {
			stored.Set(kv[0].(string), kv[1])
		}
		autoPruneStoreEstimate(ctx, h, "advice", stored)
	}
	return advice, nil
}

// Vacuum after prune setting, true when absent
func vacuumAfterPrune(sessions map[string]any) bool {
	v, ok := sessions["vacuum_after_prune"]
	return !ok || pyrepr.Truthy(v)
}

// Estimate sidecar of a home, empty when missing, unreadable or not an object
func autoPruneReadEstimate(ctx context.Context, h string) *pyjson.Object {
	data, err := os.ReadFile(filepath.Join(pruneAdvisorHome(ctx, h), AutoPruneEstimateFile))
	if err != nil {
		return pyjson.NewObject()
	}
	doc, err := pyjson.LoadsBytes(trimUTF8BOM(data))
	if err != nil {
		return pyjson.NewObject()
	}
	if obj, ok := doc.(*pyjson.Object); ok {
		return obj
	}
	return pyjson.NewObject()
}

// Merges key and value pairs, each stamped with the time, into the estimate sidecar, a failed write only logged
func autoPruneStoreEstimate(ctx context.Context, h string, pairs ...any) {
	data := autoPruneReadEstimate(ctx, h)
	now := pruneNow()
	for i := 0; i+1 < len(pairs); i += 2 {
		key := pairs[i].(string)
		data.Set(key, pairs[i+1])
		data.Set(key+"_at", now)
	}
	if err := config.AtomicJSONWrite(ctx, filepath.Join(pruneAdvisorHome(ctx, h), AutoPruneEstimateFile), data); err != nil {
		slog.Debug(fmt.Sprintf("auto-prune advice: estimate not cached: %s", err))
	}
}

// Week old all rows byte total scaled to the current file size, false to measure again
func autoPruneCachedTotalBytes(ctx context.Context, h string, dbBytes int64) (int64, bool) {
	data := autoPruneReadEstimate(ctx, h)
	rawTotal, ok1 := data.Get("total_bytes")
	rawAt, ok2 := data.Get("total_bytes_at")
	rawBase, ok3 := data.Get("total_at_db_bytes")
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	total, err1 := pyint.Of(rawTotal)
	at, err2 := pyint.Float(rawAt)
	base, err3 := pyint.Of(rawBase)
	if err1 != nil || err2 != nil || err3 != nil || !total.IsInt64() || !base.IsInt64() {
		return 0, false
	}
	if pruneNow()-at > AutoPruneTotalBytesTTL || base.Sign() <= 0 || total.Sign() <= 0 {
		return 0, false
	}
	return exactScaled(total.Int64(), dbBytes, base.Int64()), true
}

// Advice from the last stored estimate of the same retention scaled to the live disk, and whether it is stale
func AutoPruneCachedAdvice(ctx context.Context, h string) (*PruneAdvice, bool, error) {
	snap := AutoPruneShouldAsk(ctx, h)
	if snap == nil {
		return nil, false, nil
	}
	sessions, _ := autoPruneSessions(ctx, h)
	if sessions == nil {
		sessions = map[string]any{}
	}
	retention, err := autoPruneRetentionDays(sessions)
	if err != nil {
		return nil, false, err
	}
	stored := autoPruneReadEstimate(ctx, h)
	raw, _ := stored.Get("advice")
	est, ok := raw.(*pyjson.Object)
	field := func(key string) (*big.Int, error) {
		v, present := est.Get(key)
		if !present {
			return nil, &pyint.Error{Kind: "KeyError", Msg: key}
		}
		return pyint.Of(v)
	}
	soft := func(err error) bool {
		var conv *pyint.Error
		return errors.As(err, &conv) && conv.Kind != "OverflowError"
	}
	if !ok {
		return nil, true, nil
	}
	days, err := field("retention_days")
	if err != nil {
		if soft(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	if days.Cmp(big.NewInt(retention)) != 0 {
		return nil, true, nil
	}
	atRaw, present := stored.Get("advice_at")
	if !present {
		return nil, true, nil
	}
	at, err := pyint.Float(atRaw)
	if err != nil {
		return nil, true, nil
	}
	stale := pruneNow()-at > AutoPruneCLIAdviceTTL
	values := map[string]*big.Int{}
	for _, key := range []string{"reclaim_bytes", "db_bytes", "prunable", "total"} {
		n, err := field(key)
		if err != nil {
			if soft(err) {
				return nil, true, nil
			}
			return nil, false, err
		}
		values[key] = n
	}
	divisor := values["db_bytes"]
	if divisor.Cmp(big.NewInt(1)) < 0 {
		divisor = big.NewInt(1)
	}
	reclaim := new(big.Int).Div(new(big.Int).Mul(values["reclaim_bytes"], big.NewInt(snap.DBBytes)), divisor)
	if reclaim.Cmp(big.NewInt(snap.DBBytes)) > 0 {
		reclaim = big.NewInt(snap.DBBytes)
	}
	return &PruneAdvice{Disk: *snap, RetentionDays: retention, PrunableSessions: values["prunable"].Int64(),
		TotalSessions: values["total"].Int64(), ReclaimBytes: reclaim.Int64(), EstimateOK: true,
		VacuumAfterPrune: vacuumAfterPrune(sessions)}, stale, nil
}

// Stored size of one messages row, the byte length of every text or blob column of the live schema
func autoPruneRowBytesSQL(ctx context.Context, db *SessionDB) (string, error) {
	rows, err := db.readAll(ctx, "PRAGMA table_info(messages)")
	if err != nil {
		return "", err
	}
	var parts []string
	for _, r := range rows {
		typ, _ := r.Get("type")
		switch pystr.Upper(pyrepr.Str(typ)) {
		case "TEXT", "BLOB", "":
			name, _ := r.Get("name")
			parts = append(parts, `COALESCE(LENGTH(CAST(m."`+pyrepr.Str(name)+`" AS BLOB)), 0)`)
		}
	}
	if len(parts) == 0 {
		return "0", nil
	}
	return strings.Join(parts, " + "), nil
}

// Human size in GB at or above 1 GiB and MB below
func pruneGB(n int64) string {
	if n >= PruneGiB {
		return fmt.Sprintf("%.1f GB", float64(n)/PruneGiB)
	}
	return fmt.Sprintf("%.0f MB", float64(max(n, 0))/(1024*1024))
}

// Count of sessions with the noun in the right number
func pruneSessions(n int64) string {
	if n == 1 {
		return fmt.Sprintf("%d session", n)
	}
	return fmt.Sprintf("%d sessions", n)
}

// Plain text prompt every surface shares, its last line naming the commands under the prefix
func AutoPruneAdviceLines(advice PruneAdvice, commandPrefix string) []string {
	d := advice.Disk
	lines := []string{
		fmt.Sprintf("Low disk space: %s free of %s. Your session history (state.db) uses %s.", pruneGB(d.FreeBytes), pruneGB(d.TotalBytes), pruneGB(d.DBBytes)),
		fmt.Sprintf("Auto-prune is off. Turning it on deletes ended sessions inactive for more than %d days, once a day (pinned and open sessions are kept).", advice.RetentionDays),
	}
	switch {
	case !advice.EstimateOK:
		lines = append(lines, "Could not read state.db to estimate how much pruning would free.")
	case advice.PrunableSessions == 0:
		held := "there is no session history to delete"
		if advice.TotalSessions != 0 {
			held = fmt.Sprintf("none of your %s is older than %d days", pruneSessions(advice.TotalSessions), advice.RetentionDays)
		}
		lines = append(lines, "Pruning will not help right now: "+held+". Something else is filling this disk.")
	case !advice.Helps():
		lines = append(lines, fmt.Sprintf("Pruning would remove only about %s of history (%s), which will not fix this disk on its own.", pruneGB(advice.ReclaimBytes), pruneSessions(advice.PrunableSessions)))
	default:
		lines = append(lines, fmt.Sprintf("Pruning would remove about %s of history (%d of %s).", pruneGB(advice.ReclaimBytes), advice.PrunableSessions, pruneSessions(advice.TotalSessions)))
	}
	if advice.EstimateOK && advice.PrunableSessions != 0 {
		lines = append(lines, "Deleted history frees space inside state.db; to give it back to the disk, run `icebath sessions optimize` with other Icebath windows closed.")
	}
	p := commandPrefix
	lines = append(lines, fmt.Sprintf("%sautoprune on to enable, %sautoprune off to keep everything (never asked again), %sautoprune later to be reminded in a week.", p, p, p))
	return lines
}

// Notification payload for the TUI and Desktop, a short status text with the full question as detail
func AutoPruneNoticePayload(advice PruneAdvice) *pyjson.Object {
	payload := pyjson.NewObject()
	payload.Set("text", fmt.Sprintf("⚠ Low disk (%s free) · /autoprune on|off|later", pruneGB(advice.Disk.FreeBytes)))
	payload.Set("detail", strings.Join(AutoPruneAdviceLines(advice, "/"), "\n"))
	payload.Set("level", "warn")
	payload.Set("kind", "sticky")
	payload.Set("ttl_ms", nil)
	payload.Set("key", AutoPruneNoticeKey)
	payload.Set("id", AutoPruneNoticeKey)
	return payload
}

// Reports a question settled at this moment, a saved answer or a live later snooze
func AutoPruneAnswered(ctx context.Context, h string) bool {
	return !AutoPruneUndecided(ctx, h) || AutoPruneSnoozed(ctx, h, 0)
}

// Reports a home due for the local notice, again once a full snooze window passed since it was shown
func AutoPruneNoticeDue(ctx context.Context, h string, shown map[string]float64) bool {
	if AutoPruneShouldAsk(ctx, h) == nil {
		return false
	}
	last, ok := shown[pruneAdvisorHome(ctx, h)]
	return !ok || pruneNow()-last >= AutoPruneSnoozeSeconds
}

// Reports an administrator pinning sessions.auto_prune
func autoPruneManaged() bool { return config.IsKeyManaged("sessions.auto_prune") }

// Saves the answer as an explicit sessions.auto_prune, false when the save failed or a managed value wins
func AutoPruneDecide(ctx context.Context, enable bool, h string) bool {
	if autoPruneManaged() {
		return false
	}
	path := filepath.Join(pruneAdvisorHome(ctx, h), "config.yaml")
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		slog.Warn(fmt.Sprintf("Could not save sessions.auto_prune: %s", err))
		return false
	}
	if err := config.WriteConfigKey(ctx, path, "sessions.auto_prune", enable); err != nil {
		slog.Warn(fmt.Sprintf("Could not save sessions.auto_prune: %s", err))
		return false
	}
	_ = os.Remove(autoPruneSnoozePath(ctx, h))
	sessions, _ := autoPruneSessions(ctx, h)
	return pyrepr.Truthy(sessions["auto_prune"]) == enable
}

// Reply of the autoprune command with on, off, later or status, the one handler every surface shares
func AutoPruneCommand(ctx context.Context, args, h string) (string, error) {
	arg := pystr.Lower(pystr.Strip(args))
	sessions, _ := autoPruneSessions(ctx, h)
	if sessions == nil {
		sessions = map[string]any{}
	}
	state := "off"
	if pyrepr.Truthy(sessions["auto_prune"]) {
		state = "on"
	}
	switch arg {
	case "on", "off", "enable", "disable", "yes", "no", "later", "snooze":
		if autoPruneManaged() {
			return "Auto-prune is " + state + ", set by your administrator; it cannot be changed here.", nil
		}
	}
	switch arg {
	case "on", "enable", "yes":
		if AutoPruneDecide(ctx, true, h) {
			return "Auto-prune is on: ended sessions inactive past retention_days are deleted once a day.", nil
		}
		return autoPruneSaveFailed, nil
	case "off", "disable", "no":
		if AutoPruneDecide(ctx, false, h) {
			return "Auto-prune is off and you will not be asked again. `icebath sessions prune` still works any time.", nil
		}
		return autoPruneSaveFailed, nil
	case "later", "snooze":
		if err := AutoPruneSnooze(ctx, h, AutoPruneSnoozeSeconds); err != nil {
			return "", err
		}
		return "OK, I'll ask again in a week if the disk is still low.", nil
	}
	chosen := "your setting"
	if autoPruneManaged() {
		chosen = "set by your administrator"
	} else if _, set := sessions["auto_prune"]; !set {
		chosen = "default"
	}
	disk := ""
	if snap := AutoPruneDiskSnapshot(ctx, h); snap != nil {
		low := ""
		if snap.Low() {
			low = " (low)"
		}
		disk = fmt.Sprintf(" state.db %s; %s free of %s%s.", pruneGB(snap.DBBytes), pruneGB(snap.FreeBytes), pruneGB(snap.TotalBytes), low)
	}
	retention, err := autoPruneRetentionDays(sessions)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Auto-prune is %s (%s), retention %d days.%s Usage: /autoprune on | off | later", state, chosen, retention, disk), nil
}

// Startup offer of the classic CLI asked from the stored estimate, the decision or nil for no answer or later
func AutoPruneOfferInteractive(ctx context.Context, stdout io.Writer, cprint func(string), ask func(prompt string) (string, error)) (*bool, error) {
	if AutoPruneShouldAsk(ctx, "") == nil {
		return nil, nil
	}
	advice, stale, err := AutoPruneCachedAdvice(ctx, "")
	if err != nil {
		return nil, err
	}
	if stale {
		advise := autoPruneAdvise
		autoPruneSpawn("auto-prune-estimate", func() { _, _ = advise(ctx, "") })
	}
	if advice == nil {
		return nil, nil
	}
	lines := AutoPruneAdviceLines(*advice, "/")
	cprint("")
	cprint("☤ " + lines[0])
	for _, line := range lines[1 : len(lines)-1] {
		cprint("  " + line)
	}
	answer, err := ask("  Enable auto-prune? [y = enable / n = never prune, don't ask again / Enter = remind me later]: ")
	if err != nil {
		fmt.Fprintln(stdout)
		return nil, nil
	}
	answer = pystr.Lower(pystr.Strip(answer))
	switch answer {
	case "y", "yes", "n", "no":
		enable := strings.HasPrefix(answer, "y")
		if !AutoPruneDecide(ctx, enable, "") {
			cprint("  " + autoPruneSaveFailed)
			return nil, nil
		}
		if enable {
			cprint("  ✓ sessions.auto_prune: true")
		} else {
			cprint("  ✓ sessions.auto_prune: false. You won't be asked again.")
		}
		return &enable, nil
	}
	if err := AutoPruneSnooze(ctx, "", AutoPruneSnoozeSeconds); err != nil {
		return nil, err
	}
	cprint("  Later. /autoprune on|off decides it any time.")
	return nil, nil
}
