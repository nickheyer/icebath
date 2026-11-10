package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Home holding one recent ended session, bound as the process home
func pruneAdvisorFixture(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, h)
	db, err := Open(filepath.Join(h, "state.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	pruneAdvisorSession(t, db, "recent", "user", "hi", 1)
	db.Close()
	return h
}

// Creates an ended cli session holding count copies of one message
func pruneAdvisorSession(t *testing.T, db *SessionDB, id, role, content string, count int) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateSession(NewSession{ID: id, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if _, err := db.AppendMessage(ctx, id, map[string]any{"role": role, "content": content}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EndSession(id, "user_exit"); err != nil {
		t.Fatal(err)
	}
}

// Disk usage stand in reporting free of total GiB
func pruneAdvisorDisk(t *testing.T, freeGB, totalGB uint64) {
	t.Helper()
	prev := autoPruneDiskUsage
	autoPruneDiskUsage = func(string) (uint64, uint64, uint64, error) {
		return totalGB * PruneGiB, (totalGB - freeGB) * PruneGiB, freeGB * PruneGiB, nil
	}
	t.Cleanup(func() { autoPruneDiskUsage = prev })
}

// Moves the matching sessions and their messages two hundred days into the past
func pruneAdvisorBackdate(t *testing.T, dbPath, likeID string) {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	old := pruneNow() - 200*86400
	if _, err := conn.Exec("UPDATE sessions SET started_at=?, ended_at=?, last_activity_at=? WHERE id LIKE ?", old, old, old, likeID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("UPDATE messages SET timestamp=? WHERE session_id LIKE ?", old, likeID); err != nil {
		t.Fatal(err)
	}
}

// Spawn stand in recording each background function instead of running it
func pruneAdvisorSpawns(t *testing.T) *[]func() {
	t.Helper()
	started := &[]func(){}
	prev := autoPruneSpawn
	autoPruneSpawn = func(_ string, fn func()) { *started = append(*started, fn) }
	t.Cleanup(func() { autoPruneSpawn = prev })
	return started
}

func TestAsksOnlyWhenLowAndUndecidedAndEitherAnswerEndsIt(t *testing.T) {
	ctx := context.Background()
	h := pruneAdvisorFixture(t)
	pruneAdvisorDisk(t, 50, 100)
	if AutoPruneShouldAsk(ctx, h) != nil {
		t.Fatal("asked with plenty of space")
	}
	pruneAdvisorDisk(t, 1, 100)
	if AutoPruneShouldAsk(ctx, h) == nil {
		t.Fatal("silent on a low disk")
	}
	if err := AutoPruneSnooze(ctx, h, AutoPruneSnoozeSeconds); err != nil {
		t.Fatal(err)
	}
	if AutoPruneShouldAsk(ctx, h) != nil {
		t.Fatal("asked inside the snooze window")
	}
	if err := os.Remove(autoPruneSnoozePath(ctx, h)); err != nil {
		t.Fatal(err)
	}
	for _, answer := range []bool{false, true} {
		if !AutoPruneDecide(ctx, answer, h) {
			t.Fatalf("decide %v failed", answer)
		}
		data, err := os.ReadFile(filepath.Join(h, "config.yaml"))
		if err != nil || strings.Count(string(data), "auto_prune") != 1 {
			t.Fatalf("config %q %v", data, err)
		}
		if AutoPruneShouldAsk(ctx, h) != nil {
			t.Fatalf("asked after answering %v", answer)
		}
	}
}

func TestPromptSaysPruningWillNotHelpWhenNothingIsOldEnough(t *testing.T) {
	ctx := context.Background()
	h := pruneAdvisorFixture(t)
	pruneAdvisorDisk(t, 1, 100)
	advice, err := AutoPruneAdvise(ctx, h)
	if err != nil || advice == nil || advice.TotalSessions != 1 || advice.PrunableSessions != 0 {
		t.Fatalf("advice %+v %v", advice, err)
	}
	if advice.Helps() || !strings.Contains(strings.Join(AutoPruneAdviceLines(*advice, "/"), "\n"), "Pruning will not help") {
		t.Fatal(AutoPruneAdviceLines(*advice, "/"))
	}
	pruneAdvisorBackdate(t, filepath.Join(h, "state.db"), "%")
	advice, err = AutoPruneAdvise(ctx, h)
	if err != nil || advice.PrunableSessions != 1 {
		t.Fatalf("advice %+v %v", advice, err)
	}
	if strings.Contains(strings.Join(AutoPruneAdviceLines(*advice, "/"), "\n"), "will not help") {
		t.Fatal(AutoPruneAdviceLines(*advice, "/"))
	}
	t.Setenv("KEEP_DAYS", "0")
	for _, value := range []string{"0", `"0"`, "${KEEP_DAYS}"} {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("sessions:\n  retention_days: "+value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		advice, err := AutoPruneAdvise(ctx, h)
		if err != nil || advice.RetentionDays != 0 {
			t.Fatalf("%s: %+v %v", value, advice, err)
		}
	}
}

func TestAdminManagedSettingIsNeverAskedNorOverwritten(t *testing.T) {
	ctx := context.Background()
	h := pruneAdvisorFixture(t)
	managed := filepath.Join(t.TempDir(), "managed")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "config.yaml"), []byte("sessions:\n  auto_prune: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	pruneAdvisorDisk(t, 1, 100)
	if AutoPruneShouldAsk(ctx, h) != nil {
		t.Fatal("asked though an administrator decided")
	}
	reply, err := AutoPruneCommand(ctx, "off", h)
	if err != nil || !strings.Contains(reply, "administrator") {
		t.Fatalf("reply %q %v", reply, err)
	}
	if data, err := os.ReadFile(filepath.Join(h, "config.yaml")); err == nil && strings.Contains(string(data), "auto_prune") {
		t.Fatalf("config overwritten %q", data)
	}
}

func TestLaterReArmsTheLocalNoticeAfterTheSnoozeWindow(t *testing.T) {
	ctx := context.Background()
	h := pruneAdvisorFixture(t)
	pruneAdvisorDisk(t, 1, 100)
	shown := map[string]float64{}
	if !AutoPruneNoticeDue(ctx, h, shown) {
		t.Fatal("first notice not due")
	}
	shown[h] = pruneNow()
	if AutoPruneNoticeDue(ctx, h, shown) {
		t.Fatal("noticed twice in one window")
	}
	shown[h] = pruneNow() - AutoPruneSnoozeSeconds - 1
	if !AutoPruneNoticeDue(ctx, h, shown) {
		t.Fatal("not due a week later")
	}
}

func TestOnlyASettledQuestionMayWithdrawTheNotice(t *testing.T) {
	ctx := context.Background()
	h := pruneAdvisorFixture(t)
	pruneAdvisorDisk(t, 1, 100)
	if AutoPruneAnswered(ctx, h) {
		t.Fatal("answered before any reply")
	}
	for _, arg := range []string{"status", "bogus"} {
		if _, err := AutoPruneCommand(ctx, arg, h); err != nil {
			t.Fatal(err)
		}
	}
	if AutoPruneAnswered(ctx, h) {
		t.Fatal("status or a typo settled the question")
	}
	if _, err := AutoPruneCommand(ctx, "later", h); err != nil {
		t.Fatal(err)
	}
	if !AutoPruneAnswered(ctx, h) {
		t.Fatal("later did not settle the question")
	}
}

func TestEstimateWeightsByStoredBytesNotMessageCount(t *testing.T) {
	ctx := context.Background()
	h := filepath.Join(t.TempDir(), "skewed")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(h, "state.db")
	db, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		pruneAdvisorSession(t, db, "old"+strconv.Itoa(i), "user", "ok", 20)
	}
	for i := 0; i < 4; i++ {
		pruneAdvisorSession(t, db, "new"+strconv.Itoa(i), "assistant", strings.Repeat("x", 40_000), 5)
	}
	db.Close()
	pruneAdvisorBackdate(t, dbPath, "old%")
	pruneAdvisorDisk(t, 1, 100)
	advice, err := AutoPruneAdvise(ctx, h)
	if err != nil || advice.PrunableSessions != 40 {
		t.Fatalf("advice %+v %v", advice, err)
	}
	st, _ := os.Stat(dbPath)
	before := st.Size()
	installRetirementHooks(t)
	db, err = Open(dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	days := 90.0
	if _, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, _ = os.Stat(dbPath)
	shrink := before - st.Size()
	if float64(advice.ReclaimBytes) >= float64(before)*0.25 {
		t.Fatalf("reclaim %d of %d", advice.ReclaimBytes, before)
	}
	if math.Abs(float64(advice.ReclaimBytes-shrink)) >= float64(max(shrink, 64*1024))*1.0 {
		t.Fatalf("reclaim %d shrink %d", advice.ReclaimBytes, shrink)
	}
	if !strings.Contains(strings.Join(AutoPruneAdviceLines(*advice, "/"), "\n"), "icebath sessions optimize") {
		t.Fatal(AutoPruneAdviceLines(*advice, "/"))
	}
}

func TestCliStartupNeverReadsTheStoreAndAsksFromTheStoredEstimate(t *testing.T) {
	ctx := context.Background()
	h := pruneAdvisorFixture(t)
	pruneAdvisorDisk(t, 1, 100)
	started := pruneAdvisorSpawns(t)
	readStore := false
	prev := autoPruneAdvise
	autoPruneAdvise = func(context.Context, string) (*PruneAdvice, error) {
		readStore = true
		return nil, errors.New("startup read state.db")
	}
	t.Cleanup(func() { autoPruneAdvise = prev })
	var asked []string
	ask := func(q string) (string, error) { asked = append(asked, q); return "", nil }
	var stdout strings.Builder
	got, err := AutoPruneOfferInteractive(ctx, &stdout, func(string) {}, ask)
	if err != nil || got != nil || len(asked) != 0 || len(*started) != 1 || readStore {
		t.Fatalf("first launch %v %v asked %d started %d read %v", got, err, len(asked), len(*started), readStore)
	}
	(*started)[0]()
	if !readStore {
		t.Fatal("the background measurement is not the advisor")
	}
	if _, err := AutoPruneAdvise(ctx, h); err != nil {
		t.Fatal(err)
	}
	var out []string
	if _, err := AutoPruneOfferInteractive(ctx, &stdout, func(line string) { out = append(out, line) }, ask); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || !strings.Contains(strings.Join(out, "\n"), "Low disk space") {
		t.Fatalf("asked %d out %q", len(asked), out)
	}
}

// Estimate sidecar of a home decoded as plain JSON
func pruneAdvisorEstimate(t *testing.T, h string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h, AutoPruneEstimateFile))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFullScanTotalIsMeasuredOnceAndReused(t *testing.T) {
	ctx := context.Background()
	h := pruneAdvisorFixture(t)
	pruneAdvisorDisk(t, 1, 100)
	pruneAdvisorBackdate(t, filepath.Join(h, "state.db"), "%")
	first, err := AutoPruneAdvise(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	measured := pruneAdvisorEstimate(t, h)
	if measured["total_bytes"].(float64) <= 0 {
		t.Fatalf("measured %v", measured)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := AutoPruneAdvise(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if pruneAdvisorEstimate(t, h)["total_bytes_at"] != measured["total_bytes_at"] || second.ReclaimBytes != first.ReclaimBytes {
		t.Fatal("the full scan ran again")
	}
	stale := map[string]any{}
	for k, v := range measured {
		stale[k] = v
	}
	stale["total_bytes_at"] = pruneNow() - AutoPruneTotalBytesTTL - 1
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(h, AutoPruneEstimateFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AutoPruneAdvise(ctx, h); err != nil {
		t.Fatal(err)
	}
	if pruneAdvisorEstimate(t, h)["total_bytes_at"].(float64) <= stale["total_bytes_at"].(float64) {
		t.Fatal("a week old total was not measured again")
	}
}

func TestCliLaunchesFurtherApartThanTheEstimateTtlStillAsk(t *testing.T) {
	ctx := context.Background()
	h := pruneAdvisorFixture(t)
	pruneAdvisorDisk(t, 1, 100)
	if _, err := AutoPruneAdvise(ctx, h); err != nil {
		t.Fatal(err)
	}
	data := pruneAdvisorEstimate(t, h)
	data["advice_at"] = pruneNow() - AutoPruneCLIAdviceTTL - 3600
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(filepath.Join(h, AutoPruneEstimateFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	started := pruneAdvisorSpawns(t)
	var asked []string
	var stdout strings.Builder
	if _, err := AutoPruneOfferInteractive(ctx, &stdout, func(string) {}, func(q string) (string, error) {
		asked = append(asked, q)
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || len(*started) != 1 {
		t.Fatalf("asked %d started %d", len(asked), len(*started))
	}
}
