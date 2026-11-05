package provider

import "testing"

func TestSanitizeSurrogatesReplacesEachLoneSurrogate(t *testing.T) {
	lone := "\xed\xa0\x80"
	if got := sanitizeSurrogates("a" + lone + lone + "b"); got != "a��b" {
		t.Fatalf("%q", got)
	}
	if got := sanitizeSurrogates("plain é text"); got != "plain é text" {
		t.Fatalf("%q", got)
	}
	if got := sanitizeSurrogates("x\x80\x80y"); got != "x��y" {
		t.Fatalf("%q", got)
	}
}
