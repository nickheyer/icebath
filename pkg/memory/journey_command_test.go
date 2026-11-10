package memory

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func journeyCapture(t *testing.T, ns map[string]any, force bool) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if force {
		ns["force_color"] = true
	}
	var out bytes.Buffer
	code, err := JourneyCommand(context.Background(), &tools.Env{Home: h}, ns, &out)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v output %q", code, err, out.String())
	}
	return out.String()
}

func TestForceColorEmitsAnsiForReemission(t *testing.T) {
	if !strings.Contains(journeyCapture(t, map[string]any{}, true), "\x1b[") {
		t.Fatal("show without ANSI under --force-color")
	}
	if !strings.Contains(journeyCapture(t, map[string]any{"journey_action": "list"}, true), "\x1b[") {
		t.Fatal("list without ANSI under --force-color")
	}
}

func TestDefaultCaptureIsPlainForChatBubbles(t *testing.T) {
	if strings.Contains(journeyCapture(t, map[string]any{}, false), "\x1b[") {
		t.Fatal("show emitted ANSI into a captured stream")
	}
	if strings.Contains(journeyCapture(t, map[string]any{"journey_action": "list"}, false), "\x1b[") {
		t.Fatal("list emitted ANSI into a captured stream")
	}
}

func TestChartedSignalLabelsClearReadableContrastFloor(t *testing.T) {
	palette := map[string]string{"bg": "#08080C", "dim": "#525255", "label": "#A9A9AA", "memory": "#FFDC1F", "skill": "#043D92"}
	payload := map[string]any{
		"nodes": []any{
			map[string]any{"id": "old-skill", "label": "old-skill", "kind": "skill", "timestamp": float64(1_700_000_000), "category": "research", "useCount": float64(0)},
			map[string]any{"id": "new-skill", "label": "new-skill", "kind": "skill", "timestamp": float64(1_800_000_000), "category": "research", "useCount": float64(1)},
		},
		"edges":    []any{},
		"clusters": []any{map[string]any{"category": "research", "count": float64(2)}},
		"stats":    map[string]any{"learned_skills": float64(2), "memory_nodes": float64(0), "related_edges": float64(0), "memory_skill_edges": float64(0)},
	}
	lines, err := journeyFrameLines(context.Background(), palette, payload, 72, 24, 1.0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if !strings.Contains(line.plain(), "old-skill") || line[0].Text != "  " || len(line) < 4 {
			continue
		}
		var style string
		for _, run := range line {
			if run.Text == "old-skill" {
				style = run.Style
			}
		}
		if !strings.HasPrefix(style, "#") {
			t.Fatalf("label style %q", style)
		}
		if ratio := contrastRatio(HexToRGB(style), HexToRGB(palette["bg"])); ratio < chartedSignalMinContrast {
			t.Fatalf("contrast %.2f below the floor", ratio)
		}
		return
	}
	t.Fatal("no charted signal row named old-skill")
}
