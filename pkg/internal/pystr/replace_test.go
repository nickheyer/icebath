package pystr

import "testing"

func TestDecodeUTF8Replace(t *testing.T) {
	cases := map[string]string{
		"plain":                 "plain",
		"a\xffb":                "a\xEF\xBF\xBDb",
		"\xed\xa0\x80":          "\xEF\xBF\xBD\xEF\xBF\xBD\xEF\xBF\xBD",
		"\xe2\x82":              "\xEF\xBF\xBD",
		"\xf0\x9f\x98\x80 \xc3": "\xF0\x9F\x98\x80 \xEF\xBF\xBD",
	}
	for in, want := range cases {
		if got := DecodeUTF8Replace([]byte(in)); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}
