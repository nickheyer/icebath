package observability

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	// FrictionDailyCap bounds friction and dislike rows per profile and UTC day in one process
	FrictionDailyCap = 50
	// OnboardingLatchDirname holds the once ever onboarding step latches of a profile
	OnboardingLatchDirname = "desktop_onboarding"
	// DailyReportStateKey is the rollup state key of reported Desktop usage days
	DailyReportStateKey = "desktop_daily_reported"
	// DailyReportWindowDays is how many past days a Desktop daily report may still land in
	DailyReportWindowDays = 8
)

// DailyActionRowsMax bounds the action entries one daily report reads
var DailyActionRowsMax = len(DesktopActionIDs) * len(DesktopActionVias)

var (
	desktopDayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	desktopLatchMu    sync.Mutex
	desktopDaily      = map[[3]string]int{}
	// Today's UTC date in ISO form, replaced by tests
	desktopUTCDay = func() string { return time.Now().UTC().Format(time.DateOnly) }
)

// Profile home the daily latches of this scope are keyed by
func desktopHome(ctx context.Context) string { return home.Dir(ctx) }

// Stripped lower case text of a string value, empty for anything else
func desktopWord(value any) string {
	if s, ok := value.(string); ok {
		return pystr.Lower(pystr.Strip(s))
	}
	return ""
}

// Claims one of a key's daily slots in this profile, false once the cap is reached
func claimDaily(ctx context.Context, key string, cap int, day string) bool {
	today := desktopUTCDay()
	if day == "" {
		day = today
	}
	slot := [3]string{desktopHome(ctx), day, key}
	desktopLatchMu.Lock()
	defer desktopLatchMu.Unlock()
	for k := range desktopDaily {
		if k[1] < today && k[1] != slot[1] {
			delete(desktopDaily, k)
		}
	}
	if desktopDaily[slot] >= cap {
		return false
	}
	desktopDaily[slot]++
	return true
}

// Contract area of a Desktop feature, settings_other for unknown settings pages
func FeatureArea(area any) string {
	word := desktopWord(area)
	if DesktopFeatureAreas.Has(word) {
		return word
	}
	if strings.HasPrefix(word, "settings_") {
		return "settings_other"
	}
	return "other"
}

// Friction kind and detail checked against its kind's set, false for an unknown kind
func FrictionPair(kind, detail any) (string, string, bool) {
	kindWord := desktopWord(kind)
	details, ok := DesktopFrictionDetails[kindWord]
	if !ok {
		return "", "", false
	}
	if detailWord := desktopWord(detail); details.Has(detailWord) {
		return kindWord, detailWord, true
	}
	return kindWord, "other", true
}

// Active profile store and client resource for rows that latch durably or land in a past period
func storeAndResource(ctx context.Context) (*SharedMetricsStore, map[string]string, error) {
	info, err := installVersionInfo()
	if err != nil {
		return nil, nil, err
	}
	store, err := NewSharedMetricsStore(ctx, "", "")
	if err != nil {
		return nil, nil, err
	}
	method, err := DetectClientInstallMethod(ctx)
	if err != nil {
		return nil, nil, err
	}
	return store, ClientResource(info.BaseVersion, pyplatform.System(), pyplatform.Machine(), method), nil
}

// Friction row under the daily cap, nil for an unknown kind or a spent cap
func frictionFields(ctx context.Context, kind, detail any) map[string]string {
	k, d, ok := FrictionPair(kind, detail)
	if !ok || !claimDaily(ctx, fmt.Sprintf("friction:%s:%s", k, d), FrictionDailyCap, "") {
		return nil
	}
	return map[string]string{"detail": d, "kind": k}
}

// Claims one onboarding step and event once per profile across processes with an exclusive latch file
func claimOnboarding(ctx context.Context, step, event string) bool {
	dir := filepath.Join(home.Dir(ctx), "telemetry", "shared_metrics", OnboardingLatchDirname)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		panic(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, step+"."+event), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if errors.Is(err, fs.ErrExist) {
		return false
	}
	if err != nil {
		panic(err)
	}
	_ = f.Close()
	return true
}

// Drops the profile's onboarding latches when collection turns off
func PurgeOnboardingLatches(ctx context.Context) {
	_ = os.RemoveAll(filepath.Join(home.Dir(ctx), "telemetry", "shared_metrics", OnboardingLatchDirname))
}

