package slack

import (
	"strings"
	"testing"
)

func TestFormatMessageBoldItalicMixedWithBold(t *testing.T) {
	result := formatSlackMrkdwn("***important*** and **bold**")
	if !strings.Contains(result, "*_important_*") || !strings.Contains(result, "*bold*") {
		t.Fatalf("got %q", result)
	}
}

func TestFormatMessageCodeBlockPreserved(t *testing.T) {
	if got := formatSlackMrkdwn("```python\nx = **not bold**\n```"); got != "```\nx = **not bold**\n```" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageEscapedEntityTextNotDoubleDecoded(t *testing.T) {
	for _, s := range []string{"&amp;lt;", "&amp;gt;"} {
		if got := formatSlackMrkdwn(s); got != s {
			t.Fatalf("got %q want %q", got, s)
		}
	}
}

func TestFormatMessageEscapesControlCharacters(t *testing.T) {
	if got := formatSlackMrkdwn("AT&T < 5 > 3"); got != "AT&amp;T &lt; 5 &gt; 3" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageEscapesSpecialBroadcastMentions(t *testing.T) {
	result := formatSlackMrkdwn("Broadcast <!everyone> <!channel> <!here|here>")
	if result != "Broadcast &lt;!everyone&gt; &lt;!channel&gt; &lt;!here|here&gt;" {
		t.Fatalf("got %q", result)
	}
	for _, bad := range []string{"<!everyone>", "<!channel>", "<!here"} {
		if strings.Contains(result, bad) {
			t.Fatalf("%q still in %q", bad, result)
		}
	}
}

func TestFormatMessageHeaderWithBoldContent(t *testing.T) {
	if got := formatSlackMrkdwn("## **Title**"); got != "*Title*" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageInlineTripleBacktickUnchanged(t *testing.T) {
	if got := formatSlackMrkdwn("```hello```"); got != "```hello```" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageItalicAsteriskConversion(t *testing.T) {
	if got := formatSlackMrkdwn("*hello*"); got != "_hello_" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageLinkWithParenthesesInURL(t *testing.T) {
	if got := formatSlackMrkdwn("[Foo](https://en.wikipedia.org/wiki/Foo_(bar))"); got != "<https://en.wikipedia.org/wiki/Foo_(bar)|Foo>" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageMidLineTripleBackticksContentPreserved(t *testing.T) {
	text := "Use ```pip install foo``` then:\n```bash\ncode\n```"
	if got := formatSlackMrkdwn(text); got != text {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageMixedRawAndEscapedEntities(t *testing.T) {
	if got := formatSlackMrkdwn("AT&T and &amp; entity"); got != "AT&amp;T and &amp; entity" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageMultilineMixedFormatting(t *testing.T) {
	result := formatSlackMrkdwn("## Title\n**bold** and [link](https://x.com)\n> quote\n`code`")
	if !strings.HasPrefix(result, "*Title*") {
		t.Fatalf("got %q", result)
	}
	for _, want := range []string{"*bold*", "<https://x.com|link>", "> quote", "`code`"} {
		if !strings.Contains(result, want) {
			t.Fatalf("%q missing from %q", want, result)
		}
	}
}

func TestFormatMessagePreEscapedGtNotDoubleEscaped(t *testing.T) {
	if got := formatSlackMrkdwn("5 &gt; 3"); got != "5 &gt; 3" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessagePreEscapedLtNotDoubleEscaped(t *testing.T) {
	if got := formatSlackMrkdwn("&lt;"); got != "&lt;" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMessageTableIntegrationTableFenceCarriesNoLanguageTag(t *testing.T) {
	out := formatSlackMrkdwn("| a | b |\n|---|---|\n| 1 | 2 |")
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "```") {
			if ln != "```" {
				t.Fatalf("first fence %q", ln)
			}
			return
		}
	}
	t.Fatalf("no fence in %q", out)
}
