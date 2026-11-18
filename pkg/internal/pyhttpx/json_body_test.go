package pyhttpx

import "testing"

func TestJSONBodyWritesTheBytesHttpxSends(t *testing.T) {
	v := map[string]any{"a": "<b>&\u2028x\u2029", "b": "\\u2028 literal", "c": "café \U0001f680", "d": "\b\f\n\t\x01\x7f"}
	got, err := JSONBody(v)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"a\":\"<b>&\xe2\x80\xa8x\xe2\x80\xa9\",\"b\":\"\\\\u2028 literal\",\"c\":\"caf\xc3\xa9 \xf0\x9f\x9a\x80\",\"d\":\"\\b\\f\\n\\t\\u0001\x7f\"}"
	if string(got) != want {
		t.Fatalf("body\n%q\nwant\n%q", got, want)
	}
}

func TestRawLineSeparatorsLeavesEscapedBackslashes(t *testing.T) {
	if got := string(rawLineSeparators([]byte(`{"b":"\\u2028 x"}`))); got != `{"b":"\\u2028 x"}` {
		t.Fatalf("got %q", got)
	}
}
