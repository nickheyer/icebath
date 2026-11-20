package pystr

import "testing"

func TestDecodeUTF8MatchesStrictCodec(t *testing.T) {
	if s, err := DecodeUTF8([]byte("\xef\xbb\xbfok")); err != nil || s != "\xef\xbb\xbfok" {
		t.Fatalf("bom kept: %q, %v", s, err)
	}
	for in, want := range map[string]string{
		"a\xffb":       "'utf-8' codec can't decode byte 0xff in position 1: invalid start byte",
		"\xed\xa0\x80": "'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte",
		"ab\xe2\x82":   "'utf-8' codec can't decode bytes in position 2-3: unexpected end of data",
	} {
		if _, err := DecodeUTF8([]byte(in)); err == nil || err.Error() != want {
			t.Fatalf("%q: %v, want %s", in, err, want)
		}
	}
}
