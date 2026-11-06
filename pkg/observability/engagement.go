package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const (
	IdleCapMS            = 5 * 60_000
	EngagementStateKey   = "engagement_day_state"
	engagementAllStreams = "*"
	busyPeakStateKey     = "busy_chats_peak"
)

var (
	activeThresholds   = []threshold{{5 * minuteMS, "lt_5m"}, {30 * minuteMS, "5m_to_30m"}, {120 * minuteMS, "30m_to_2h"}, {360 * minuteMS, "2h_to_6h"}}
	switchThresholds   = []threshold{{2, "1"}, {4, "2_to_3"}, {11, "4_to_10"}, {31, "11_to_30"}}
	engagedEntrypoints = Set("gateway_message", "interactive", "one_shot")
	interactionMetrics = Set(TaskStartedMetric, TaskFinishedMetric)
	engagementNow      = pyTime
)

// ClassifiedCounter is one metric, dimensions and amount an event satisfies
type ClassifiedCounter struct {
	Metric     string
	Dimensions map[string]string
	Amount     int64
}

// Bucket of active milliseconds, zero for none
func ActiveMinutesBucket(activeMS int64) string {
	if activeMS <= 0 {
		return "0"
	}
	return bucket(float64(activeMS), activeThresholds, "gte_6h")
}

// Count of surfaces used, capped at gte_4
func SurfacesUsedBucket(count int) string {
	if count < 4 {
		return fmt.Sprint(count)
	}
	return "gte_4"
}

// Bucket of turns a model served before a switch
func TurnsBeforeSwitchBucket(turns int) string {
	return bucket(float64(turns), switchThresholds, "gte_31")
}

// Engagement surface of the first engaged task start or end counter, empty when none
func InteractionSurface(classified []ClassifiedCounter) string {
	for _, row := range classified {
		if interactionMetrics.Has(row.Metric) && dimensionIn(row.Dimensions, "entrypoint", engagedEntrypoints.Sorted()...) {
			if surface := row.Dimensions["execution_surface"]; EngagementSurfaces.Has(surface) {
				return surface
			}
			return ""
		}
	}
	return ""
}

// Reports a user turn sent from an engaged entrypoint
func EngagedTurn(startFields map[string]string, userTurn bool) bool {
	return userTurn && dimensionIn(startFields, "entrypoint", engagedEntrypoints.Sorted()...)
}

// Provider and model of one validated engagement turn mark
func TurnRoute(event *RelayEvent) ([2]string, bool) {
	if !validShape(event, markShape...) || eventText(event, "name") != EngagementTurnMark {
		return [2]string{}, false
	}
	data, ok := asDict(eventAttr(event, "data"))
	if !ok || !keysOf(data).Equal(Set("model", "provider")) {
		return [2]string{}, false
	}
	var route [2]string
	for i, key := range []string{"provider", "model"} {
		value, isText := data[key].(string)
		if !isText || value != metricIdentifier(value, modelRouteMaxLengths[key]) {
			return [2]string{}, false
		}
		route[i] = value
	}
	return route, true
}

// Folds an interaction or attended turn into the store's daily rollup and the root profile's host row
func Record(ctx context.Context, store *SharedMetricsStore, resource map[string]string, surface string, route *[2]string) error {
	profileHome := filepath.Dir(filepath.Dir(filepath.Dir(store.DatabasePath)))
	rootHome := engagementRootHome(profileHome)
	owner := rootHome == profileHome
	profile := ""
	if surface != "" {
		profile = profileHash(profileHome)
	}
	ownerProfile := ""
	if owner {
		ownerProfile = profile
	}
	var applyErr error
	err := store.UpdateRollupState(ctx, EngagementStateKey, func(state map[string]any) (any, []RollupRow) {
		next, rows, err := Apply(state, nowMS(), resource, surface, route, ownerProfile, owner,
			func(day string) int { return DayPeak(profileHome, day) })
		if err != nil {
			applyErr = err
			return state, nil
		}
		return next, rows
	})
	if err != nil {
		return err
	}
	if applyErr != nil {
		return applyErr
	}
	if profile == "" || owner || !collects(ctx, rootHome) {
		return nil
	}
	root, err := rootStore(ctx, rootHome)
	if err != nil {
		return err
	}
	err = root.UpdateRollupState(ctx, EngagementStateKey, func(state map[string]any) (any, []RollupRow) {
		next, rows, err := Apply(state, nowMS(), resource, "", nil, profile, true, nil)
		if err != nil {
			applyErr = err
			return state, nil
		}
		return next, rows
	})
	if err != nil {
		return err
	}
	return applyErr
}

