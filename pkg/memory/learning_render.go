package memory

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Recency of the oldest node, just off zero like the desktop time axis
const LeadIn = 0.06

// Age gradient inks, old quiet and recent bright
const (
	AgeOldInk = 0.42
	AgeMidInk = 0.74
	AgeNewInk = 0.95
	AgeMid    = 0.52
)

// Style keys consumers map to base colors, the run alpha giving brightness
const (
	StyleBG     = "bg"
	StyleSkill  = "skill"
	StyleMemory = "memory"
	StyleLabel  = "label"
	StyleDim    = "dim"
)

// Legend glyphs mirroring the desktop node shapes
const (
	SkillGlyph  = "\u25cf"
	MemoryGlyph = "\u25c6"
)

// Marker keys handed to label rows in order
var labelKeys = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b", "c"}

// Value clamped to a range, a NaN passing through like the Python comparison chain
func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if hi < v {
		return hi
	}
	return v
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func smoothstep(p float64) float64 {
	p = clampF(p, 0, 1)
	return p * p * (3 - 2*p)
}

// Python float modulo, the result taking the sign of the divisor
func pyFMod(a, b float64) float64 {
	m := math.Mod(a, b)
	if m != 0 && (b < 0) != (m < 0) {
		m += b
	}
	return m
}

// Python round to an integer, halves to even
func pyRoundInt(x float64) int { return int(math.RoundToEven(x)) }

// Node as a mapping, the AttributeError text Python raises for anything else
func nodeMap(v any) (map[string]any, error) {
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return nil, &pyError{class: "AttributeError", msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(v))}
}

// Mapping list of a payload field accepting typed and decoded JSON lists
func payloadMaps(v any) ([]map[string]any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []map[string]any:
		return x, nil
	case []any:
		out := make([]map[string]any, len(x))
		for i, item := range x {
			m, err := nodeMap(item)
			if err != nil {
				return nil, err
			}
			out[i] = m
		}
		return out, nil
	}
	return nil, &pyError{class: "TypeError", msg: fmt.Sprintf("'%s' object is not iterable", pyTypeName(v))}
}

// Raw list of a payload field for filters that skip non mapping entries
func payloadItems(v any) []any {
	switch x := v.(type) {
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out
	case []any:
		return x
	}
	return nil
}

func isMemoryNode(n map[string]any) bool { return n["kind"] == "memory" }

func nodeID(n map[string]any) string {
	v, ok := n["id"]
	if !ok {
		return ""
	}
	return pyStr(v)
}

// UTC datetime of epoch seconds rounded to microseconds, false where fromtimestamp raises
func utcFromTimestamp(ts float64) (time.Time, bool) {
	if math.IsNaN(ts) || math.IsInf(ts, 0) || math.Abs(ts) > 1e15 {
		return time.Time{}, false
	}
	sec := math.Trunc(ts)
	us := math.RoundToEven((ts - sec) * 1e6)
	if us >= 1e6 {
		sec++
		us -= 1e6
	} else if us < 0 {
		sec--
		us += 1e6
	}
	t := time.Unix(int64(sec), int64(us)*1000).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return time.Time{}, false
	}
	return t, true
}

func leadIn(ratio float64) float64 { return LeadIn + (1-LeadIn)*ratio }

func visibleCount(reveal float64, n int) int {
	return int(clampF(math.Ceil(reveal*float64(n)), 0, float64(n)))
}

// Label of a node, its id or unknown when the label is empty
func nodeRawLabel(n map[string]any) string {
	v := n["label"]
	if !pyTruthy(v) {
		v = n["id"]
	}
	if !pyTruthy(v) {
		v = "unknown"
	}
	return pyStrip(pyStr(v))
}

// Float of a JSON value the way float accepts it, false where float raises
func pyFloatOf(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		return pyFloatString(string(x))
	case string:
		return pyFloatString(x)
	}
	return 0, false
}

// Timestamp of a node as a float, nil when absent or not numeric
func nodeTS(n map[string]any) *float64 {
	v, ok := n["timestamp"]
	if !ok || v == nil {
		return nil
	}
	f, ok := pyFloatOf(v)
	if !ok {
		return nil
	}
	return &f
}

// Timestamp of a node or a fallback when it is missing or zero
func nodeTSOr(n map[string]any, fallback float64) float64 {
	if ts := nodeTS(n); ts != nil && *ts != 0 {
		return *ts
	}
	return fallback
}

// Ink alpha of a recency ratio along the age gradient
func RecencyInk(rec float64) float64 {
	t := clampF(rec, 0, 1)
	if t <= AgeMid {
		return lerp(AgeOldInk, AgeMidInk, smoothstep(t/AgeMid))
	}
	return lerp(AgeMidInk, AgeNewInk, smoothstep((t-AgeMid)/(1-AgeMid)))
}

