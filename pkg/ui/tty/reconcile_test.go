package tty

import (
	"io"
	"testing"
	"time"
)

// Renderer over a one row frame whose terminal widths come from a table
func reconcileWith(t *testing.T, content string, w int, widths map[probeKey]int, sized bool) []want {
	t.Helper()
	r := NewRenderer(io.Discard, w, 1)
	r.parse.parse(&r.frame, content, w, 1)
	r.widths.sized = sized
	for k, n := range widths {
		r.widths.learn(k.text, k.enc, n)
	}
	if need := r.target(time.Unix(1, 0)); len(need) > 0 {
		t.Fatalf("widths still needed %v", need)
	}
	return r.tgt
}

// Text and width of each target column
func columns(row []want) []string {
	var out []string
	for _, c := range row {
		out = append(out, c.Text+":"+string(rune('0'+c.Width)))
	}
	return out
}

func TestNarrowerClusterIsPadded(t *testing.T) {
	row := reconcileWith(t, "a❤️b", 5, map[probeKey]int{{"❤️", encText}: 1}, false)
	got := columns(row)
	want := []string{"a:1", "❤️:1", " :1", "b:1", " :1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("columns %v, want %v", got, want)
		}
	}
}

func TestWiderClusterTakesFollowingBlanks(t *testing.T) {
	row := reconcileWith(t, "a☺ b", 4, map[probeKey]int{{"☺", encText}: 2}, false)
	if row[1].Text != "☺" || row[1].Width != 2 || row[2].Width != 0 || row[3].Text != "b" {
		t.Fatalf("columns %v", columns(row))
	}
}

func TestWiderBoxCharacterUsesDECGraphics(t *testing.T) {
	row := reconcileWith(t, "──", 2, map[probeKey]int{{"─", encText}: 2, {"─", encDEC}: 1}, false)
	if row[0].enc != encDEC || row[1].enc != encDEC || row[0].Width != 1 {
		t.Fatalf("box drawing kept as %+v %+v", row[0], row[1])
	}
}

func TestWiderSymbolFallsBackToItsLookalike(t *testing.T) {
	row := reconcileWith(t, "→x", 2, map[probeKey]int{{"→", encText}: 2}, false)
	if row[0].Text != ">" || row[1].Text != "x" {
		t.Fatalf("columns %v", columns(row))
	}
}

func TestSizedTextKeepsTheFrameWidth(t *testing.T) {
	row := reconcileWith(t, "❤️x", 3, map[probeKey]int{{"❤️", encText}: 1}, true)
	if row[0].enc != encSized || row[0].Width != 2 || row[2].Text != "x" {
		t.Fatalf("columns %v enc %d", columns(row), row[0].enc)
	}
}

func TestUnmeasuredRowWaitsForItsProbe(t *testing.T) {
	r := NewRenderer(io.Discard, 6, 2)
	r.parse.parse(&r.frame, "ab\n中x", 6, 2)
	need := r.target(time.Unix(1, 0))
	if len(need) != 1 || need[0].text != "中" {
		t.Fatalf("probes asked %v", need)
	}
	if r.tgt[0].Text != "a" || r.tgt[6].Text != " " {
		t.Fatalf("rows %v", columns(r.tgt))
	}
	r.widths.pending[need[0]] = time.Unix(1, 0)
	r.target(time.Unix(1, 0).Add(probeFloor))
	if !r.tgt[6].blind {
		t.Fatalf("row was not painted provisionally after the deadline")
	}
}

func TestClassesInferAfterAQuorum(t *testing.T) {
	w := newWidths()
	for _, s := range []string{"一", "丁", "七"} {
		if _, ok := w.of("万", encText); ok {
			t.Fatalf("inferred before the quorum")
		}
		w.learn(s, encText, 2)
	}
	if n, ok := w.of("万", encText); !ok || n != 2 {
		t.Fatalf("class not inferred, %d %v", n, ok)
	}
	for _, s := range []string{"😀", "😁", "😂"} {
		w.learn(s, encText, 2)
	}
	if _, ok := w.of("😃", encText); ok {
		t.Fatalf("pictographs must never be inferred")
	}
	w.learn("丈", encText, 1)
	if _, ok := w.of("三", encText); ok {
		t.Fatalf("a contested class was inferred")
	}
}

func TestFragileClusters(t *testing.T) {
	for s, want := range map[string]bool{"❤️": true, "👨‍👩‍👧": true, "é": false, "क्ष": false, "ने": true, "a​": true, "é": false} {
		if fragile(s) != want {
			t.Errorf("fragile(%q) = %v, want %v", s, !want, want)
		}
	}
}
