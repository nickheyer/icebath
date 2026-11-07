package telegram

import (
	"strings"
	"testing"
)

func TestEscapesAllSpecialCharacters(t *testing.T) {
	escaped := escapeMdv2(`_*[]()~` + "`" + `>#+-=|{}.!\ `)
	for _, ch := range `_*[]()~` + "`" + `>#+-=|{}.!\` {
		if !strings.Contains(escaped, `\`+string(ch)) {
			t.Errorf("%q not escaped in %q", ch, escaped)
		}
	}
	if got := escapeMdv2("plain text"); got != "plain text" {
		t.Errorf("plain text changed to %q", got)
	}
}

func TestRemovesEscapeBackslashes(t *testing.T) {
	if got := stripMdv2(`hello\.world\!`); got != "hello.world!" {
		t.Fatalf("got %q", got)
	}
}

func TestRemovesBothBoldAndItalic(t *testing.T) {
	if got := stripMdv2("*bold* and _italic_"); got != "bold and italic" {
		t.Fatalf("got %q", got)
	}
}

func TestPreservesSnakeCase(t *testing.T) {
	if got := stripMdv2("my_variable_name"); got != "my_variable_name" {
		t.Fatalf("got %q", got)
	}
}

func TestStripMdv2DoubleBoldStrikeAndSpoiler(t *testing.T) {
	if got := stripMdv2("**big** ~gone~ ||secret|| é_ital_"); got != "big gone secret é_ital_" {
		t.Fatalf("got %q", got)
	}
}

func TestBoldLabelHTML(t *testing.T) {
	for in, want := range map[string]string{
		"Model: gpt <x>": "<b>Model:</b> gpt &lt;x&gt;",
		"No colon & co":  "<b>No colon &amp; co</b>",
		"a:b:c":          "<b>a:</b>b:c",
	} {
		if got := boldLabelHTML(in); got != want {
			t.Errorf("boldLabelHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSeparateChunkIndicatorFromFence(t *testing.T) {
	in := "```python\nx = 1\n``` \\(1/2\\)\nmore\n``` (3/4)"
	want := "```python\nx = 1\n```\n\\(1/2\\)\nmore\n```\n(3/4)"
	if got := separateChunkIndicatorFromFence(in); got != want {
		t.Fatalf("got %q", got)
	}
	if got := separateChunkIndicatorFromFence("``` (1/2) tail"); got != "``` (1/2) tail" {
		t.Fatalf("mid line marker moved %q", got)
	}
}

func TestRichNormalizeLinebreaksKeepsProtectedRegions(t *testing.T) {
	in := "line one\nline two\n\npara\n```go\na\nb\n```\nafter\n| h1 | h2 |\n|---|---|\n| é | 2 |\ntail\nend"
	want := "line one  \nline two\n\npara  \n```go\na\nb\n```  \nafter  \n| h1 | h2 |\n|---|---|\n| é | 2 |  \ntail  \nend"
	if got := richNormalizeLinebreaks(in); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	for _, in := range []string{"", "single line"} {
		if got := richNormalizeLinebreaks(in); got != in {
			t.Fatalf("%q changed to %q", in, got)
		}
	}
}