// Onboarding row of a known step and event claimed for the first time, nil otherwise
func onboardingFields(ctx context.Context, step, event any) map[string]string {
	stepWord, eventWord := desktopWord(step), desktopWord(event)
	if !DesktopOnboardingSteps.Has(stepWord) || !DesktopOnboardingEvents.Has(eventWord) {
		return nil
	}
	if !claimOnboarding(ctx, stepWord, eventWord) {
		return nil
	}
	return map[string]string{"event": eventWord, "step": stepWord}
}

// Non negative integer of a numeric client value, zero for anything else
func desktopCount(value any) int {
	switch v := value.(type) {
	case bool:
		return 0
	case int:
		return max(v, 0)
	case int64:
		return int(max(v, 0))
	case float64:
		if math.IsInf(v, 1) {
			panic("OverflowError: cannot convert float infinity to integer")
		}
		if v >= 0 {
			return int(v)
		}
	}
	if n, ok := pyInt(value); ok && n.Sign() >= 0 && n.IsInt64() {
		return int(n.Int64())
	}
	return 0
}

// Registry action id stripped but case kept, other when unknown
func ActionID(action any) string {
	word := ""
	if s, ok := action.(string); ok {
		word = pystr.Strip(s)
	}
	if DesktopActionIDs.Has(word) {
		return word
	}
	return "other"
}

// Dotted leaf paths of a nested config, an empty mapping counting as a leaf
func settingLeaves(cfg map[string]any, prefix string, leaves map[string]any) map[string]any {
	for key, value := range cfg {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if d, ok := asDict(value); ok && len(d) > 0 {
			settingLeaves(d, path, leaves)
		} else {
			leaves[path] = value
		}
	}
	return leaves
}

// Setting key and its direction against the default, other and none for a key off the settings schema
func settingDirection(ctx context.Context, key string) (string, string) {
	defaults := settingLeaves(config.Defaults(), "", map[string]any{})
	def, known := defaults[key]
	if !known || pystr.Len(key) > DesktopSettingKeyMaxLength {
		return "other", "none"
	}
	tree, err := config.LoadContext(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		panic(err)
	}
	var current any = map[string]any(tree)
	for _, part := range strings.Split(key, ".") {
		if d, ok := asDict(current); ok {
			current = d[part]
		} else {
			current = nil
		}
	}
	if pyrepr.Equal(current, def) {
		return key, "to_default"
	}
	return key, "away_from_default"
}

// Dislike row of a known signal under the daily cap, nil otherwise
func dislikeFields(ctx context.Context, signal, target, setting any) map[string]string {
	signalWord := desktopWord(signal)
	targets, ok := DesktopDislikeTargets[signalWord]
	if !ok {
		return nil
	}
	settingWord, direction, targetWord := "none", "none", ""
	if signalWord == "setting_off_default" {
		key := ""
		if s, ok := setting.(string); ok {
			key = pystr.Strip(s)
		}
		settingWord, direction = settingDirection(ctx, key)
		targetWord = "setting"
	} else {
		raw := ""
		if s, ok := target.(string); ok {
			raw = pystr.Strip(s)
		}
		if signalWord == "rage_click" && targets.Has(raw) {
			targetWord = raw
		} else {
			targetWord = desktopWord(raw)
		}
		if !targets.Has(targetWord) {
			targetWord = "other"
		}
	}
	if !claimDaily(ctx, "dislike:"+signalWord, FrictionDailyCap, "") {
		return nil
	}
	return map[string]string{"direction": direction, "setting": settingWord, "signal": signalWord, "target": targetWord}
}

// Mode use rows of one daily report, the first entry per used mode winning
func modeRows(modes, botCount any) []MarkRow {
	list, _ := modes.([]any)
	seen := map[string]bool{}
	var rows []MarkRow
	for _, entry := range list {
		d, _ := asDict(entry)
		mode, activeMS, messages := "", 0, 0
		if d != nil {
			mode = desktopWord(d["mode"])
		}
		if mode != "" {
			activeMS, messages = desktopCount(d["active_ms"]), desktopCount(d["messages_sent"])
		}
		if !DesktopModes.Has(mode) || seen[mode] || (activeMS == 0 && messages == 0) {
			continue
		}
		seen[mode] = true
		rows = append(rows, MarkRow{DesktopModeUseMark, map[string]string{
			"active_minutes_bucket": DesktopActiveMinutesBucket(activeMS),
			"bot_count_bucket":      SizeBucket(desktopCount(botCount)),
			"messages_sent_bucket":  SizeBucket(messages),
			"mode":                  mode,
		}})
	}
	return rows
}