// Day, month and year of a timestamp, unknown for missing, zero, or out of range values
func FormatDate(ts *float64) string {
	if ts == nil || *ts == 0 {
		return "unknown"
	}
	t, ok := utcFromTimestamp(*ts)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%d %s", t.Day(), t.Format("Jan 2006"))
}

// Recency ratio of every node, timed graphs by timestamp and untimed graphs by ordinal position
type Recency struct {
	Rec   map[string]float64
	Timed bool
	MinTS *float64
	MaxTS *float64
}

// Recency of nodes ported from the desktop time axis
func ComputeRecency(nodes []map[string]any) Recency {
	var minTS, maxTS *float64
	for _, n := range nodes {
		ts := nodeTS(n)
		if ts == nil {
			continue
		}
		if minTS == nil {
			lo, hi := *ts, *ts
			minTS, maxTS = &lo, &hi
			continue
		}
		if *ts < *minTS {
			*minTS = *ts
		}
		if *ts > *maxTS {
			*maxTS = *ts
		}
	}
	timed := minTS != nil && *maxTS > *minTS
	type keyed struct {
		ts float64
		id string
	}
	keys := make([]keyed, len(nodes))
	order := make([]int, len(nodes))
	for i, n := range nodes {
		ts := math.Inf(1)
		if t := nodeTS(n); t != nil {
			ts = *t
		}
		keys[i] = keyed{ts, nodeID(n)}
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ka, kb := keys[order[a]], keys[order[b]]
		if ka.ts != kb.ts {
			return ka.ts < kb.ts
		}
		return ka.id < kb.id
	})
	last := max(len(order)-1, 1)
	ordRatio := map[string]float64{}
	for i, idx := range order {
		r := 0.0
		if len(order) > 1 {
			r = float64(i) / float64(last)
		}
		ordRatio[keys[idx].id] = r
	}
	rec := map[string]float64{}
	for _, n := range nodes {
		id, ts := nodeID(n), nodeTS(n)
		var ratio float64
		if timed && ts != nil {
			ratio = (*ts - *minTS) / (*maxTS - *minTS)
		} else {
			ratio = ordRatio[id]
		}
		rec[id] = leadIn(clampF(ratio, 0, 1))
	}
	return Recency{Rec: rec, Timed: timed, MinTS: minTS, MaxTS: maxTS}
}

// Timestamp a reveal fraction points at, nil for untimed graphs
func dateAt(rec Recency, reveal float64) *float64 {
	if !rec.Timed || rec.MinTS == nil || rec.MaxTS == nil {
		return nil
	}
	v := float64(pyRoundInt(*rec.MinTS + clampF(reveal, 0, 1)*(*rec.MaxTS-*rec.MinTS)))
	return &v
}

// Value of a hex digit string the way int with base 16 parses it
func pyIntBase16(s string) (int, bool) {
	t := pyStrip(s)
	neg := false
	if strings.HasPrefix(t, "+") || strings.HasPrefix(t, "-") {
		neg = t[0] == '-'
		t = t[1:]
	}
	if len(t) >= 2 && t[0] == '0' && (t[1] == 'x' || t[1] == 'X') {
		t = strings.TrimPrefix(t[2:], "_")
	}
	if t == "" || strings.HasPrefix(t, "_") || strings.HasSuffix(t, "_") || strings.Contains(t, "__") {
		return 0, false
	}
	v := 0
	for _, r := range t {
		d := -1
		switch {
		case r == '_':
			continue
		case r >= '0' && r <= '9':
			d = int(r - '0')
		case r >= 'a' && r <= 'f':
			d = int(r-'a') + 10
		case r >= 'A' && r <= 'F':
			d = int(r-'A') + 10
		case r > 0x7f && isUnicodeDecimal(r):
			d = int(unicodeDecimalValue(r))
		}
		if d < 0 {
			return 0, false
		}
		v = v*16 + d
	}
	if neg {
		v = -v
	}
	return v, true
}

// Rune slice of s between i and j clamped like a Python slice
func runeSlice(rs []rune, i, j int) string {
	i, j = min(max(i, 0), len(rs)), min(max(j, 0), len(rs))
	if i >= j {
		return ""
	}
	return string(rs[i:j])
}

