package slack

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func textOf(e map[string]any) string { s, _ := e["text"].(string); return s }

func styleFlag(e map[string]any, key string) bool { return asMap(e["style"])[key] == true }

func firstRich(blocks []any) map[string]any {
	for _, b := range blocks {
		if asMap(b)["type"] == "rich_text" {
			return asMap(b)
		}
	}
	return nil
}

func itemElements(blocks []any) []map[string]any {
	var out []map[string]any
	for _, e := range asList(asMap(asList(asMap(asList(firstRich(blocks)["elements"])[0])["elements"])[0])["elements"]) {
		out = append(out, asMap(e))
	}
	return out
}

func hasElem(els []map[string]any, want map[string]any) bool {
	for _, e := range els {
		if reflect.DeepEqual(e, want) {
			return true
		}
	}
	return false
}

func TestRenderBlocksBasics(t *testing.T) {
	if RenderBlocks("", nil) != nil || RenderBlocks("   \n  ", nil) != nil {
		t.Fatal("empty rendered")
	}
	b := asMap(RenderBlocks("# Title", nil)[0])
	if b["type"] != "header" || asMap(b["text"])["type"] != "plain_text" || asMap(b["text"])["text"] != "Title" {
		t.Fatalf("%+v", b)
	}
}

func TestNestedBulletsProduceIncreasingIndent(t *testing.T) {
	var indents []int
	for _, e := range asList(firstRich(RenderBlocks("- a\n  - b\n    - c", nil))["elements"]) {
		if asMap(e)["type"] == "rich_text_list" {
			indents = append(indents, asMap(e)["indent"].(int))
		}
	}
	if len(indents) != 3 || indents[0] != 0 || indents[1] != 1 || indents[2] != 2 {
		t.Fatal(indents)
	}
}

func TestSlackMrkdwnLinkInBulletBecomesLinkElement(t *testing.T) {
	els := itemElements(RenderBlocks("- <https://example.com/x|GitLab #1> — allow `in_progress`", nil))
	var links []map[string]any
	code := false
	for _, e := range els {
		if e["type"] == "link" {
			links = append(links, e)
		}
		if strings.Contains(textOf(e), "<https://") {
			t.Fatal("raw link left")
		}
		code = code || styleFlag(e, "code")
	}
	if len(links) != 1 || links[0]["url"] != "https://example.com/x" || links[0]["text"] != "GitLab #1" || !code {
		t.Fatalf("%+v", els)
	}
}

func TestSlackMentionsInBulletAreNotLinks(t *testing.T) {
	els := itemElements(RenderBlocks("- ping <@U123> in <#C456>", nil))
	for _, e := range els {
		if e["type"] == "link" || strings.Contains(textOf(e), "<@") || strings.Contains(textOf(e), "<#") {
			t.Fatalf("%+v", els)
		}
	}
	if !hasElem(els, map[string]any{"type": "user", "user_id": "U123"}) || !hasElem(els, map[string]any{"type": "channel", "channel_id": "C456"}) {
		t.Fatalf("%+v", els)
	}
}

func TestBlankLineSeparatedOrderedItemsStayInOneList(t *testing.T) {
	var lists []map[string]any
	for _, e := range asList(firstRich(RenderBlocks("1. alpha\n\n1. beta\n\n1. gamma", nil))["elements"]) {
		if asMap(e)["type"] == "rich_text_list" {
			lists = append(lists, asMap(e))
		}
	}
	if len(lists) != 1 || len(asList(lists[0]["elements"])) != 3 {
		t.Fatalf("%+v", lists)
	}
}

