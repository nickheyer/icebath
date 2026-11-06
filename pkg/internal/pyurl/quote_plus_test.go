package pyurl

import "testing"

func TestQuotePlusAndURLEncodeMatchUrllib(t *testing.T) {
	if got := QuotePlus("a b/c&d", ""); got != "a+b%2Fc%26d" {
		t.Fatalf("quote_plus %q", got)
	}
	if got := QuotePlus("a/b", "/"); got != "a/b" {
		t.Fatalf("quote_plus safe %q", got)
	}
	if got := URLEncode([][2]string{{"time", "07:15"}, {"what", "drink water é"}}); got != "time=07%3A15&what=drink+water+%C3%A9" {
		t.Fatalf("urlencode %q", got)
	}
}