// RGB triple of a hex color, three digit shorthand expanded, gold when unparseable
func HexToRGB(s string) [3]int {
	t := strings.TrimLeft(pyStrip(s), "#")
	if utf8.RuneCountInString(t) == 3 {
		var b strings.Builder
		for _, r := range t {
			b.WriteRune(r)
			b.WriteRune(r)
		}
		t = b.String()
	}
	rs := []rune(t)
	var out [3]int
	for i := 0; i < 3; i++ {
		v, ok := pyIntBase16(runeSlice(rs, i*2, i*2+2))
		if !ok {
			return [3]int{255, 215, 0}
		}
		out[i] = v
	}
	return out
}

// Upper case hex color of an RGB triple, channels clamped and truncated
func RGBToHex(c [3]float64) string {
	return fmt.Sprintf("#%02X%02X%02X", int(clampF(c[0], 0, 255)), int(clampF(c[1], 0, 255)), int(clampF(c[2], 0, 255)))
}

func rgbFloats(c [3]int) [3]float64 { return [3]float64{float64(c[0]), float64(c[1]), float64(c[2])} }

// Channel wise blend of two colors at t, rounded half to even
func MixRGB(a, b [3]int, t float64) [3]int {
	var out [3]int
	for i := 0; i < 3; i++ {
		out[i] = pyRoundInt(lerp(float64(a[i]), float64(b[i]), clampF(t, 0, 1)))
	}
	return out
}

// Hue in degrees, saturation, and lightness of an RGB color
func rgbToHSL(c [3]int) (float64, float64, float64) {
	r, g, b := float64(c[0])/255, float64(c[1])/255, float64(c[2])/255
	mx, mn := math.Max(math.Max(r, g), b), math.Min(math.Min(r, g), b)
	light, d := (mx+mn)/2, mx-mn
	if d == 0 {
		return 0, 0, light
	}
	var s float64
	if light > 0.5 {
		s = d / (2 - mx - mn)
	} else {
		s = d / (mx + mn)
	}
	var h float64
	switch {
	case mx == r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case mx == g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h * 60, s, light
}

// RGB of a hue, saturation, and lightness, each channel rounded half to even
func hslToRGB(h, s, light float64) [3]int {
	hue := pyFMod(pyFMod(h, 360)+360, 360)
	c := (1 - math.Abs(2*light-1)) * s
	x := c * (1 - math.Abs(pyFMod(hue/60, 2)-1))
	m := light - c/2
	var v [3]float64
	switch min(int(math.Floor(hue/60)), 5) {
	case 0:
		v = [3]float64{c, x, 0}
	case 1:
		v = [3]float64{x, c, 0}
	case 2:
		v = [3]float64{0, c, x}
	case 3:
		v = [3]float64{0, x, c}
	case 4:
		v = [3]float64{x, 0, c}
	default:
		v = [3]float64{c, 0, x}
	}
	return [3]int{pyRoundInt((v[0] + m) * 255), pyRoundInt((v[1] + m) * 255), pyRoundInt((v[2] + m) * 255)}
}

// Terminal palette derived from a primary color, memories in primary ink and skills in a muted complement
func DerivePalette(primaryHex string, dark bool) map[string]string {
	primary := HexToRGB(primaryHex)
	base, bg := [3]int{255, 255, 255}, [3]int{8, 8, 12}
	memoryMix := 0.12
	if !dark {
		base, bg = [3]int{0, 0, 0}, [3]int{250, 250, 250}
		memoryMix = 0.18
	}
	h, s, light := rgbToHSL(primary)
	return map[string]string{
		"primary": primaryHex,
		"memory":  RGBToHex(rgbFloats(MixRGB(primary, base, memoryMix))),
		"skill":   RGBToHex(rgbFloats(MixRGB(hslToRGB(h+165, math.Max(s, 0.5), clampF(light, 0.5, 0.7)), bg, 0.45))),
		"label":   RGBToHex(rgbFloats(MixRGB(base, bg, 0.35))),
		"dim":     RGBToHex(rgbFloats(MixRGB(base, bg, 0.7))),
		"bg":      RGBToHex(rgbFloats(bg)),
	}
}

// Integer of a JSON value the way int converts it, the Python error text otherwise
func pyIntOf(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) {
			return 0, &pyError{class: "ValueError", msg: "cannot convert float NaN to integer"}
		}
		if math.IsInf(x, 0) {
			return 0, &pyError{class: "OverflowError", msg: "cannot convert float infinity to integer"}
		}
		return int(math.Trunc(x)), nil
	case json.Number:
		if pyTypeName(x) == "float" {
			f, err := strconv.ParseFloat(string(x), 64)
			if err != nil {
				return 0, err
			}
			return pyIntOf(f)
		}
		n, err := strconv.ParseInt(string(x), 10, 64)
		return int(n), err
	case string:
		if i, ok := pyInt(x); ok {
			return i, nil
		}
		return 0, &pyError{class: "ValueError", msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyRepr(x))}
	case bool, int, int64:
		i, _ := pyInt(x)
		return i, nil
	}
	return 0, &pyError{class: "TypeError", msg: fmt.Sprintf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))}
}

