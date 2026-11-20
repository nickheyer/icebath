package pystr

import "testing"

func TestDecodeUTF8SigDropsOneBOMAndRefusesSurrogates(t *testing.T) {
	text, err := DecodeUTF8Sig([]byte("\xef\xbb\xbf\xef\xbb\xbfx"))
	if err != nil || text != "\ufeffx" {
		t.Fatalf("got %q %v", text, err)
	}
	if _, err := DecodeUTF8Sig([]byte("a\xed\xa0\x80")); err == nil {
		t.Fatal("an encoded surrogate decoded under strict utf-8")
	}
	if _, err := DecodeUTF8Sig([]byte("a\xff")); err == nil || err.Error() != "'utf-8' codec can't decode byte 0xff in position 1: invalid start byte" {
		t.Fatalf("got %v", err)
	}
}

func TestDecodeUTF8SigFileReadsALoneBOMStartAsEmpty(t *testing.T) {
	for _, raw := range []string{"", "\xef", "\xef\xbb", "\xef\xbb\xbf"} {
		if text, err := DecodeUTF8SigFile([]byte(raw)); err != nil || text != "" {
			t.Fatalf("%q got %q %v", raw, text, err)
		}
	}
	if text, err := DecodeUTF8SigFile([]byte("\xef\xbb\xbfhome = x")); err != nil || text != "home = x" {
		t.Fatalf("got %q %v", text, err)
	}
	if _, err := DecodeUTF8SigFile([]byte("\xef\xbb\xbfhome = x\xff")); err == nil || err.Error() != "'utf-8' codec can't decode byte 0xff in position 8: invalid start byte" {
		t.Fatalf("got %v", err)
	}
	if _, err := DecodeUTF8SigFile([]byte("\xef\xbb\xbf\xe4\xbd")); err == nil || err.Error() != "'utf-8' codec can't decode bytes in position 0-1: unexpected end of data" {
		t.Fatalf("got %v", err)
	}
}
