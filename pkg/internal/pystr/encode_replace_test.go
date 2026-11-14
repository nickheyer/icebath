package pystr

import "testing"

func TestEncodeUTF8ReplaceSwapsSurrogatesOnly(t *testing.T) {
	lone := string(AppendRune(nil, 0xD800))
	cases := map[string]string{"plain": "plain", "café \U0001F600": "café \U0001F600", "a" + lone + "b": "a?b", "": ""}
	for in, want := range cases {
		if got := string(EncodeUTF8Replace(in)); got != want {
			t.Errorf("EncodeUTF8Replace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeUTF8BackslashReplaceEscapesLoneSurrogates(t *testing.T) {
	in := "a" + FromRunes([]rune{0xD83D}) + "é" + FromRunes([]rune{0xDC00})
	if got := EncodeUTF8BackslashReplace(in); got != "a\\ud83dé\\udc00" {
		t.Fatalf("got %q", got)
	}
	if got := EncodeUTF8BackslashReplace("plain 中"); got != "plain 中" {
		t.Fatalf("got %q", got)
	}
}