// Float of a JSON value the way float converts it, the Python error text otherwise
func pyFloatOfErr(v any) (float64, error) {
	if f, ok := pyFloatOf(v); ok {
		return f, nil
	}
	switch v.(type) {
	case string, json.Number:
		return 0, &pyError{class: "ValueError", msg: fmt.Sprintf("could not convert string to float: %s", pyRepr(pyStr(v)))}
	}
	return 0, &pyError{class: "TypeError", msg: fmt.Sprintf("float() argument must be a string or a real number, not '%s'", pyTypeName(v))}
}

// Value of a node count field read like int of the value or zero
func nodeCount(n map[string]any, key string) (int, error) {
	v := n[key]
	if !pyTruthy(v) {
		return 0, nil
	}
	return pyIntOf(v)
}

// Marker priority of a node, memories first then recent, used, and pinned skills
func nodeScore(n map[string]any, rec float64) (float64, error) {
	if isMemoryNode(n) {
		return 3.5 + rec, nil
	}
	uses := 0.0
	if v := n["useCount"]; pyTruthy(v) {
		f, err := pyFloatOfErr(v)
		if err != nil {
			return 0, err
		}
		uses = f
	}
	score := rec*2 + math.Sqrt(math.Max(0, uses))
	if pyTruthy(n["pinned"]) {
		score += 2
	}
	return score, nil
}

// Glyph, label, meta line and style shared by label rows and bucket trees
func nodeCard(n map[string]any) (map[string]any, error) {
	mem, text, date := isMemoryNode(n), nodeRawLabel(n), FormatDate(nodeTS(n))
	var meta string
	if mem {
		kind := "memory"
		if n["memorySource"] == "profile" {
			kind = "profile memory"
		}
		meta = kind + " \u00b7 " + date
	} else {
		count, err := nodeCount(n, "useCount")
		if err != nil {
			return nil, err
		}
		category := "skill"
		if pyTruthy(n["category"]) {
			category = pyStr(n["category"])
		}
		parts := []string{category, date}
		if count != 0 {
			parts = append(parts, fmt.Sprintf("x%d", count))
		}
		if pyTruthy(n["pinned"]) {
			parts = append(parts, "pinned")
		}
		meta = strings.Join(parts, " \u00b7 ")
	}
	label := text
	if utf8.RuneCountInString(text) > 26 {
		label = strings.TrimRightFunc(runePrefix(text, 23), pyIsSpace) + "\u2026"
	}
	glyph, style := SkillGlyph, StyleSkill
	if mem {
		glyph, style = MemoryGlyph, StyleMemory
	}
	return map[string]any{"glyph": glyph, "label": label, "meta": meta, "style": style}, nil
}

// Insertion ordered count of skill categories, uncategorized skills counted as skill
func skillCategoryCounts(nodes []map[string]any) *orderedCounter {
	c := newOrderedCounter()
	for _, n := range nodes {
		if isMemoryNode(n) {
			continue
		}
		cat := "skill"
		if pyTruthy(n["category"]) {
			cat = pyStr(n["category"])
		}
		c.add(cat, 1)
	}
	return c
}

// One timeline row of the chart, its nodes, timestamp, and recency
type chartBucket struct {
	label string
	ts    float64
	rec   float64
	nodes []map[string]any
}

func newChartBucket(label string, ts float64) *chartBucket {
	return &chartBucket{label: label, ts: ts, rec: 1}
}

func (b *chartBucket) memories() int {
	n := 0
	for _, node := range b.nodes {
		if isMemoryNode(node) {
			n++
		}
	}
	return n
}

func (b *chartBucket) skills() int { return len(b.nodes) - b.memories() }

func (b *chartBucket) total() int { return len(b.nodes) }

// Dominant skill category of the bucket, the first seen winning a tie, empty without skills
func (b *chartBucket) category() string {
	counts := skillCategoryCounts(b.nodes)
	best, bestN := "", -1
	for _, k := range counts.keys {
		if counts.counts[k] > bestN {
			best, bestN = k, counts.counts[k]
		}
	}
	return best
}

// Period key and row label of a timestamp at day, month, or year granularity
func period(ts float64, granularity string) ([]int, string, error) {
	t, ok := utcFromTimestamp(ts)
	if !ok {
		return nil, "", &pyError{class: "ValueError", msg: fmt.Sprintf("year is out of range for timestamp %s", pyFloatRepr(ts))}
	}
	switch granularity {
	case "day":
		return []int{t.Year(), int(t.Month()), t.Day()}, fmt.Sprintf("%d %s", t.Day(), t.Format("Jan")), nil
	case "month":
		return []int{t.Year(), int(t.Month())}, t.Format("Jan 2006"), nil
	}
	return []int{t.Year()}, t.Format("2006"), nil
}

