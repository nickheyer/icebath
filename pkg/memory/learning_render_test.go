package memory

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func renderPayload(skills, memories int) map[string]any {
	const baseTS = 1_700_000_000
	nodes := []any{}
	for i := 0; i < skills; i++ {
		category := "research"
		if i%2 == 1 {
			category = "devops"
		}
		nodes = append(nodes, map[string]any{"id": fmt.Sprintf("skill%d", i), "label": fmt.Sprintf("skill%d", i), "kind": "skill",
			"timestamp": int64(baseTS + i*86400*20), "category": category, "useCount": i})
	}
	for j := 0; j < memories; j++ {
		nodes = append(nodes, map[string]any{"id": fmt.Sprintf("memory:memory:%d", j), "label": fmt.Sprintf("mem %d", j), "kind": "memory",
			"timestamp": int64(baseTS + (skills+j)*86400*20), "category": "memory"})
	}
	edges := []any{}
	if skills > 1 {
		edges = append(edges, map[string]any{"source": "skill0", "target": "skill1"})
	}
	return map[string]any{
		"nodes": nodes, "edges": edges,
		"clusters": []any{map[string]any{"category": "devops", "count": skills}, map[string]any{"category": "memory", "count": memories}},
		"stats":    map[string]any{"learned_skills": skills, "memory_nodes": memories, "related_edges": len(edges), "memory_skill_edges": 0},
	}
}

func gridRuns(grid any) [][]any {
	var out [][]any
	for _, row := range grid.([]any) {
		for _, r := range row.([]any) {
			out = append(out, r.([]any))
		}
	}
	return out
}

func TestRecencyIsTimedAndBounded(t *testing.T) {
	nodes, _ := payloadMaps(renderPayload(8, 3)["nodes"])
	rec := ComputeRecency(nodes)
	if !rec.Timed {
		t.Fatal("recency must be timed")
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, r := range rec.Rec {
		if r < LeadIn-1e-9 || r > 1+1e-9 {
			t.Fatalf("ratio %v out of bounds", r)
		}
		lo, hi = math.Min(lo, r), math.Max(hi, r)
	}
	if math.Abs(lo-LeadIn) > 1e-9 || math.Abs(hi-1) > 1e-9 {
		t.Fatalf("min %v max %v", lo, hi)
	}
}

func TestRecencyInkFollowsAgeGradient(t *testing.T) {
	if math.Abs(RecencyInk(0)-AgeOldInk) > 1e-6 || math.Abs(RecencyInk(1)-AgeNewInk) > 1e-6 {
		t.Fatal("ink endpoints must match the age gradient")
	}
	samples := make([]float64, 11)
	for i := range samples {
		samples[i] = RecencyInk(float64(i) / 10)
	}
	if !sort.Float64sAreSorted(samples) {
		t.Fatalf("ink not monotonic: %v", samples)
	}
}

func TestGridRunsAreTextStyleAlpha(t *testing.T) {
	frame, err := RenderGraph(renderPayload(8, 3), 60, 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range gridRuns(frame["grid"]) {
		if len(r) < 3 || len(r) > 4 {
			t.Fatalf("run %v", r)
		}
		text, ok1 := r[0].(string)
		_, ok2 := r[1].(string)
		alpha, ok3 := r[2].(float64)
		if !ok1 || !ok2 || !ok3 || alpha < 0 || alpha > 1 || text == "" {
			t.Fatalf("run %v", r)
		}
		if len(r) == 4 {
			if _, ok := r[3].(string); r[3] != nil && !ok {
				t.Fatalf("run hex %v", r[3])
			}
		}
	}
}

func TestBarsRenderSkillsAndMemories(t *testing.T) {
	frame, err := RenderGraph(renderPayload(10, 4), 72, 18, 1)
	if err != nil {
		t.Fatal(err)
	}
	var flat strings.Builder
	styles := map[any]bool{}
	for _, r := range gridRuns(frame["grid"]) {
		flat.WriteString(r[0].(string))
		styles[r[1]] = true
	}
	if !strings.Contains(flat.String(), "\u2501") || !strings.Contains(flat.String(), MemoryGlyph) {
		t.Fatalf("grid text %q", flat.String())
	}
	if !styles[StyleSkill] || !styles[StyleMemory] {
		t.Fatalf("styles %v", styles)
	}
}

func TestFramesPlayThroughGrowsVisibility(t *testing.T) {
	payload := renderPayload(10, 4)
	out, err := RenderFrames(payload, 50, 16, 12)
	if err != nil {
		t.Fatal(err)
	}
	frames := out["frames"].([]map[string]any)
	if out["count"] != 14 || len(frames) != 12 {
		t.Fatalf("count %v frames %d", out["count"], len(frames))
	}
	if frames[0]["visible"].(int) > frames[11]["visible"].(int) || frames[11]["visible"] != 14 {
		t.Fatalf("visibility %v .. %v", frames[0]["visible"], frames[11]["visible"])
	}
	if _, ok := out["axis"]; !ok {
		t.Fatal("axis missing")
	}
	for _, f := range frames {
		if len(f["grid"].([]any)) == 0 {
			t.Fatal("empty grid")
		}
	}
}

func TestMemoryRowsCarryTheirCardBodyForFingerprintedIDs(t *testing.T) {
	ctx, h := learningHome(t)
	writeTestFile(t, filepath.Join(h, "memories", "MEMORY.md"), []byte("alpha note\n\u00a7\nbeta note"))
	payload, err := BuildLearningGraph(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderFrames(payload, 80, 24, 1)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, b := range out["buckets"].([]map[string]any) {
		for _, n := range b["nodes"].([]map[string]any) {
			bodies = append(bodies, n["body"].(string))
		}
	}
	sort.Strings(bodies)
	if !reflect.DeepEqual(bodies, []string{"alpha note", "beta note"}) {
		t.Fatalf("bodies %v", bodies)
	}
	_ = context.Background()
}

func TestRenderFailsLikePythonOnAnOutOfRangeTimestamp(t *testing.T) {
	payload := map[string]any{"nodes": []any{
		map[string]any{"id": "a", "kind": "skill", "timestamp": 1e13},
		map[string]any{"id": "b", "kind": "skill", "timestamp": 1.0},
	}}
	if _, err := RenderFrames(payload, 80, 16, 2); err == nil {
		t.Fatal("a timestamp past year 9999 must fail the render like fromtimestamp")
	}
}

// Python render_frames output for fixed payloads, gzip and base64 encoded JSON
const renderGoldenB64 = `
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
D2+paPPeqgEA
`

func loadRenderGolden(t *testing.T) map[string]any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(renderGoldenB64), "\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]any
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// First differing path between two decoded JSON values
func jsonDiff(path string, a, b any) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: %#v != %#v", path, a, b)
		}
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		for k := range keys {
			if d := jsonDiff(path+"."+k, x[k], y[k]); d != "" {
				return d
			}
		}
		return ""
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return fmt.Sprintf("%s: %#v != %#v", path, a, b)
		}
		for i := range x {
			if d := jsonDiff(fmt.Sprintf("%s[%d]", path, i), x[i], y[i]); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Sprintf("%s: %#v != %#v", path, a, b)
	}
	return ""
}

