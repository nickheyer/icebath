package tools

import (
	"strings"
	"testing"
)

func TestRedactCDPURLMasksMultipleQueryCredentials(t *testing.T) {
	out := RedactCDPURL("wss://provider.example/session?token=aaa-secret&apikey=bbb-secret")
	if strings.Contains(out, "aaa-secret") || strings.Contains(out, "bbb-secret") {
		t.Fatalf("credentials survived %q", out)
	}
	if out != "wss://provider.example/session?token=***&apikey=***" {
		t.Fatalf("redacted %q", out)
	}
}

func TestRedactCDPURLEmptyReturnsEmpty(t *testing.T) {
	if got := RedactCDPURL(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactCDPURLPassesAndRegexForms(t *testing.T) {
	for in, want := range map[string]string{
		"ws://user:hunter2@127.0.0.1:9222/devtools/browser/abc":       "ws://user:***@127.0.0.1:9222/devtools/browser/abc",
		"https://cdp.example/connect?session=s1&mode=x#frag":          "https://cdp.example/connect?session=***&mode=x#frag",
		"see wss://h.example/p?Key=v&flag next":                       "see wss://h.example/p?Key=***&flag next",
		"ftp://h.example/p?signature=abc\x0bhttp://x.example/?code=9": "ftp://h.example/p?signature=***\x0bhttp://x.example/?code=***",
		"wss://h.example/p?token=a tail":                              "wss://h.example/p?token=*** tail",
		"wss://h.example/p":                                           "wss://h.example/p",
		"no url here":                                                 "no url here",
	} {
		if got := RedactCDPURL(in); got != want {
			t.Errorf("RedactCDPURL(%q) = %q, want %q", in, got, want)
		}
	}
}