func lessInts(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// Drops each node into the bin its recency ratio maps to, order preserved
func fillEvenBins(buckets []*chartBucket, nodes []map[string]any, rec Recency) {
	n := len(buckets)
	for _, node := range nodes {
		idx := int(clampF(math.Floor(rec.Rec[nodeID(node)]*float64(n)), 0, float64(n-1)))
		buckets[idx].nodes = append(buckets[idx].nodes, node)
	}
}

// Timeline rows at the finest date granularity that fits, oldest to newest
func buildChartBuckets(nodes []map[string]any, rec Recency, maxRows int) ([]*chartBucket, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	if !rec.Timed {
		buckets := make([]*chartBucket, min(maxRows, len(nodes)))
		for i := range buckets {
			buckets[i] = newChartBucket(fmt.Sprintf("#%d", i+1), float64(i))
		}
		sorted := append([]map[string]any{}, nodes...)
		sort.SliceStable(sorted, func(a, b int) bool { return rec.Rec[nodeID(sorted[a])] < rec.Rec[nodeID(sorted[b])] })
		fillEvenBins(buckets, sorted, rec)
		return buckets, nil
	}
	var chosen []*chartBucket
	for _, granularity := range []string{"day", "month", "year"} {
		groups := map[string]*chartBucket{}
		keys := map[string][]int{}
		var order []string
		for _, node := range nodes {
			ts := nodeTS(node)
			if ts == nil {
				continue
			}
			key, label, err := period(*ts, granularity)
			if err != nil {
				return nil, err
			}
			k := fmt.Sprint(key)
			if _, ok := groups[k]; !ok {
				groups[k], keys[k] = newChartBucket(label, *ts), key
				order = append(order, k)
			}
			groups[k].nodes = append(groups[k].nodes, node)
		}
		if len(groups) <= maxRows || (granularity == "day" && len(groups) <= 32) {
			sort.Slice(order, func(a, b int) bool { return lessInts(keys[order[a]], keys[order[b]]) })
			for _, k := range order {
				chosen = append(chosen, groups[k])
			}
			break
		}
	}
	minTS, maxTS := rec.MinTS, rec.MaxTS
	if chosen == nil {
		nBins := max(1, maxRows)
		for i := 0; i < nBins; i++ {
			stop := float64(i)
			if minTS != nil && maxTS != nil && *minTS != 0 && *maxTS != 0 {
				stop = *minTS + (float64(i)/float64(max(1, nBins-1)))*(*maxTS-*minTS)
			}
			s := stop
			chosen = append(chosen, newChartBucket(FormatDate(&s), stop))
		}
		fillEvenBins(chosen, nodes, rec)
	}
	span := 0.0
	if minTS != nil && maxTS != nil && *maxTS > *minTS {
		span = *maxTS - *minTS
	}
	for _, b := range chosen {
		if span != 0 {
			b.rec = leadIn((b.ts - *minTS) / span)
		} else {
			b.rec = 1
		}
	}
	return chosen, nil
}

// Interactive tree rows of each bucket with its nodes in chronological order
func bucketRows(buckets []*chartBucket, payload map[string]any) ([]map[string]any, error) {
	cmap, err := CategoryColorMap(payload)
	if err != nil {
		return nil, err
	}
	memoryLookup := map[string]map[string]any{}
	for idx, item := range payloadItems(payload["memory"]) {
		if card, ok := item.(map[string]any); ok {
			memoryLookup[MemoryNodeID(card, idx)] = card
		}
	}
	rows := []map[string]any{}
	for idx, b := range buckets {
		cat := b.category()
		var color any
		if cat != "" {
			if c, ok := cmap[cat]; ok {
				color = c
			}
		}
		nodes := append([]map[string]any{}, b.nodes...)
		sort.SliceStable(nodes, func(i, j int) bool { return nodeTSOr(nodes[i], b.ts) < nodeTSOr(nodes[j], b.ts) })
		nodeRows := []map[string]any{}
		for _, n := range nodes {
			card, err := nodeCard(n)
			if err != nil {
				return nil, err
			}
			body := ""
			if memory := memoryLookup[nodeID(n)]; len(memory) > 0 {
				if v, ok := memory["body"]; ok {
					body = pyStr(v)
				}
			}
			nodeRows = append(nodeRows, map[string]any{
				"id": nodeID(n), "glyph": card["glyph"], "label": card["label"], "fullLabel": nodeRawLabel(n),
				"meta": card["meta"], "body": body, "style": card["style"],
			})
		}
		var category any
		if cat != "" {
			category = cat
		}
		ts := b.ts
		rows = append(rows, map[string]any{
			"index": idx, "label": b.label, "date": FormatDate(&ts), "skills": b.skills(), "memories": b.memories(),
			"total": b.total(), "category": category, "color": color, "nodes": nodeRows,
		})
	}
	return rows, nil
}

// Skill categories with counts from the clusters, else counted from the nodes by count then name
func categoryCounts(payload map[string]any) ([]categoryCount, error) {
	var out []categoryCount
	for _, item := range payloadItems(payload["clusters"]) {
		c, err := nodeMap(item)
		if err != nil {
			return nil, err
		}
		cat := c["category"]
		if !pyTruthy(cat) || cat == "memory" {
			continue
		}
		n := 0
		if v, ok := c["count"]; ok {
			i, err := pyIntOf(v)
			if err != nil {
				return nil, err
			}
			n = i
		}
		out = append(out, categoryCount{pyStr(cat), n})
	}
	if len(out) > 0 {
		return out, nil
	}
	nodes, err := payloadMaps(payload["nodes"])
	if err != nil {
		return nil, err
	}
	counts := skillCategoryCounts(nodes)
	for _, k := range counts.keys {
		out = append(out, categoryCount{k, counts.counts[k]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].category < out[j].category
	})
	return out, nil
}

// Evenly spread golden angle hue per skill category, independent of the theme
func CategoryColorMap(payload map[string]any) (map[string]string, error) {
	cats, err := categoryCounts(payload)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, cc := range cats {
		out[cc.category] = RGBToHex(rgbFloats(hslToRGB(pyFMod(float64(i)*137.508, 360), 0.55, 0.62)))
	}
	return out, nil
}

// Colored legend entries of the top categories plus a count of the rest
func CategoryLegend(payload map[string]any, limit int) ([]map[string]any, error) {
	cmap, err := CategoryColorMap(payload)
	if err != nil {
		return nil, err
	}
	cats, err := categoryCounts(payload)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for i, cc := range cats {
		if i >= limit {
			break
		}
		out = append(out, map[string]any{"glyph": "\u25cf", "color": cmap[cc.category], "label": fmt.Sprintf("%s (%d)", cc.category, cc.count)})
	}
	if len(cats) > limit {
		out = append(out, map[string]any{"glyph": "\u00b7", "color": "", "label": fmt.Sprintf("+%d", len(cats)-limit)})
	}
	return out, nil
}

// Run of a grid row, text, style, alpha, and an optional hex color override
func run(text, style string, alpha float64, hex ...any) []any {
	r := []any{text, style, alpha}
	return append(r, hex...)
}

// Cumulative learning curve as a compact star path sparkline
func trajectoryRow(buckets []*chartBucket, width int, reveal float64) []any {
	if len(buckets) == 0 {
		return nil
	}
	total := 0
	for _, b := range buckets {
		total += b.total()
	}
	if total == 0 {
		total = 1
	}
	cells := make([]string, width)
	for i := range cells {
		cells[i] = " "
	}
	acc, last := 0, 0
	for _, b := range buckets[:visibleCount(reveal, len(buckets))] {
		acc += b.total()
		p := pyRoundInt((float64(acc) / float64(total)) * float64(width-1))
		for x := min(last, p); x <= max(last, p); x++ {
			if x >= 0 && x < width && cells[x] == " " {
				cells[x] = "\u00b7"
			}
		}
		if p >= 0 && p < width {
			cells[p] = "\u2726"
		}
		last = p
	}
	return []any{run("trajectory ", StyleLabel, 0.55), run(strings.Join(cells, ""), StyleSkill, 0.48)}
}

// Bar, skill, and memory cell counts of a bucket, a present kind never rounding to zero
func barLengths(b *chartBucket, maxTotal, barW int) (int, int, int) {
	barLen := 0
	if b.total() != 0 {
		barLen = max(1, pyRoundInt((float64(b.total())/float64(maxTotal))*float64(barW)))
	}
	skillLen := 0
	if b.skills() != 0 {
		skillLen = max(1, pyRoundInt((float64(b.skills())/float64(b.total()))*float64(barLen)))
	}
	if b.memories() != 0 && skillLen == barLen && barLen > 1 {
		skillLen = barLen - 1
	}
	return barLen, skillLen, barLen - skillLen
}

// Right aligns text to width code points like a Python format spec
func padLeft(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

// One timeline frame at a reveal fraction with category colored bars, markers, and the trajectory
func RenderGraph(payload map[string]any, cols, rows int, reveal float64) (map[string]any, error) {
	reveal, cols, rows = clampF(reveal, 0, 1), max(44, cols), max(14, rows)
	nodes, err := payloadMaps(payload["nodes"])
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return map[string]any{"grid": []any{[]any{run("no learning yet \u2014 keep using Icebath and it maps out here", StyleDim, 0.7)}},
			"date": "", "reveal": reveal, "visible": 0}, nil
	}
	rec := ComputeRecency(nodes)
	cmap, err := CategoryColorMap(payload)
	if err != nil {
		return nil, err
	}
	buckets, err := buildChartBuckets(nodes, rec, max(4, rows-3))
	if err != nil {
		return nil, err
	}
	visibleBuckets := visibleCount(reveal, len(buckets))
	maxTotal := 0
	labelW := 0
	for _, b := range buckets {
		maxTotal = max(maxTotal, b.total())
		labelW = max(labelW, utf8.RuneCountInString(b.label))
	}
	if maxTotal == 0 {
		maxTotal = 1
	}
	labelW = min(9, labelW)
	barW := max(14, cols-labelW-16)
	grid := []any{}
	labels := []any{}
	visible := 0
	for i, b := range buckets[:visibleBuckets] {
		visible += b.total()
		ink, cat := RecencyInk(b.rec), b.category()
		barLen, skillLen, memoryLen := barLengths(b, maxTotal, barW)
		var catHex any
		if cat != "" {
			if c, ok := cmap[cat]; ok {
				catHex = c
			}
		}
		row := []any{run(padLeft(b.label, labelW)+" ", StyleLabel, ink), run("\u2502 ", StyleDim, 0.55)}
		if len(b.nodes) > 0 && len(labels) < 6 {
			best, bestScore := b.nodes[0], math.Inf(-1)
			for j, n := range b.nodes {
				score, err := nodeScore(n, nodeTSOr(n, b.ts))
				if err != nil {
					return nil, err
				}
				if j == 0 || score > bestScore {
					best, bestScore = n, score
				}
			}
			card, err := nodeCard(best)
			if err != nil {
				return nil, err
			}
			marker := labelKeys[len(labels)]
			entry := map[string]any{"key": marker}
			for k, v := range card {
				entry[k] = v
			}
			entry["alpha"] = pyRound(ink, 3)
			labels = append(labels, entry)
			row = append(row, run(marker, StyleLabel, 0.95))
		} else if b.total() != 0 {
			if b.skills() != 0 {
				row = append(row, run("\u2726", StyleSkill, ink, catHex))
			} else {
				row = append(row, run("\u25c6", StyleMemory, ink, nil))
			}
		}
		if skillLen != 0 {
			row = append(row, run(strings.Repeat("\u2501", skillLen), StyleSkill, ink, catHex))
		}
		if memoryLen != 0 {
			bar := "\u25c6"
			if memoryLen != 1 {
				bar = "\u25c6" + strings.Repeat("\u2501", memoryLen-2) + "\u25c6"
			}
			row = append(row, run(bar, StyleMemory, math.Max(0.65, ink)))
		}
		if barLen < barW {
			row = append(row, run(strings.Repeat(" ", barW-barLen), StyleBG, 1.0))
		}
		row = append(row, run("  ", StyleBG, 1.0), run(strconv.Itoa(b.skills()), StyleSkill, math.Max(0.72, ink)))
		if b.memories() != 0 {
			row = append(row, run("+", StyleDim, 0.6), run(strconv.Itoa(b.memories()), StyleMemory, math.Max(0.72, ink)))
		}
		if i == visibleBuckets-1 {
			row = append(row, run("  \u25c0 now", StyleLabel, 0.9))
		} else if b.total() == maxTotal && maxTotal > 1 {
			row = append(row, run("  \u2604 peak", StyleLabel, 0.75))
		}
		grid = append(grid, row)
	}
	for range buckets[visibleBuckets:] {
		grid = append(grid, []any{})
	}
	last := []any{run(strings.Repeat(" ", labelW+2), StyleBG, 1.0)}
	last = append(last, trajectoryRow(buckets, max(12, cols-labelW-13), reveal)...)
	grid = append(grid, last)
	return map[string]any{"grid": grid, "date": FormatDate(dateAt(rec, reveal)), "reveal": reveal, "visible": visible, "labels": labels}, nil
}

// Skill and memory legend entries with their counts
func BuildLegend(payload map[string]any) ([]map[string]any, error) {
	nodes, err := payloadMaps(payload["nodes"])
	if err != nil {
		return nil, err
	}
	memories := 0
	for _, n := range nodes {
		if isMemoryNode(n) {
			memories++
		}
	}
	return []map[string]any{
		{"glyph": SkillGlyph, "style": StyleSkill, "label": fmt.Sprintf("skills (%d)", len(nodes)-memories)},
		{"glyph": MemoryGlyph, "style": StyleMemory, "label": fmt.Sprintf("memories (%d)", memories)},
	}, nil
}

// Start and end dates of a timed graph, oldest and now otherwise
func AxisLabels(payload map[string]any) (map[string]string, error) {
	nodes, err := payloadMaps(payload["nodes"])
	if err != nil {
		return nil, err
	}
	rec := ComputeRecency(nodes)
	if rec.Timed {
		return map[string]string{"start": FormatDate(rec.MinTS), "end": FormatDate(rec.MaxTS)}, nil
	}
	return map[string]string{"start": "oldest", "end": "now"}, nil
}

// Busiest learning day line, empty when no node is dated
func peakDay(payload map[string]any) (string, error) {
	nodes, err := payloadMaps(payload["nodes"])
	if err != nil {
		return "", err
	}
	counts := newOrderedCounter()
	labels := map[string]string{}
	for _, n := range nodes {
		ts := nodeTS(n)
		if ts == nil {
			continue
		}
		key, label, err := period(*ts, "day")
		if err != nil {
			return "", err
		}
		k := fmt.Sprint(key)
		counts.add(k, 1)
		labels[k] = label
	}
	if len(counts.keys) == 0 {
		return "", nil
	}
	best := counts.keys[0]
	for _, k := range counts.keys {
		if counts.counts[k] > counts.counts[best] {
			best = k
		}
	}
	return fmt.Sprintf("busiest day %s \u00b7 %d learned", labels[best], counts.counts[best]), nil
}

// Summary lines of learned skills, memories, links, and the busiest day
func BuildSummary(payload map[string]any) ([]string, error) {
	stats, _ := payload["stats"].(map[string]any)
	if stats == nil {
		stats = map[string]any{}
	}
	learned, ok := stats["learned_skills"]
	if !ok {
		learned, ok = stats["nodes"]
		if !ok {
			learned = 0
		}
	}
	get := func(key string) any {
		if v, ok := stats[key]; ok {
			return v
		}
		return 0
	}
	lines := []string{fmt.Sprintf("%s learned skills \u00b7 %s memories \u00b7 %s skill links", pyStr(learned), pyStr(get("memory_nodes")), pyStr(get("related_edges")))}
	var extra []string
	if pyTruthy(stats["memory_skill_edges"]) {
		extra = append(extra, fmt.Sprintf("%s memory\u2194skill links", pyStr(stats["memory_skill_edges"])))
	}
	peak, err := peakDay(payload)
	if err != nil {
		return nil, err
	}
	if peak != "" {
		extra = append(extra, peak)
	}
	if len(extra) > 0 {
		lines = append(lines, strings.Join(extra, " \u00b7 "))
	}
	return lines, nil
}

// Full play through from reveal zero to one plus the static legend, buckets, and summary
func RenderFrames(payload map[string]any, cols, rows, frames int) (map[string]any, error) {
	frames = max(2, min(frames, 240))
	nodes, err := payloadMaps(payload["nodes"])
	if err != nil {
		return nil, err
	}
	var buckets []*chartBucket
	if len(nodes) > 0 {
		if buckets, err = buildChartBuckets(nodes, ComputeRecency(nodes), max(4, rows-3)); err != nil {
			return nil, err
		}
	}
	outFrames := []map[string]any{}
	for i := 0; i < frames; i++ {
		frame, err := RenderGraph(payload, cols, rows, float64(i)/float64(frames-1))
		if err != nil {
			return nil, err
		}
		labels, ok := frame["labels"]
		if !ok {
			labels = []any{}
		}
		outFrames = append(outFrames, map[string]any{"reveal": frame["reveal"], "date": frame["date"], "visible": frame["visible"],
			"grid": frame["grid"], "labels": labels})
	}
	legend, err := BuildLegend(payload)
	if err != nil {
		return nil, err
	}
	categories, err := CategoryLegend(payload, 4)
	if err != nil {
		return nil, err
	}
	rowsOut, err := bucketRows(buckets, payload)
	if err != nil {
		return nil, err
	}
	summary, err := BuildSummary(payload)
	if err != nil {
		return nil, err
	}
	axis, err := AxisLabels(payload)
	if err != nil {
		return nil, err
	}
	return map[string]any{"frames": outFrames, "legend": legend, "categories": categories, "buckets": rowsOut,
		"summary": summary, "axis": axis, "count": len(nodes), "cols": cols, "rows": rows}, nil
}

// Learning graph renderer is pure and registers nothing at import
func registerLearningGraphRender() {}