func TestRenderFramesMatchesPythonGolden(t *testing.T) {
	golden := loadRenderGolden(t)
	for name, raw := range golden {
		if strings.HasPrefix(name, "_") {
			continue
		}
		c := raw.(map[string]any)
		out, err := RenderFrames(c["payload"].(map[string]any), int(c["cols"].(float64)), int(c["rows"].(float64)), int(c["frames"].(float64)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d := jsonDiff(name, normalizeJSON(t, out), c["want"]); d != "" {
			t.Errorf("render mismatch %s", d)
		}
	}
}

func TestPaletteAndDatesMatchPythonGolden(t *testing.T) {
	golden := loadRenderGolden(t)
	for color, raw := range golden["_palette"].(map[string]any) {
		want := raw.(map[string]any)
		got := map[string]any{"dark": DerivePalette(color, true), "light": DerivePalette(color, false), "rgb": HexToRGB(color)}
		if d := jsonDiff(color, normalizeJSON(t, got), want); d != "" {
			t.Errorf("palette mismatch %s", d)
		}
	}
	inputs := map[string]*float64{}
	for _, v := range []float64{0, 1, 86399.9999996, -1.5, 253402300800, 1_700_000_000.4, math.NaN()} {
		f := v
		inputs[pyFloatRepr(f)] = &f
	}
	for key, want := range golden["_dates"].(map[string]any) {
		ts, ok := inputs[key]
		if !ok {
			t.Fatalf("no input for %s", key)
		}
		if got := FormatDate(ts); got != want {
			t.Errorf("FormatDate(%s) = %q, want %q", key, got, want)
		}
	}
}