// Action use rows of one daily report, counts summed per action and via in sorted order
func actionRows(actions any) []MarkRow {
	list, _ := actions.([]any)
	if len(list) > DailyActionRowsMax {
		list = list[:DailyActionRowsMax]
	}
	totals := map[[2]string]int{}
	for _, entry := range list {
		d, ok := asDict(entry)
		if !ok || !DesktopActionVias.Has(desktopWord(d["via"])) {
			continue
		}
		key := [2]string{ActionID(d["action"]), desktopWord(d["via"])}
		totals[key] += desktopCount(d["count"])
	}
	keys := slices.SortedFunc(maps.Keys(totals), func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	})
	var rows []MarkRow
	for _, key := range keys {
		if count := totals[key]; count != 0 {
			rows = append(rows, MarkRow{DesktopActionUseMark, map[string]string{"action": key[0],
				"count_bucket": SizeBucket(count), "via": key[1]}})
		}
	}
	return rows
}

// Rollup step that emits the usage day's rows unless that day was already reported
func dailyRollup(usageDay, oldest string, marks []MarkRow, resource map[string]string) func(map[string]any) (any, []RollupRow) {
	return func(st map[string]any) (any, []RollupRow) {
		var days []any
		var raw []any
		switch v := st["days"].(type) {
		case []any:
			raw = v
		case string:
			for _, r := range pystr.Runes(v) {
				raw = append(raw, pystr.FromRunes([]rune{r}))
			}
		}
		reported := false
		for _, d := range raw {
			if s, ok := d.(string); ok && s >= oldest {
				days = append(days, s)
				reported = reported || s == usageDay
			}
		}
		if reported {
			return map[string]any{"days": days}, nil
		}
		days = append(days, usageDay)
		slices.SortFunc(days, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
		rows := make([]RollupRow, 0, len(marks))
		for _, m := range marks {
			rows = append(rows, RollupRow{Metric: m.Mark, Dimensions: m.Dims, Resource: resource, PeriodStart: usageDay})
		}
		return map[string]any{"days": days}, rows
	}
}

// Rollup write of the daily report, replaced by tests that refuse it
var updateRollupStateFn = (*SharedMetricsStore).UpdateRollupState

// Records one finished Desktop day in that day's period, true once the day is settled
func RecordDesktopDaily(ctx context.Context, day, modes, actions, botCount any) (settled bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Desktop daily report not recorded", "error", r)
			settled = false
		}
	}()
	if !relayEnabled(ctx) {
		return false
	}
	today := desktopUTCDay()
	t, err := time.Parse(time.DateOnly, today)
	if err != nil {
		panic(err)
	}
	oldest := t.AddDate(0, 0, -DailyReportWindowDays).Format(time.DateOnly)
	usageDay := ""
	if s, ok := day.(string); ok && desktopDayPattern.MatchString(s) && oldest <= s && s <= today {
		usageDay = s
	}
	var marks []MarkRow
	if usageDay != "" {
		marks = append(modeRows(modes, botCount), actionRows(actions)...)
	}
	if len(marks) > 0 {
		store, resource, err := storeAndResource(ctx)
		if err != nil {
			panic(err)
		}
		if err := updateRollupStateFn(store, ctx, DailyReportStateKey, dailyRollup(usageDay, oldest, marks, resource)); err != nil {
			panic(err)
		}
	}
	return true
}

// Records one Desktop area use once per profile, UTC day and area, latched with the row
func RecordDesktopFeatureUse(ctx context.Context, area any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Desktop feature use not recorded", "error", r)
		}
	}()
	if !relayEnabled(ctx) {
		return
	}
	store, resource, err := storeAndResource(ctx)
	if err != nil {
		panic(err)
	}
	if _, err := store.RecordCounterOncePerDay(ctx, DesktopFeatureUseMetric, map[string]string{"area": FeatureArea(area)}, resource); err != nil {
		panic(err)
	}
}

// Counts one Desktop friction kind and detail under the daily cap
func RecordDesktopFriction(ctx context.Context, kind, detail any) {
	emit(ctx, DesktopFrictionMark, func() map[string]string { return frictionFields(ctx, kind, detail) })
}

// Counts one Desktop onboarding step event once per profile ever
func RecordDesktopOnboarding(ctx context.Context, step, event any) {
	emit(ctx, DesktopOnboardingMark, func() map[string]string { return onboardingFields(ctx, step, event) })
}

// Counts one Desktop dislike signal under the daily cap, nil setting reading None
func RecordDesktopDislike(ctx context.Context, signal, target, setting any) {
	emit(ctx, DesktopDislikeMark, func() map[string]string { return dislikeFields(ctx, signal, target, setting) })
}
