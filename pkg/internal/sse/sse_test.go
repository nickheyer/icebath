package sse

import (
	"strings"
	"testing"
)

func collect(t *testing.T, in string) []Event {
	t.Helper()
	var out []Event
	for ev, err := range Read(strings.NewReader(in)) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func TestReadDispatchesOnBlankLine(t *testing.T) {
	in := ": comment\nevent: ping\ndata: {\"a\":1}\n\ndata:first\ndata: second\nid: 7\n\nevent: x\n\ndata: tail"
	got := collect(t, in)
	if len(got) != 2 {
		t.Fatalf("got %d events %+v", len(got), got)
	}
	if got[0].Name != "ping" || got[0].Data != `{"a":1}` {
		t.Fatalf("%+v", got[0])
	}
	if got[1].Name != "" || got[1].Data != "first\nsecond" || got[1].ID != "7" {
		t.Fatalf("%+v", got[1])
	}
}

func TestReadHandlesEveryLineEnding(t *testing.T) {
	for _, in := range []string{"data: a\r\n\r\n", "data: a\n\n", "data: a\r\r"} {
		got := collect(t, in)
		if len(got) != 1 || got[0].Data != "a" {
			t.Fatalf("%q -> %+v", in, got)
		}
	}
}

func TestReadKeepsLongLines(t *testing.T) {
	long := strings.Repeat("x", 200000)
	got := collect(t, "data: "+long+"\n\n")
	if len(got) != 1 || got[0].Data != long {
		t.Fatal("long line truncated")
	}
}

func TestReadStopsWhenConsumerBreaks(t *testing.T) {
	n := 0
	for range Read(strings.NewReader("data: 1\n\ndata: 2\n\n")) {
		n++
		break
	}
	if n != 1 {
		t.Fatal(n)
	}
}
