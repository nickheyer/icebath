package slack

import (
	"strings"
	"testing"
)

func rawCell(text string) map[string]any { return map[string]any{"type": "raw_text", "text": text} }

func richCell(text string) map[string]any {
	return map[string]any{"type": "rich_text", "elements": []any{map[string]any{"type": "rich_text_section", "elements": []any{
		map[string]any{"type": "text", "text": text, "style": map[string]any{"bold": true}},
	}}}}
}

func tableOf(rows ...any) map[string]any { return map[string]any{"type": "table", "rows": rows} }

func TestRenderHandlesRawRichRaggedAndMalformedCells(t *testing.T) {
	block := tableOf(
		[]any{rawCell("Name"), richCell("Status")},
		"not-a-row",
		[]any{rawCell(""), nil},
		[]any{rawCell("Hermes"), richCell("ok"), map[string]any{"type": "mystery"}},
	)
	if got := renderSlackTableBlock(block, slackTableMaxChars); got != "Name | Status\nHermes | ok | " {
		t.Fatalf("got %q", got)
	}
	if got := renderSlackTableBlock(map[string]any{"type": "table"}, slackTableMaxChars); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := renderSlackTableBlock(map[string]any{"type": "table", "rows": "bad"}, slackTableMaxChars); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderCapsHugeTablesWithVisibleMarker(t *testing.T) {
	rows := make([]any, 10)
	for i := range rows {
		rows[i] = []any{rawCell(strings.Repeat("x", 5000))}
	}
	out := renderSlackTableBlock(tableOf(rows...), slackTableMaxChars)
	if !strings.HasSuffix(out, "[table truncated]") || len([]rune(out)) > slackTableMaxChars {
		t.Fatalf("len %d", len(out))
	}
}

func TestTopLevelTableSurvivesInboundDedupe(t *testing.T) {
	out := extractAdditionalTextFromSlackBlocks([]any{tableOf([]any{rawCell("col1"), rawCell("col2")})}, "intro sentence", "")
	if !strings.Contains(out, "col1 | col2") {
		t.Fatalf("got %q", out)
	}
}

func TestAttachmentNestedTableReachesLiveAndHistoryPaths(t *testing.T) {
	attachments := []any{map[string]any{"blocks": []any{tableOf([]any{rawCell("Item"), rawCell("Qty")})}}}
	if got := appendLinkUnfurls("intro", attachments); !strings.Contains(got, "Item | Qty") {
		t.Fatalf("live %q", got)
	}
	if got := extractTextFromSlackAttachments(attachments); !strings.Contains(got, "Item | Qty") {
		t.Fatalf("history %q", got)
	}
}

func TestSerializerSkipsTableHuskButKeepsOtherBlocks(t *testing.T) {
	blocks := []any{tableOf([]any{rawCell("a")}), map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "hello"}}}
	if got := serializeSlackBlocksForAgent(blocks[:1], 6000); got != "" {
		t.Fatalf("husk %q", got)
	}
	out := serializeSlackBlocksForAgent(blocks, 6000)
	if !strings.Contains(out, "section") || strings.Contains(out, `"table"`) {
		t.Fatalf("got %q", out)
	}
}
