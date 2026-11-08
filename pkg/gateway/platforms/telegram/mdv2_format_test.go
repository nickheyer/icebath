package telegram

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"strings"
	"testing"
)

// Fails unless every wanted substring is present and every refused one absent
func assertFormat(t *testing.T, input string, want, refuse []string) string {
	t.Helper()
	out := FormatMessage(input)
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("%q missing %q in %q", input, w, out)
		}
	}
	for _, r := range refuse {
		if strings.Contains(out, r) {
			t.Fatalf("%q has %q in %q", input, r, out)
		}
	}
	return out
}

func TestFormatMessageBasicPlainTextSpecialsEscaped(t *testing.T) {
	assertFormat(t, "Price is $5.00!", []string{`\.`, `\!`}, nil)
}

func TestFormatMessageCodeBlocksRepeatedFencedToolProgressStaysACodeBlock(t *testing.T) {
	rendered := "\U0001F4BB terminal\n```\nssh -F /opt/data/home/.ssh/config fed...\n```\n(\u00d74)"
	assertFormat(t, rendered, []string{"```\nssh -F /opt/data/home/.ssh/config fed...\n```"}, []string{"\\`\\`\\`"})
}

func TestFormatMessageCodeBlocksCodeBlockSpecialCharsNotEscaped(t *testing.T) {
	assertFormat(t, "```\nif (x > 0) { return !x; }\n```", []string{"if (x > 0) { return !x; }"}, nil)
}

func TestFormatMessageCodeBlocksInlineCodeSpecialCharsNotEscaped(t *testing.T) {
	assertFormat(t, "Run `rm -rf ./*` carefully", []string{"`rm -rf ./*`"}, nil)
}

func TestFormatMessageCodeBlocksInlineCodeBackslashesEscaped(t *testing.T) {
	assertFormat(t, "Check `C:\\ProgramData\\VMware\\` path", []string{"`C:\\\\ProgramData\\\\VMware\\\\`"}, nil)
}

func TestFormatMessageCodeBlocksFencedCodeBlockBackslashesEscaped(t *testing.T) {
	assertFormat(t, "```\npath = r'C:\\Users\\test'\n```", []string{`C:\\Users\\test`}, nil)
}

func TestFormatMessageCodeBlocksFencedCodeBlockBackticksEscaped(t *testing.T) {
	assertFormat(t, "```\necho `hostname`\n```", []string{"echo \\`hostname\\`"}, nil)
}

func TestFormatMessageCodeBlocksInlineCodeNoDoubleEscape(t *testing.T) {
	assertFormat(t, "Use `\\\\server\\share`", []string{"`\\\\\\\\server\\\\share`"}, nil)
}

func TestFormatMessageCodeBlocksInlineTripleBackticksNotTreatedAsFence(t *testing.T) {
	assertFormat(t, "the syntax is ```like this``` inline", []string{"like this"}, []string{"```"})
}

func TestFormatMessageCodeBlocksFenceAndInlineBackticksMixed(t *testing.T) {
	out := assertFormat(t, "```\nx = 1\n```\nand inline ```y``` after", []string{"```\nx = 1\n```", "y"}, nil)
	if strings.Count(out, "```") != 2 {
		t.Fatalf("fences %q", out)
	}
}

func TestFormatMessageCodeBlocksFenceWithTrailingWhitespaceOnClose(t *testing.T) {
	assertFormat(t, "```\ncode\n```   ", []string{"code"}, []string{"``````"})
}

func TestFormatMessageCodeBlocksFenceWithCrlfLineEndingsIsProtected(t *testing.T) {
	assertFormat(t, "```\r\ncode\r\n```\r\n", []string{"code"}, []string{"\\`"})
}

func TestFormatMessageCodeBlocksMidlineOpenedFenceStaysProtected(t *testing.T) {
	assertFormat(t, "Here is the code: ```python\nprint('hi')\n```", []string{"```python\nprint('hi')\n```"}, []string{"\\`"})
}

func TestFormatMessageCodeBlocksListNestedIndentedFenceStaysProtected(t *testing.T) {
	assertFormat(t, "- item:\n    ```\n    code\n    ```", []string{"    ```\n    code\n    ```"}, []string{"\\`"})
}

func TestFormatMessageCodeBlocksMidlineFenceAfterInlineCodeLeadIn(t *testing.T) {
	assertFormat(t, "use `foo` then: ```python\nx\n```", []string{"```python\nx\n```", "`foo`"}, []string{"\\`"})
}

func TestFormatMessageCodeBlocksInlinePairThenRealFenceBothHandled(t *testing.T) {
	out := assertFormat(t, "the syntax is ```like this``` inline\n```python\nx\n```", []string{"```python\nx\n```", "like this"}, nil)
	if strings.Count(out, "```") != 2 {
		t.Fatalf("fences %q", out)
	}
}

func TestFormatMessageBoldItalicBoldConverted(t *testing.T) {
	assertFormat(t, "This is **bold** text", []string{"*bold*"}, []string{"**"})
}

func TestFormatMessageBoldItalicReloadMcpSummaryEscapesDynamicServerNames(t *testing.T) {
	content := "\U0001F504 **MCP Servers Reloaded**\n\u267b\ufe0f Reconnected: agent_one, tool[beta]\n\u2795 Added: alpha*prod\n\U0001F527 3 tool(s) available from 2 server(s)"
	assertFormat(t, content, []string{"*MCP Servers Reloaded*", `agent\_one`, `tool\[beta\]`, `alpha\*prod`}, nil)
}

