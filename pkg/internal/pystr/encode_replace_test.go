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
