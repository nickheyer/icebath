package pycodecs

import "testing"

func TestDecodeMatchesCPythonStrictDecode(t *testing.T) {
	ok := []struct{ data, codec, want string }{
		{"heure d'\xe9t\xe9", "cp1252", "heure d'été"},
		{"heure d'\xe9t\xe9", "Windows-1252", "heure d'été"},
		{"\xe9", " Latin-1 ", "é"},
		{"h\xc3\xa9", "UTF8", "hé"},
		{"\xef\xbb\xbfx", "utf-8-sig", "x"},
		{"\x82\xa0", "shift_jis", "あ"},
		{"\xff\xfea\x00", "utf-16", "a"},
		{"\x00a", "UTF-16BE", "a"},
		{"\xe0", "koi8_r", "Ю"},
		{"abc", "us-ascii", "abc"},
	}
	for _, c := range ok {
		if got, err := Decode([]byte(c.data), c.codec); err != nil || got != c.want {
			t.Errorf("Decode(%q, %q) = %q, %v", c.data, c.codec, got, err)
		}
	}
	bad := []struct{ data, codec, want string }{
		{"\x81", "cp1252", "'charmap' codec can't decode byte 0x81 in position 0: character maps to <undefined>"},
		{"a\xe9", "utf-8", "'utf-8' codec can't decode byte 0xe9 in position 1: unexpected end of data"},
		{"\xe9ab", "utf-8", "'utf-8' codec can't decode byte 0xe9 in position 0: invalid continuation byte"},
		{"\xff", "utf-8", "'utf-8' codec can't decode byte 0xff in position 0: invalid start byte"},
		{"\x80", "ascii", "'ascii' codec can't decode byte 0x80 in position 0: ordinal not in range(128)"},
		{"x", "x-unknown", "unknown encoding: x-unknown"},
	}
	for _, c := range bad {
		if _, err := Decode([]byte(c.data), c.codec); err == nil || err.Error() != c.want {
			t.Errorf("Decode(%q, %q) error = %v, want %q", c.data, c.codec, err, c.want)
		}
	}
	if mod, ok := Lookup("ISO-8859-15"); !ok || mod != "iso8859_15" {
		t.Fatalf("Lookup = %q, %v", mod, ok)
	}
}

func TestLocaleEncodingCoercesTheCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	if got := LocaleEncoding(); got != "utf-8" && got[:2] != "cp" {
		t.Fatalf("LocaleEncoding = %q", got)
	}
}
