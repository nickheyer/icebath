package memory

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
)

func skillNode(name, category string, related ...string) *SkillNode {
	return &SkillNode{Name: name, Category: category, Source: "profile", State: "active", Related: related}
}

func TestDensityStatsCountIsolatedNodes(t *testing.T) {
	nodes := []*SkillNode{skillNode("a", "x", "b"), skillNode("b", "x", "a"), skillNode("c", "y")}
	stats := DensityStats(nodes, BuildEdges(nodes))
	if stats["nodes"] != 3 || stats["linked_nodes"] != 2 {
		t.Fatalf("stats = %v", stats)
	}
	if stats["isolated_pct"] != pyRound(100.0/3, 1) || stats["isolated_pct"] != 33.3 {
		t.Fatalf("isolated_pct = %v", stats["isolated_pct"])
	}
	if stats["related_edges"] != 1 || stats["edges_per_node"] != 0.333 {
		t.Fatalf("edges = %v per node %v", stats["related_edges"], stats["edges_per_node"])
	}
}

func TestDensityStatsEmptyGraphCountsAsFullyIsolated(t *testing.T) {
	stats := DensityStats(nil, nil)
	if stats["nodes"] != 0 || stats["isolated_pct"] != 100.0 || stats["edges_per_node"] != 0.0 {
		t.Fatalf("stats = %v", stats)
	}
}

func learningHome(t *testing.T) (context.Context, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	return home.WithOverride(context.Background(), h), h
}

