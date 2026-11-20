package tools

import (
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestJSONEncodeWTF8SlowPathKeepsSurrogatesInsideStructs(t *testing.T) {
	r := &ReadResult{Content: "1|a\xed\xb2\x80b", TotalLines: 1, Error: "bad \xff"}
	got, err := pyjson.EncodeWTF8(r)
	want := "{\"content\":\"1|a\xed\xb2\x80b\",\"total_lines\":1,\"file_size\":0,\"truncated\":false,\"is_binary\":false,\"is_image\":false,\"error\":\"bad \xed\xb3\xbf\",\"not_found\":false}"
	if err != nil || string(got) != want {
		t.Fatalf("%q %v", got, err)
	}
	d := toDict(r)
	if c, _ := d.Get("content"); c != "1|a\xed\xb2\x80b" || JSON(d) != string(pyjson.SpaceSeparators([]byte(want))) {
		t.Fatalf("toDict %#v %s", c, JSON(d))
	}
	if got := ErrorResult(errors.New("x\xed\xa0\x80<")); got != "{\"error\": \"x\xed\xa0\x80<\"}" {
		t.Fatalf("error result %q", got)
	}
}

func TestParseToolArgumentsKeepsSurrogates(t *testing.T) {
	m, errText := ParseToolArguments(`{"old_string": "a\udc80b", "n": 2}`)
	if errText != "" || m["old_string"] != "a\xed\xb2\x80b" || m["n"] != float64(2) {
		t.Fatalf("%#v %s", m, errText)
	}
	if _, errText := ParseToolArguments("{\"a\": \"\x80\"}"); errText == "" {
		t.Fatal("stray byte accepted")
	}
}

func TestPyReprStrSurrogates(t *testing.T) {
	cases := map[string]string{"\xed\xa0\x80": `'\ud800'`, "\xed\xb2\x80": `'\udc80'`, "\xff": `'\udcff'`, "a\xed\xbf\xbfb": `'a\udfffb'`}
	for in, want := range cases {
		if got := pyReprStr(in); got != want {
			t.Errorf("%q repr %s, want %s", in, got, want)
		}
	}
}

func TestPyLowerAndPrefixKeepSurrogates(t *testing.T) {
	if got := pyLower("AB\xed\xa0\x80\xc3\x89\x80"); got != "ab\xed\xa0\x80\xc3\xa9\x80" {
		t.Fatalf("lower %q", got)
	}
	if got := runePrefix("a\xed\xa0\x80bc", 2); got != "a\xed\xa0\x80" {
		t.Fatalf("prefix %q", got)
	}
	if got := pyLen("a\xed\xa0\x80b"); got != 3 {
		t.Fatalf("len %d", got)
	}
}