func TestFormatMessageHeadersH1ConvertedToBold(t *testing.T) {
	assertFormat(t, "# Title", []string{"*Title*"}, []string{"#"})
}

func TestFormatMessageHeadersHeaderWithInnerBoldStripped(t *testing.T) {
	if out := assertFormat(t, "## **Important**", []string{"*Important*"}, nil); strings.Count(out, "*") != 2 {
		t.Fatalf("stars %q", out)
	}
}

func TestFormatMessageLinksLinkDisplayTextEscaped(t *testing.T) {
	assertFormat(t, "[Hello!](https://example.com)", []string{`Hello\!`}, nil)
}

func TestFormatMessageLinksLinkUrlParenthesesEscaped(t *testing.T) {
	assertFormat(t, "[link](https://example.com/path_(1))", []string{`\)`}, nil)
}

func TestItalicNewlineBugBulletListNotCorrupted(t *testing.T) {
	out := assertFormat(t, "* Item one\n* Item two\n* Item three", []string{"Item one", "Item two", "Item three"}, nil)
	if strings.Contains(out, "_") && strings.Contains(strings.Split(out, "_")[1], "Item") {
		t.Fatalf("italic spans lines %q", out)
	}
}

func TestFormatMessageStrikethroughStrikethroughConverted(t *testing.T) {
	assertFormat(t, "This is ~~deleted~~ text", []string{"~deleted~"}, []string{"~~"})
}

func TestFormatMessageSpoilerSpoilerPipesNotEscaped(t *testing.T) {
	assertFormat(t, "||secret||", []string{"||secret||"}, []string{`\|\|`})
}

func TestFormatMessageBlockquoteBlockquoteMultiline(t *testing.T) {
	assertFormat(t, "> Line one\n> Line two", []string{"> Line one", "> Line two"}, []string{`\>`})
}

func TestFormatMessageBlockquoteGtInMiddleOfLineStillEscaped(t *testing.T) {
	assertFormat(t, "5 > 3", []string{`\>`}, nil)
}

func TestFormatMessageBlockquoteRegularBlockquoteWithPipesEscaped(t *testing.T) {
	assertFormat(t, "> not expandable||", []string{"> not expandable", `\|`}, []string{`\>`})
}

func TestFormatMessageComplexCodeBlockWithBoldOutside(t *testing.T) {
	out := assertFormat(t, "**Note:**\n```\ncode here\n```", []string{"```\ncode here\n```"}, nil)
	if !strings.Contains(out, "*Note:*") && !strings.Contains(out, `*Note\:*`) {
		t.Fatalf("bold %q", out)
	}
}

func TestFormatMessageComplexBoldInsideCodeNotConverted(t *testing.T) {
	assertFormat(t, "```\n**not bold**\n```", []string{"**not bold**"}, nil)
}

func TestFormatMessageComplexEmptyBold(t *testing.T) {
	_ = FormatMessage("****")
}

func TestFormatMessageTablesTableRenderedAsBullets(t *testing.T) {
	assertFormat(t, "Data:\n\n| Col1 | Col2 |\n|------|------|\n| A    | B    |\n", []string{"*A*", "\u2022 Col2: B"}, []string{"\u2022 Col1: A", "```", `\|`})
}

func TestWrapMarkdownTablesBasicTableRewrittenAsRowGroups(t *testing.T) {
	text := "Scores:\n\n| Player | Score |\n|--------|-------|\n| Alice  | 150   |\n| Bob    | 120   |\n\nEnd."
	out := gateway.ConvertTableToBullets(text)
	for _, want := range []string{"**Alice**", "• Score: 150", "**Bob**", "• Score: 120", "**Alice**\n• Score: 150", "• Score: 150\n\n**Bob**"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "• Player: Alice") || !strings.HasPrefix(out, "Scores:") || !strings.HasSuffix(out, "End.") {
		t.Fatalf("out=%q", out)
	}
}

func TestWrapMarkdownTablesNoPipeCharacterShortCircuits(t *testing.T) {
	text := "Plain **bold** text with no table."
	if got := gateway.ConvertTableToBullets(text); got != text {
		t.Fatalf("got=%q", got)
	}
}

func TestWrapMarkdownTablesRowGroupUsesSingleNewlinesWithinGroup(t *testing.T) {
	text := "| Play | Capital | Build | $/day | Risk |\n|---|---|---|---|---|\n" +
		"| A. Copy Hands (HK/SZ) | $5-10k | 2 wk | $30-70 | Low |\n" +
		"| B. NO-sweeper        | $50-100k | 3 wk | $300-1000 | Med |"
	out := gateway.ConvertTableToBullets(text)
	if strings.Contains(out, "\n\n• ") {
		t.Fatalf("bullet after blank line in %q", out)
	}
	var groups []string
	for _, g := range strings.Split(out, "\n\n") {
		if strings.TrimSpace(g) != "" {
			groups = append(groups, g)
		}
	}
	if len(groups) != 2 {
		t.Fatalf("groups=%q", groups)
	}
	for _, g := range groups {
		if n := strings.Count(g, "\n") + 1; n != 5 {
			t.Fatalf("group has %d lines: %q", n, g)
		}
	}
}
