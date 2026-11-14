package pystr

import (
	"reflect"
	"testing"
)

func TestTextwrapWrapNoHyphenBreak(t *testing.T) {
	cases := []struct {
		text  string
		width int
		brk   bool
		want  []string
	}{
		{"The quick brown fox jumps over the lazy dog", 10, true, []string{"The quick", "brown fox", "jumps over", "the lazy", "dog"}},
		{"abcdefghijklmnop", 5, true, []string{"abcde", "fghij", "klmno", "p"}},
		{"abcdefghijklmnop", 5, false, []string{"abcdefghijklmnop"}},
		{"well-known hyphen-words here", 12, true, []string{"well-known", "hyphen-words", "here"}},
		{"  lead\tand trail  ", 8, true, []string{"  lead", "and", "trail"}},
		{"", 10, true, nil},
		{"   ", 10, true, nil},
	}
	for _, c := range cases {
		if got := TextwrapWrapNoHyphenBreak(c.text, c.width, c.brk); !reflect.DeepEqual(got, c.want) {
			t.Errorf("wrap(%q, %d) = %q want %q", c.text, c.width, got, c.want)
		}
	}
}

func TestTextwrapWrapMatchesPythonTextWrapper(t *testing.T) {
	indent := DefaultTextwrap(10)
	indent.SubsequentIndent = "  "
	keep := DefaultTextwrap(30)
	keep.ReplaceWhitespace, keep.DropWhitespace = false, false
	noTabs := DefaultTextwrap(20)
	noTabs.ExpandTabs = false
	noTabs.ReplaceWhitespace = false
	cases := []struct {
		text string
		o    TextwrapOptions
		want []string
	}{
		{"The quick brown fox jumps over the lazy dog", DefaultTextwrap(10), []string{"The quick", "brown fox", "jumps over", "the lazy", "dog"}},
		{"The quick brown fox jumps over the lazy dog", indent, []string{"The quick", "  brown", "  fox", "  jumps", "  over the", "  lazy dog"}},
		{"well-known long-standing issue", DefaultTextwrap(12), []string{"well-known", "long-", "standing", "issue"}},
		{"supercalifragilistic-expialidocious", DefaultTextwrap(15), []string{"supercalifragil", "istic-", "expialidocious"}},
		{"ab-cdefghijklmnop", DefaultTextwrap(8), []string{"ab-cdefg", "hijklmno", "p"}},
		{"1234-5678-9012", DefaultTextwrap(9), []string{"1234-", "5678-9012"}},
		{"foo--bar", DefaultTextwrap(5), []string{"foo--", "bar"}},
		{"  lead and trail  ", keep, []string{"  lead and trail  "}},
		{"a\tb  c", noTabs, []string{"a\tb  c"}},
		{"a\tb", DefaultTextwrap(20), []string{"a       b"}},
		{"", DefaultTextwrap(10), nil},
	}
	for _, tc := range cases {
		if got := TextwrapWrap(tc.text, tc.o); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("wrap(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
	if ExpandTabs("x\ty", 4) != "x   y" {
		t.Fatal(ExpandTabs("x\ty", 4))
	}
}
