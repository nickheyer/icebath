package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/skills"
)

func TestSlashInvokedSkillIsMarkedAndQuotedAsTyped(t *testing.T) {
	ctx := context.Background()
	scope := skills.NewScope(t.TempDir(), config.Defaults())
	note := `[IMPORTANT: The user has invoked the "probe-inv" skill, indicating they want ` +
		"you to follow its instructions. The full skill content is loaded below.]"
	body := "RULE: include INV-HERON.\n" + strings.Repeat("skill body prose. ", 600)
	invocation := scope.BuildSkillMessage(ctx, map[string]any{"name": "probe-inv", "content": body}, "/skills/probe-inv", note, "remember PLUM-77", "", "")
	auto := scope.BuildSkillMessage(ctx, map[string]any{
		"name": "gw-skill", "content": "gateway skill body", "linked_files": map[string]any{"references": []any{"references/gw.md"}},
		"setup_needed": true, "setup_note": "First setup step.\nSecond setup step.",
	}, "/skills/gw-skill", `[IMPORTANT: The "gw-skill" skill is auto-loaded. Follow its instructions for this session.]`, "", "", "") + "\n\nhello from the gateway"
	c := ghostCompressor(t, func(o *ContextCompressorOptions) { o.TailMode = "lean" })
	msgs := []map[string]any{
		{"role": "system", "content": "System prompt"},
		{"role": "user", "content": "start"},
		{"role": "assistant", "content": "ok"},
		{"role": "user", "content": auto},
		{"role": "assistant", "content": "hi"},
		{"role": "user", "content": invocation},
		{"role": "assistant", "content": "INV-HERON noted. " + strings.Repeat("w", 400)},
		{"role": "user", "content": "continue " + strings.Repeat("c", 400)},
		{"role": "assistant", "content": "more work " + strings.Repeat("z", 500)},
		{"role": "user", "content": "latest ask"},
		{"role": "assistant", "content": "ack"},
	}
	patchTailCut(t, 9)
	patchCallLLM(t, func(map[string]any) (any, error) { return contentResponse("## Goal\nWork."), nil })
	result, err := c.CompressContext(ctx, msgs, nil, nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	summary := ghostSummaryText(t, result)
	for _, want := range []string{skillPrunedMarker("probe-inv"), "/probe-inv remember PLUM-77", skillPrunedMarker("gw-skill"), "> hello from the gateway"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary lacks %q\n%s", want, summary)
		}
	}
	for _, banned := range []string{"skill body prose", "scripts/foo.js", "references/gw.md"} {
		if strings.Contains(summary, banned) {
			t.Fatalf("summary carries %q\n%s", banned, summary)
		}
	}
}