func TestMemoryIsCardsSplitOnSeparator(t *testing.T) {
	ctx, h := learningHome(t)
	writeTestFile(t, filepath.Join(h, "memories", "MEMORY.md"), []byte("Project uses pytest with xdist\n\u00a7\nUser prefers concise responses"))
	graph, err := BuildLearningGraph(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cards := graph["memory"].([]map[string]any)
	titles := map[any]bool{}
	for _, c := range cards {
		titles[c["title"]] = true
		if c["source"] != "memory" && c["source"] != "profile" {
			t.Fatalf("card source %v", c["source"])
		}
		if _, ok := c["timestamp"]; !ok {
			t.Fatal("card without timestamp")
		}
	}
	if !titles["Project uses pytest with xdist"] || !titles["User prefers concise responses"] {
		t.Fatalf("titles = %v", titles)
	}
	found := false
	for _, n := range graph["nodes"].([]map[string]any) {
		if n["kind"] == "memory" {
			found = true
		}
	}
	if !found {
		t.Fatal("no memory kind node")
	}
}

func TestFullPayloadShapeAndEdgeIntegrity(t *testing.T) {
	ctx, _ := learningHome(t)
	graph, err := BuildLearningGraph(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes := graph["nodes"].([]map[string]any)
	ids := map[any]bool{}
	for _, n := range nodes {
		ids[n["id"]] = true
	}
	for _, e := range graph["edges"].([]map[string]any) {
		if !ids[e["source"]] || !ids[e["target"]] {
			t.Fatalf("dangling edge %v", e)
		}
	}
	cats := map[any]bool{}
	for _, c := range graph["clusters"].([]map[string]any) {
		cats[c["category"]] = true
	}
	skillCount := 0
	for _, n := range nodes {
		if !cats[n["category"]] {
			t.Fatalf("category %v missing from clusters", n["category"])
		}
		if _, ok := n["timestamp"]; !ok {
			t.Fatal("node without timestamp")
		}
		if n["kind"] == "skill" {
			skillCount++
		}
	}
	stats := graph["stats"].(map[string]any)
	if stats["nodes"] != skillCount || stats["memory_nodes"] != len(graph["memory"].([]map[string]any)) {
		t.Fatalf("stats = %v", stats)
	}
}

func TestBuildSkillNodesSkipsProfileInstallSwapLeftover(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	writeTestFile(t, filepath.Join(root, "demo", "SKILL.md"), []byte("---\nname: demo-new\n---\n"))
	writeTestFile(t, filepath.Join(root, skills.DistSwapDir, "demo.old-1", "SKILL.md"), []byte("---\nname: demo-old\n---\n"))
	nodes, err := BuildSkillNodes([]SkillRoot{{Source: "profile", Dir: root}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "demo-new" {
		t.Fatalf("nodes = %v", nodes)
	}
}

func TestForegroundCreatedSkillIsInJourneyBeforeFirstUse(t *testing.T) {
	ctx, h := learningHome(t)
	for _, name := range []string{"fresh-learn-skill", "hand-written"} {
		writeTestFile(t, filepath.Join(h, "skills", "demo", name, "SKILL.md"),
			[]byte("---\nname: "+name+"\ndescription: d.\n---\n\n# "+name+"\n"))
	}
	writeTestFile(t, filepath.Join(h, "skills", ".usage.json"), []byte(`{"fresh-learn-skill": {"use_count": 0, "created_by": "learn", "state": "active"}}`))
	graph, err := BuildLearningGraph(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[any]bool{}
	for _, n := range graph["nodes"].([]map[string]any) {
		if n["kind"] == "skill" {
			ids[n["id"]] = true
		}
	}
	if !ids["fresh-learn-skill"] || ids["hand-written"] {
		t.Fatalf("skill ids = %v", ids)
	}
}

func TestBundledSkillNamesStayOutOfTheJourney(t *testing.T) {
	ctx, h := learningHome(t)
	nodes, err := BuildSkillNodes([]SkillRoot{{Source: "base", FS: skills.BundledFS()}}, nil, nil)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("bundled nodes = %d, %v", len(nodes), err)
	}
	name := nodes[0].Name
	writeTestFile(t, filepath.Join(h, "skills", "copy", "SKILL.md"), []byte("---\nname: "+name+"\n---\n"))
	writeTestFile(t, filepath.Join(h, "skills", ".usage.json"), []byte(`{"`+name+`": {"use_count": 4, "created_by": "agent"}}`))
	graph, err := BuildLearningGraph(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range graph["nodes"].([]map[string]any) {
		if n["id"] == name {
			t.Fatalf("bundled skill %s entered the journey", name)
		}
	}
}

func TestMemorySkillEdgesScoreNameHitsAndTokens(t *testing.T) {
	cards := []map[string]any{{"source": "memory", "fingerprint": "abc", "title": "Use pytest-runner for tests", "body": "pytest runner xdist"}}
	nodes := []*SkillNode{skillNode("pytest-runner", "dev"), skillNode("xdist-tools", "dev"), skillNode("unrelated", "dev")}
	edges := memorySkillEdges(cards, nodes)
	if len(edges) != 2 || edges[0] != [2]string{"memory:memory:0:abc", "pytest-runner"} || edges[1][1] != "xdist-tools" {
		t.Fatalf("edges = %v", edges)
	}
}

func TestMemoryFingerprintDigestsStrippedText(t *testing.T) {
	if MemoryFingerprint("  hello \n") != MemoryFingerprint("hello") || len(MemoryFingerprint("x")) != 12 {
		t.Fatal("fingerprint must digest the stripped text into 12 hex characters")
	}
	if MemoryFingerprint("hello") != "2cf24dba5fb0" {
		t.Fatalf("fingerprint = %s", MemoryFingerprint("hello"))
	}
}

func TestToIntTimestampMatchesPython(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		{nil, nil}, {"", nil}, {"  ", nil}, {true, int64(1)}, {false, int64(0)}, {12, int64(12)}, {12.9, int64(12)},
		{-12.9, int64(-12)}, {math.NaN(), nil}, {math.Inf(1), nil}, {"12", int64(12)}, {" 12.7 ", int64(12)},
		{"1e3", int64(1000)}, {"1_000", int64(1000)}, {"0x10", nil}, {"inf", nil}, {"nan", nil}, {"-inf", nil},
		{"+12", int64(12)}, {".5", int64(0)}, {"5.", int64(5)}, {"1__0", nil}, {"\u0661\u0662", int64(12)},
		{"2024-01-01T00:00:00Z", int64(1704067200)}, {"2024-01-01", int64(1704067200)}, {"garbage", nil}, {"12abc", nil},
	}
	for _, c := range cases {
		if got := toIntTimestamp(c.in); got != c.want {
			t.Errorf("toIntTimestamp(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestFromISOFormatMatchesCPython(t *testing.T) {
	cases := []struct {
		in string
		ok bool
		ts int64
	}{
		{"2024-01-01T12:30:45.", false, 0},
		{"2024-01-01 ", false, 0},
		{"2024-W01", true, 1704067200},
		{"2024W011", true, 1704067200},
		{"2024-01-01T12", true, 1704110400},
		{"2024-01-01T1230", true, 1704112200},
		{"2024-01-01T12:30:45+0530", true, 1704092445},
		{"2024-01-01T12:30:45.123456789Z", true, 1704112245},
		{"2024-01-01x12:00", true, 1704110400},
		{"2024-01-01T24:00", true, 1704153600},
		{"20240101", true, 1704067200},
		{"2024-01-01T12:30:45+05:30:15.5", true, 1704092429},
		{"2024-01-01T12:30,5", false, 0},
		{"2024-12-31T24:00:00", true, 1735689600},
		{"9999-12-31T24:00", false, 0},
		{"2024-01-01T24:00:01", false, 0},
		{"2024-02-30", false, 0},
		{"0000-01-01", false, 0},
		{"2024-13-01", false, 0},
		{"2020-W53-7", true, 1609632000},
		{"2021-W53", false, 0},
		{"2024-W01-8", false, 0},
		{"2024-01-01T12:30:45Z", true, 1704112245},
		{"2024-01-01T12:30:45z", false, 0},
		{"2024-01-01T12:30:45-08:00", true, 1704141045},
		{"2024-01-01T12:30:45+24:00", false, 0},
		{"2024-01-01T12:30:45+23:59", true, 1704025905},
		{"2024-01-01T12:30:45.5", true, 1704112245},
		{"2024-01-01T12:30:45,25", true, 1704112245},
		{"2024-01-01T12:3", false, 0},
		{"2024-01-01T123045", true, 1704112245},
		{"2024-01-01T12:3045", false, 0},
		{"2024-01-01T12:30:45+05", true, 1704094245},
		{"2024-01-01T12:30:45+5", false, 0},
		{"2024-01-01T12:30:45+05:3", false, 0},
		{"2024-01-01T12:30:45Zjunk", false, 0},
		{"2024-01-01T12:30:45 ", false, 0},
		{"2024-0101", false, 0},
		{"202401-01", false, 0},
		{"2024-01-01T12:60", false, 0},
		{"2024-01-01T12:30:60", false, 0},
		{"2025-04-05T06:07:08.000009+00:00", true, 1743833228},
		{"1970-01-01T00:00:00.5-00:00", true, 0},
		{"1969-12-31T23:59:59.5Z", true, 0},
		{"2024W01", true, 1704067200},
		{"2024-W011", false, 0},
		{"2024-01-01T12.5", false, 0},
		{"2024-01-01T12:30.5", false, 0},
		{"2024-01-01T12:30:45.+00:00", false, 0},
		{"2024-01-01T12:30:45.1234567+01:00", true, 1704108645},
		{"2011-11-04T00:05:23.283+00:00", true, 1320365123},
		{"2024-01-01\u00e912:00", true, 1704110400},
		{"20240101\u00e91200", true, 1704110400},
		{"2024W01\u00e912", true, 1704110400},
		{"2024-01-01T12:00\u00e9", false, 0},
		{"2024-01\u00e901", false, 0},
		{"2024-01-01\u00e9", false, 0},
		{"2024-01-01\U0001F60012:00", true, 1704110400},
		{"2024-W01-2\u00e912", true, 1704196800},
		{"2024-01-01T12:30:45:+00:00", true, 1704112245},
		{"2024-01-01T12:30:45x+00:00", true, 1704112245},
		{"2024-01-01T12:30:+00:00", true, 1704112200},
		{"2024-01-01T12:30x+00:00", true, 1704112200},
		{"2024-01-01T12:+00:00", true, 1704110400},
		{"2024-01-01T12x+00:00", true, 1704110400},
		{"2024-01-01T12:30:45.1+00:00", true, 1704112245},
		{"2024-01-01T12:30:45,+00:00", false, 0},
		{"2024-01-01T12:30:45:", false, 0},
		{"2024-01-01T12:30:", false, 0},
		{"2024-01-01T12:", false, 0},
		{"2024-01-01T12:30:45x", false, 0},
		{"2024-01-01T12:30:45+05:", false, 0},
		{"2024-01-01T12:30:45+05:30:", false, 0},
		{"2024-01-01T12:30:45+05:30.", false, 0},
		{"2024-01-01T12:30:45+05:30:00.", false, 0},
		{"2024-01-01T12:30:45+05:30x", false, 0},
		{"2024-01-01T1230+0500", true, 1704094200},
		{"2024-01-01T12:30:45.123x", false, 0},
		{"2024-01-01T12:30:45.123+", false, 0},
		{"2024-01-01T12:30:45+", false, 0},
		{"2024-01-01T12:30:45Z+", false, 0},
	}
	for _, c := range cases {
		got, err := pyFromISOFormat(c.in)
		if (err == nil) != c.ok {
			t.Errorf("pyFromISOFormat(%q) err = %v, want ok %v", c.in, err, c.ok)
			continue
		}
		if c.ok && isoEpochSeconds(got) != c.ts {
			t.Errorf("pyFromISOFormat(%q) = %d, want %d", c.in, isoEpochSeconds(got), c.ts)
		}
	}
}

func TestUTF8DecodeErrorsMatchCPython(t *testing.T) {
	cases := map[string]string{
		"\xef\xbb\xbfab\xff": "'utf-8' codec can't decode byte 0xff in position 2: invalid start byte",
		"ab\xe2\x82":         "'utf-8' codec can't decode bytes in position 2-3: unexpected end of data",
		"\xe2":               "'utf-8' codec can't decode byte 0xe2 in position 0: unexpected end of data",
		"\xe2(\xa1":          "'utf-8' codec can't decode byte 0xe2 in position 0: invalid continuation byte",
		"\xf0\x90(":          "'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte",
		"\xed\xa0\x80":       "'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte",
		"\xe0\x80":           "'utf-8' codec can't decode byte 0xe0 in position 0: invalid continuation byte",
		"\xc0\xaf":           "'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte",
	}
	for in, want := range cases {
		_, err := decodeUTF8Sig([]byte(in))
		if err == nil || err.Error() != want {
			t.Errorf("decodeUTF8Sig(%q) = %v, want %s", in, err, want)
		}
	}
	if text, err := decodeUTF8Sig([]byte("\xef\xbb\xbfok")); err != nil || text != "ok" {
		t.Fatalf("BOM text = %q, %v", text, err)
	}
}
