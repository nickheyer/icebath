package pyjson

import "testing"

func TestSafeLoads(t *testing.T) {
	obj, ok := SafeLoads(`{"b": 1, "a": [2]}`, nil).(*Object)
	if !ok || len(obj.Keys) != 2 || obj.Keys[0] != "b" {
		t.Fatalf("object %#v", obj)
	}
	if got := SafeLoads([]byte(`[1, 2]`), nil).([]any); len(got) != 2 {
		t.Fatalf("bytes %#v", got)
	}
	for _, bad := range []any{"{", "", nil, int64(5), 1.5, []byte{0xff, 0xfe, 0x00}, "\xef\xbb\xbf{}"} {
		if got := SafeLoads(bad, "dflt"); got != "dflt" {
			t.Fatalf("SafeLoads(%#v) = %#v", bad, got)
		}
	}
	if got := SafeLoads("null", "dflt"); got != nil {
		t.Fatalf("null %#v", got)
	}
}
