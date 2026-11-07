package state

import "testing"

func TestDumpsUnicodeKeepsOrderAndNonASCII(t *testing.T) {
	got, err := DumpsUnicode([]any{pyObject{{"emoji", "👍"}, {"author", "agent"}}})
	if err != nil || got != `[{"emoji": "👍", "author": "agent"}]` {
		t.Fatalf("got %s err %v", got, err)
	}
}
