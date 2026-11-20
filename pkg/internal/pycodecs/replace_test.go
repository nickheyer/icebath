package pycodecs

import "testing"

func TestDecodeReplaceMatchesCPython(t *testing.T) {
	cases := []struct {
		data, codec, want string
	}{
		{"a\xffb\xe2\x82", "utf-8", "a�b�"},
		{"ok\x80\x81z", "ascii", "ok��z"},
		{"\x80\x81\x82", "cp1252", "€�‚"},
		{"\xe9t\xe9", "latin-1", "été"},
	}
	for _, c := range cases {
		got, err := DecodeReplace([]byte(c.data), c.codec)
		if err != nil || got != c.want {
			t.Errorf("%q as %s gave %q %v, want %q", c.data, c.codec, got, err, c.want)
		}
	}
}
