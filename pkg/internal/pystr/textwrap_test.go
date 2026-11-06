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