func nowMS() int64 { return int64(engagementNow() * 1000) }

func engagementRootHome(profileHome string) string {
	if resolved, err := pypath.Resolve(home.RootOf(profileHome)); err == nil {
		return resolved
	}
	resolved, _ := pypath.Resolve(profileHome)
	return resolved
}

func profileHash(profileHome string) string {
	resolved, _ := pypath.Resolve(profileHome)
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])[:16]
}

// Reports the root profile's own collection consent read without touching its runtime
func collects(ctx context.Context, rootHome string) bool {
	raw, err := config.ReadRawConfigReadonly(home.WithOverride(ctx, rootHome), rootHome)
	if err != nil {
		return false
	}
	var cfg any = raw
	if !pyTruthy(raw) {
		cfg = map[string]any{}
	}
	for _, key := range []string{"telemetry", "shared_metrics"} {
		cfg = installSub(cfg, key)
	}
	d, ok := asDict(cfg)
	if !ok {
		return false
	}
	enabled, isBool := d["enabled"].(bool)
	return isBool && enabled
}

var (
	rootStoresMu sync.Mutex
	rootStores   = map[string]*SharedMetricsStore{}
)

func rootStore(ctx context.Context, rootHome string) (*SharedMetricsStore, error) {
	rootStoresMu.Lock()
	defer rootStoresMu.Unlock()
	if s, ok := rootStores[rootHome]; ok {
		return s, nil
	}
	root := filepath.Join(rootHome, "telemetry", "shared_metrics")
	s, err := NewSharedMetricsStore(ctx, filepath.Join(root, "metrics.sqlite3"), filepath.Join(root, "outbox"))
	if err != nil {
		return nil, err
	}
	rootStores[rootHome] = s
	return s, nil
}

func utcDay(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.DateOnly)
}

func freshEngagement(day string, resource map[string]string, owner bool) map[string]any {
	res := make(map[string]any, len(resource))
	for key, value := range resource {
		res[key] = value
	}
	return map[string]any{"day": day, "resource": res, "surfaces": map[string]any{}, "models": map[string]any{},
		"profiles": []any{}, "owner": owner}
}

func validEngagementState(state map[string]any) bool {
	if state == nil {
		return false
	}
	_, dayOK := state["day"].(string)
	_, surfacesOK := state["surfaces"].(map[string]any)
	_, modelsOK := state["models"].(map[string]any)
	return dayOK && surfacesOK && modelsOK
}

// Python isinstance int of a decoded value, bool included
func intLike(v any) (int64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	if i, ok := pyInt(v); ok && i.IsInt64() {
		return i.Int64(), true
	}
	return 0, false
}

func pyIntOf(v any) (int64, error) {
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("OverflowError: int too large")
	}
	return n.Int64(), nil
}

func foldSurface(surfaces map[string]any, stream string, now int64) error {
	var active, last any = int64(0), nil
	if stored := surfaces[stream]; pyTruthy(stored) {
		pair, ok := stored.([]any)
		if !ok || len(pair) != 2 {
			return fmt.Errorf("ValueError: cannot unpack %T into the active and last pair", stored)
		}
		active, last = pair[0], pair[1]
	}
	if lastMS, ok := intLike(last); ok {
		gap := min(max(0, now-lastMS), IdleCapMS)
		switch a := active.(type) {
		case float64:
			active = a + float64(gap)
		default:
			n, isInt := intLike(a)
			if !isInt {
				return fmt.Errorf("TypeError: unsupported operand type for +=: %T", a)
			}
			active = n + gap
		}
	}
	var latest any = now
	if pyTruthy(last) {
		if lastMS, ok := intLike(last); ok {
			latest = max(now, lastMS)
		} else if f, isFloat := last.(float64); isFloat {
			if f > float64(now) {
				latest = f
			}
		} else {
			return fmt.Errorf("TypeError: '>' not supported between int and %T", last)
		}
	}
	surfaces[stream] = []any{active, latest}
	return nil
}

