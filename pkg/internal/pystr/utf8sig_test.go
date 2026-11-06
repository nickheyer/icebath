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
