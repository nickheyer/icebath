package pyre

import "testing"

func TestMustCompileMatchesLikeCompileAndPanicsOnABadPattern(t *testing.T) {
	re := MustCompile(`(?:^|\s)--port(?:=|\s+)(\d+)`)
	m, err := re.FindStringMatch("serve --port 9120")
	if err != nil || m == nil || m.GroupByNumber(1).String() != "9120" {
		t.Fatalf("match %v %v", m, err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("bad pattern compiled")
		}
	}()
	MustCompile("(unclosed")
}