// Folds one interaction, attended turn, profile or busy peak into the day's state, closing an earlier day first
func Apply(state map[string]any, nowMS int64, resource map[string]string, surface string, route *[2]string,
	profile string, owner bool, busyPeak func(day string) int) (map[string]any, []RollupRow, error) {
	today := utcDay(nowMS)
	var rows []RollupRow
	if state != nil && !validEngagementState(state) {
		state = nil
	}
	if state != nil && state["day"].(string) < today {
		if busyPeak != nil {
			foldBusyPeak(state, busyPeak(state["day"].(string)))
		}
		closed, err := CloseDay(state)
		if err != nil {
			return nil, nil, err
		}
		rows, state = closed, nil
	}
	if state == nil {
		state = freshEngagement(today, resource, owner)
	}
	state["owner"] = owner
	if _, set := state["profiles"]; !set {
		state["profiles"] = []any{}
	}
	if profiles, ok := state["profiles"].([]any); ok && profile != "" {
		found := false
		for _, p := range profiles {
			if p == profile {
				found = true
			}
		}
		if !found {
			state["profiles"] = append(profiles, profile)
		}
	}
	if EngagementSurfaces.Has(surface) {
		surfaces := state["surfaces"].(map[string]any)
		for _, stream := range []string{surface, engagementAllStreams} {
			if err := foldSurface(surfaces, stream, nowMS); err != nil {
				return nil, nil, err
			}
		}
	}
	if route != nil {
		key := route[0] + " " + route[1]
		models := state["models"].(map[string]any)
		var stored any = int64(0)
		if v, ok := models[key]; ok {
			stored = v
		}
		count, err := pyIntOf(stored)
		if err != nil {
			return nil, nil, err
		}
		models[key] = count + 1
	}
	if busyPeak != nil {
		foldBusyPeak(state, busyPeak(today))
	}
	return state, rows, nil
}

func foldBusyPeak(state map[string]any, peak int) {
	stored, ok := intLike(state[busyPeakStateKey])
	if !ok {
		stored = 0
	}
	state[busyPeakStateKey] = max(stored, int64(peak))
}

func primaryRoute(models map[string]any) (string, string) {
	bestCount, bestKey, found := int64(0), "", false
	for key, n := range models {
		count, ok := intLike(n)
		if !ok || strings.Count(key, " ") != 1 {
			continue
		}
		if !found || count > bestCount || (count == bestCount && key > bestKey) {
			bestCount, bestKey, found = count, key, true
		}
	}
	if !found {
		return "none", "none"
	}
	provider, model, _ := strings.Cut(bestKey, " ")
	return provider, model
}

func stringMap(v any) (map[string]string, bool) {
	d, ok := asDict(v)
	if !ok {
		return map[string]string{}, !pyTruthy(v)
	}
	out := make(map[string]string, len(d))
	for key, value := range d {
		text, isText := value.(string)
		if !isText {
			return nil, false
		}
		out[key] = text
	}
	return out, true
}

