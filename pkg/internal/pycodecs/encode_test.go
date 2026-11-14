package pycodecs

import "testing"

func TestEncodeMatchesCPythonStrictEncode(t *testing.T) {
	ok := []struct{ text, codec, want string }{
		{"heure d'été", "cp1252", "heure d'\xe9t\xe9"},
		{"é", " Latin-1 ", "\xe9"},
		{"hé", "UTF8", "h\xc3\xa9"},
		{"x", "utf-8-sig", "\xef\xbb\xbfx"},
		{"あ", "shift_jis", "\x82\xa0"},
		{"a", "UTF-16BE", "\x00a"},
		{"a€", "utf-16-le", "a\x00\xac\x20"},
		{"Ю", "koi8_r", "\xe0"},
		{"€x€", "cp1251", "\x88x\x88"},
		{"abc", "us-ascii", "abc"},
		{"\U0001f600", "utf-32-be", "\x00\x01\xf6\x00"},
	}
	for _, c := range ok {
		if got, err := Encode(c.text, c.codec); err != nil || got != c.want {
			t.Errorf("Encode(%q, %q) = %q, %v", c.text, c.codec, got, err)
		}
	}
	bad := []struct{ text, codec, want string }{
		{"€€a", "latin-1", "'latin-1' codec can't encode characters in position 0-1: ordinal not in range(256)"},
		{"a€", "ascii", `'ascii' codec can't encode character '\u20ac' in position 1: ordinal not in range(128)`},
		{"ab€€€", "ascii", "'ascii' codec can't encode characters in position 2-4: ordinal not in range(128)"},
		{"\x80\x81", "utf-8", "'utf-8' codec can't encode characters in position 0-1: surrogates not allowed"},
		{"€€", "euc_jp", `'euc_jp' codec can't encode character '\u20ac' in position 0: illegal multibyte sequence`},
		{"\U0001f600", "latin-1", `'latin-1' codec can't encode character '\U0001f600' in position 0: ordinal not in range(256)`},
		{"a\x80b", "utf-16-le", `'utf-16-le' codec can't encode character '\udc80' in position 1: surrogates not allowed`},
		{"\u0081", "cp1252", `'charmap' codec can't encode character '\x81' in position 0: character maps to <undefined>`},
		{"\u0081\u0081", "cp1252", "'charmap' codec can't encode characters in position 0-1: character maps to <undefined>"},
		{"x", "x-unknown", "unknown encoding: x-unknown"},
	}
	for _, c := range bad {
		if _, err := Encode(c.text, c.codec); err == nil || err.Error() != c.want {
			t.Errorf("Encode(%q, %q) error = %v, want %q", c.text, c.codec, err, c.want)
		}
	}
	if got, err := Encode("a", "utf-16"); err != nil || len(got) != 4 {
		t.Fatalf("utf-16 BOM form %q %v", got, err)
	}
	if back, err := Decode([]byte("\xff\xfea\x00"), "utf-16"); err != nil || back != "a" {
		t.Fatalf("utf-16 decode %q %v", back, err)
	}
}