func TestSlackMentionElements(t *testing.T) {
	els := itemElements(RenderBlocks("- see <#C0C6N2RF5HS|general>", nil))
	if !hasElem(els, map[string]any{"type": "channel", "channel_id": "C0C6N2RF5HS"}) || strings.Contains(fmt.Sprint(els), "general") {
		t.Fatalf("%+v", els)
	}
	var ranges []string
	for _, e := range itemElements(RenderBlocks("- <!here> <!channel> <!everyone>", nil)) {
		if e["type"] == "broadcast" {
			ranges = append(ranges, e["range"].(string))
		}
	}
	if strings.Join(ranges, ",") != "here,channel,everyone" {
		t.Fatal(ranges)
	}
	els = itemElements(RenderBlocks("- notify <!subteam^S0123ABC|@marketing> and <!subteam^S987XYZ>", nil))
	if !hasElem(els, map[string]any{"type": "usergroup", "usergroup_id": "S0123ABC"}) || !hasElem(els, map[string]any{"type": "usergroup", "usergroup_id": "S987XYZ"}) || strings.Contains(fmt.Sprint(els), "marketing") {
		t.Fatalf("%+v", els)
	}
	els = itemElements(RenderBlocks("- see <#C1|dev_ops_alerts> and <!subteam^S1|@eng_team_x>", nil))
	if !hasElem(els, map[string]any{"type": "channel", "channel_id": "C1"}) || !hasElem(els, map[string]any{"type": "usergroup", "usergroup_id": "S1"}) {
		t.Fatalf("%+v", els)
	}
	for _, e := range els {
		if e["style"] != nil || strings.Contains(textOf(e), "<") {
			t.Fatalf("%+v", els)
		}
	}
	quote := asMap(asList(asMap(RenderBlocks("> ping <@U123> now", nil)[0])["elements"])[0])
	var qels []map[string]any
	for _, e := range asList(quote["elements"]) {
		qels = append(qels, asMap(e))
	}
	if quote["type"] != "rich_text_quote" || !hasElem(qels, map[string]any{"type": "user", "user_id": "U123"}) {
		t.Fatalf("%+v", quote)
	}
	table := asMap(RenderBlocks("| who | note |\n| --- | --- |\n| <@U123> | ok |", nil)[0])
	cell := asMap(asList(asList(table["rows"])[1])[0])
	var cels []map[string]any
	for _, e := range asList(asMap(asList(cell["elements"])[0])["elements"]) {
		cels = append(cels, asMap(e))
	}
	if table["type"] != "table" || !hasElem(cels, map[string]any{"type": "user", "user_id": "U123"}) {
		t.Fatalf("%+v", table)
	}
	joined := ""
	for _, e := range itemElements(RenderBlocks("- a <b> c", nil)) {
		joined += textOf(e)
	}
	if !strings.Contains(joined, "a <b> c") {
		t.Fatal(joined)
	}
}

func TestMentionInsideEmphasisPreservesStylingAndMention(t *testing.T) {
	for _, tc := range []struct {
		md, marker, style, before, after string
		mention                          map[string]any
	}{
		{"- **ping <@U123> now**", "**", "bold", "ping ", " now", map[string]any{"type": "user", "user_id": "U123"}},
		{"- *ping <#C456>*", "*", "italic", "ping ", "", map[string]any{"type": "channel", "channel_id": "C456"}},
		{"- ~~hi <!here>~~", "~~", "strike", "hi ", "", map[string]any{"type": "broadcast", "range": "here"}},
	} {
		els := itemElements(RenderBlocks(tc.md, nil))
		gotBefore, gotAfter := false, tc.after == ""
		for _, e := range els {
			if strings.Contains(textOf(e), tc.marker) {
				t.Fatalf("%s: %+v", tc.md, els)
			}
			gotBefore = gotBefore || (textOf(e) == tc.before && styleFlag(e, tc.style))
			gotAfter = gotAfter || (textOf(e) == tc.after && styleFlag(e, tc.style))
		}
		if !hasElem(els, tc.mention) || !gotBefore || !gotAfter {
			t.Fatalf("%s: %+v", tc.md, els)
		}
	}
}

func TestTables(t *testing.T) {
	blocks := RenderBlocks("| Name | Status |\n|------|--------|\n| a | ok |\n| b | fail |", nil)
	rows := asList(asMap(blocks[0])["rows"])
	if len(blocks) != 1 || asMap(blocks[0])["type"] != "table" || len(rows) != 3 || strings.Count(fmt.Sprint(rows[0]), "Name") != 1 || !strings.Contains(fmt.Sprint(rows[2]), "fail") {
		t.Fatalf("%+v", blocks)
	}
	for _, r := range rows {
		if len(asList(r)) != 2 {
			t.Fatal(r)
		}
	}
	var big []string
	for i := 0; i < 120; i++ {
		big = append(big, fmt.Sprintf("| x%d | y |", i))
	}
	b := asMap(RenderBlocks("| a | b |\n|---|---|\n"+strings.Join(big, "\n"), nil)[0])
	if b["type"] != "rich_text" || asMap(asList(b["elements"])[0])["type"] != "rich_text_preformatted" {
		t.Fatalf("%+v", b)
	}
	b = asMap(RenderBlocks("| Expr | Meaning |\n|------|--------|\n| a \\| b | or |", nil)[0])
	body := asList(asList(b["rows"])[1])
	if b["type"] != "table" || len(body) != 2 || !strings.Contains(fmt.Sprint(body[0]), "|") {
		t.Fatalf("%+v", b)
	}
	if RenderBlocks(strings.Repeat("---\n\n", MaxBlocks+10), nil) != nil {
		t.Fatal("too many blocks rendered")
	}
}