// Rows one closed day reports, the root profile's host row carrying the active profile count
func CloseDay(state map[string]any) ([]RollupRow, error) {
	day := state["day"].(string)
	resource, resourceOK := stringMap(state["resource"])
	streams := map[string][]any{}
	for key, value := range state["surfaces"].(map[string]any) {
		if pair, ok := value.([]any); ok && len(pair) == 2 {
			streams[key] = pair
		}
	}
	var names []string
	for key := range streams {
		if EngagementSurfaces.Has(key) {
			names = append(names, key)
		}
	}
	sort.Strings(names)
	activeProfiles := 0
	if ownerFlag, isBool := state["owner"].(bool); isBool && ownerFlag {
		if profiles, ok := state["profiles"].([]any); ok {
			activeProfiles = len(profiles)
		}
	}
	if len(names) == 0 && activeProfiles == 0 {
		return nil, nil
	}
	var rows []RollupRow
	for _, name := range names {
		active, err := pyIntOf(streams[name][0])
		if err != nil {
			return nil, err
		}
		rows = append(rows, RollupRow{Metric: EngagementSurfaceMetric, Resource: resource, PeriodStart: day,
			Dimensions: map[string]string{"active_minutes_bucket": ActiveMinutesBucket(active), "surface": name}})
	}
	var allActive any = int64(0)
	if pair, ok := streams[engagementAllStreams]; ok {
		allActive = pair[0]
	}
	active, err := pyIntOf(allActive)
	if err != nil {
		return nil, err
	}
	provider, model := primaryRoute(state["models"].(map[string]any))
	peak, ok := intLike(state[busyPeakStateKey])
	if !ok {
		peak = 0
	}
	rows = append(rows, RollupRow{Metric: EngagementDayMetric, Resource: resource, PeriodStart: day,
		Dimensions: map[string]string{
			"active_minutes_bucket":       ActiveMinutesBucket(active),
			"active_profile_count_bucket": SizeBucket(activeProfiles),
			BusyPeakField:                 CountBucket(int(min(peak, 1<<31-1))),
			"primary_model":               model, "primary_provider": provider,
			"surfaces_used_count": SurfacesUsedBucket(len(names)),
		}})
	if !resourceOK {
		return nil, nil
	}
	return rows, nil
}

// RouteRun counts consecutive turns the conversation's current model served since the last switch
type RouteRun struct {
	Route map[string]string
	Turns int
}

// Counts one turn on a route, restarting the run when the route changed
func (r *RouteRun) Observe(route map[string]string) {
	if r.Route != nil && route != nil && maps.Equal(route, r.Route) {
		r.Turns++
		return
	}
	r.Route, r.Turns = route, 1
}

// Route being left and its turn count, resetting the run
func (r *RouteRun) Take() (map[string]string, int, bool) {
	route, turns := r.Route, r.Turns
	r.Route, r.Turns = nil, 0
	if route == nil || turns == 0 {
		return nil, 0, false
	}
	return route, turns, true
}

// Route fields plus the bucket of turns served before the switch
func SwitchAfterFields(route map[string]string, turns int) map[string]string {
	out := maps.Clone(route)
	if out == nil {
		out = map[string]string{}
	}
	out["turns_before_switch_bucket"] = TurnsBeforeSwitchBucket(turns)
	return out
}

// SessionTally merges the segments compression rotation splits one conversation into
type SessionTally struct {
	StartFields map[string]string
	Turns       int
	FailedTurns int
	LastOutcome string
	FirstTurnNS int64
	LastTurnNS  int64
	ModelCalls  int
	ToolCalls   int
	Replies     int
}

// Fresh tally whose last outcome reads unknown
func NewSessionTally() *SessionTally { return &SessionTally{LastOutcome: "unknown"} }

// Merges another segment's tally, keeping the earliest start and the latest outcome
func (t *SessionTally) Absorb(other *SessionTally) {
	if other.Turns == 0 {
		return
	}
	if t.Turns == 0 || other.FirstTurnNS < t.FirstTurnNS {
		t.StartFields, t.FirstTurnNS = other.StartFields, other.FirstTurnNS
	}
	if other.LastTurnNS >= t.LastTurnNS {
		t.LastOutcome, t.LastTurnNS = other.LastOutcome, other.LastTurnNS
	}
	t.Turns += other.Turns
	t.FailedTurns += other.FailedTurns
	t.ModelCalls += other.ModelCalls
	t.ToolCalls += other.ToolCalls
	t.Replies += other.Replies
}

// Session summary fields of the merged conversation, nil before its first turn
func (t *SessionTally) Fields() map[string]string {
	if t.Turns == 0 || t.StartFields == nil {
		return nil
	}
	fields := SessionFields(t.StartFields, t.Turns, t.FailedTurns, t.LastOutcome, max(0, t.LastTurnNS-t.FirstTurnNS)/1_000_000)
	fields["message_count_bucket"] = LongSizeBucket(t.Turns + t.Replies + t.ToolCalls)
	fields["model_call_count_bucket"] = LongSizeBucket(t.ModelCalls)
	fields["tool_call_count_bucket"] = LongSizeBucket(t.ToolCalls)
	return fields
}