func assertSchemaValid(t *testing.T, o any) {
	t.Helper()
	switch v := o.(type) {
	case map[string]any:
		switch v["type"] {
		case "rich_text_section", "rich_text_preformatted", "rich_text_quote":
			if len(asList(v["elements"])) == 0 {
				t.Fatalf("empty %v elements", v["type"])
			}
		case "text":
			if textOf(v) == "" {
				t.Fatal("zero-length text element")
			}
		case "header":
			if textOf(asMap(v["text"])) == "" {
				t.Fatal("empty plain_text header")
			}
		}
		for _, x := range v {
			assertSchemaValid(t, x)
		}
	case []any:
		for _, x := range v {
			assertSchemaValid(t, x)
		}
	}
}

func TestEmptyContentGuards(t *testing.T) {
	blocks := RenderBlocks("| x | y | z |\n| --- | --- | --- |\n| 1 |  | 3 |\n| 4 |", nil)
	if asMap(blocks[0])["type"] != "table" {
		t.Fatalf("%+v", blocks)
	}
	assertSchemaValid(t, blocks)
	blocks = RenderBlocks("```\n```\n\n> \n\n- \n- real item", nil)
	if blocks == nil {
		t.Fatal("nil")
	}
	assertSchemaValid(t, blocks)
	if headerBlock("***") != nil || RenderBlocks("# ***", nil) != nil {
		t.Fatal("emphasis only header kept")
	}
}

func TestSanitizeBlocks(t *testing.T) {
	out := SanitizeBlocks([]any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": strings.Repeat("x", 3500)}}})
	txt := textOf(asMap(asMap(out[0])["text"]))
	if len([]rune(txt)) > MaxSectionText || !strings.HasSuffix(txt, "…") {
		t.Fatal(len(txt))
	}
	ctxBlock := map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": "✅ ok"}}}
	out = SanitizeBlocks([]any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": strings.Repeat("a", 2990) + strings.Repeat("&amp;", 10)}}, ctxBlock})
	if len([]rune(textOf(asMap(asMap(out[0])["text"])))) > MaxSectionText || !reflect.DeepEqual(out[1], ctxBlock) {
		t.Fatalf("%+v", out[1])
	}
	out = SanitizeBlocks([]any{map[string]any{"type": "table", "rows": []any{[]any{map[string]any{"type": "rich_text", "elements": []any{}}}}, "column_settings": []any{nil, map[string]any{"align": "center"}, nil}}})
	if !reflect.DeepEqual(asMap(out[0])["column_settings"], []any{map[string]any{}, map[string]any{"align": "center"}}) {
		t.Fatalf("%+v", out[0])
	}
	out = SanitizeBlocks([]any{map[string]any{"type": "table", "rows": []any{[]any{map[string]any{"type": "rich_text", "elements": []any{}}}}, "column_settings": []any{nil, nil}}})
	if _, ok := asMap(out[0])["column_settings"]; ok {
		t.Fatalf("%+v", out[0])
	}
	if SanitizeBlocks([]any{"x", map[string]any{"type": "section", "text": map[string]any{"text": "  "}}, map[string]any{"type": "rich_text", "elements": []any{}}}) != nil {
		t.Fatal("empty blocks kept")
	}
	if SanitizeBlocks([]any{map[string]any{"type": "header", "text": map[string]any{"text": 5}}}) != nil {
		t.Fatal("non string header text did not abort")
	}
}

func TestSplitTextFenceBalanced(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = strings.Repeat("y", 20)
	}
	chunks := splitText("```\n"+strings.Join(lines, "\n")+"\n```", 100)
	if len(chunks) < 2 {
		t.Fatal(chunks)
	}
	for i, c := range chunks {
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("chunk %d has unbalanced fences: %q", i, c)
		}
	}
}
