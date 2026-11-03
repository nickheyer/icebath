package termlab

import (
	"bytes"
	"image"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/term"
)

func TestMouseSeq(t *testing.T) {
	if got := string(MouseSeq(12, 7, 0, true)); got != "\x1b[<0;12;7M" {
		t.Errorf("press %q", got)
	}
	if got := string(MouseSeq(1, 1, 2, false)); got != "\x1b[<2;1;1m" {
		t.Errorf("release %q", got)
	}
}

func TestChunkCodec(t *testing.T) {
	in := []Chunk{{At: time.Unix(0, 1700000000123456789), Data: []byte("\x1b_Ga=T;AAAA\x1b\\")}, {At: time.Unix(0, 5), Data: []byte{0, 255, '\n'}}}
	f := bytes.Fields([]byte(encodeChunks(in)))
	var fields []string
	for _, b := range f {
		fields = append(fields, string(b))
	}
	out, err := decodeChunks(fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || !out[0].At.Equal(in[0].At) || !bytes.Equal(out[0].Data, in[0].Data) || !bytes.Equal(out[1].Data, in[1].Data) {
		t.Errorf("round trip %v", out)
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", DriverEnv + "=/s", "B=2"}, []string{"B=3", "C=4"})
	if want := []string{"A=1", "B=3", "C=4"}; !slices.Equal(got, want) {
		t.Errorf("merged %v, want %v", got, want)
	}
}

func TestCopySize(t *testing.T) {
	a, as, err := openPty()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer as.Close()
	b, bs, err := openPty()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	defer bs.Close()
	want := &unix.Winsize{Col: 123, Row: 45, Xpixel: 861, Ypixel: 765}
	if err := unix.IoctlSetWinsize(int(a.Fd()), unix.TIOCSWINSZ, want); err != nil {
		t.Fatal(err)
	}
	if _, err := copySize(as, b); err != nil {
		t.Fatal(err)
	}
	got, err := unix.IoctlGetWinsize(int(bs.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Errorf("copied %+v, want %+v", *got, *want)
	}
}

func TestLimited(t *testing.T) {
	const limit = 1 << 34
	cmd, err := Limited(limit, "sleep", "5")
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		comm, _ := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/comm")
		var r unix.Rlimit
		if err := unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_AS, nil, &r); err != nil {
			t.Fatal(err)
		}
		if string(bytes.TrimSpace(comm)) == "sleep" {
			if r.Cur != limit || r.Max != limit {
				t.Errorf("sleep runs with an address space limit of %d and %d, want %d", r.Cur, r.Max, limit)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the limited helper never became sleep, comm %q", comm)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatch(t *testing.T) {
	s := &Session{cols: 10, rows: 3, mirror: term.New(10, 3, term.Options{})}
	feed := func(at time.Time, text string) {
		s.outMu.Lock()
		defer s.outMu.Unlock()
		s.mirror.Write([]byte(text))
		for _, w := range s.watches {
			w.track(at)
		}
	}
	feed(time.Unix(0, 0), "\x1b[Habc")
	w := s.Watch(image.Rect(0, 0, 4, 1))
	t1, t2, t3 := time.Unix(10, 0), time.Unix(20, 0), time.Unix(30, 0)
	feed(t1, "\x1b[1;1Hx")
	feed(t2, "\x1b[1;2Hyz")
	feed(t3, "\x1b[2;1Hrow two")
	if got := w.Moved(time.Unix(0, 0), t1); len(got) != 1 || !got[0] {
		t.Errorf("moved by t1 %v", got)
	}
	if got := w.Diff(time.Unix(5, 0), t3); !slices.Equal(got, []int{0, 1, 2}) {
		t.Errorf("diff %v", got)
	}
	if at, ok := w.Answer(t1, []int{1, 2}, 1); !ok || !at.Equal(t2) {
		t.Errorf("answer %v %v", at, ok)
	}
	if _, ok := w.Answer(t2, []int{0, 1, 2}, 0.5); ok {
		t.Error("answered after t2 though the box never changed again")
	}
	if got := w.At(t1); got[0].Content != "x" || got[1].Content != "b" {
		t.Errorf("state at t1 %q %q", got[0].Content, got[1].Content)
	}
	if p := w.Cell(3); p != image.Pt(3, 0) {
		t.Errorf("cell 3 at %v", p)
	}
	w.Stop()
	if len(s.watches) != 0 {
		t.Error("stopped watch still tracks")
	}
}

func TestGraphics(t *testing.T) {
	chunks := []Chunk{
		{At: time.Unix(1, 0), Data: []byte("\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24,q=1;AAAA\x1b\\\x1b_Ga=T,i=2,f=24,o=z,q=2,m=1;AAAA")},
		{At: time.Unix(2, 0), Data: []byte("\x1b\\\x1b_Gm=0;AAAA\x1b\\\x1b_Ga=T,i=3,t=s,U=1;bmFtZQ==\x1b\\\x1b_Ga=d,d=I,i=2,q=2\x1b\\")},
	}
	s := &Session{chunks: chunks}
	for _, c := range chunks {
		s.out = append(s.out, c.Data...)
	}
	g := graphics(s)
	if len(g) != 5 {
		t.Fatalf("found %d commands", len(g))
	}
	var sends []bool
	for _, c := range g {
		sends = append(sends, c.transmits())
	}
	if !slices.Equal(sends, []bool{false, true, false, true, false}) {
		t.Errorf("transmissions %v", sends)
	}
	if g[1].keys["o"] != "z" || !g[1].at.Equal(time.Unix(1, 0)) || g[3].keys["t"] != "s" || g[3].keys["U"] != "1" || !g[3].at.Equal(time.Unix(2, 0)) {
		t.Errorf("keys or times wrong %+v %+v", g[1], g[3])
	}
	if !sixelStart.Match([]byte("\x1bP0;1;q\"1;1;240;360#0")) || sixelStart.Match([]byte("\x1bP+q544e\x1b\\")) {
		t.Error("sixel start pattern misjudged")
	}
	answers := []Chunk{{At: time.Unix(1, 0), Data: []byte("\x1b_Gi=31;OK\x1b\\\x1b_Gi=2;ENOTSUP")}, {At: time.Unix(2, 0), Data: []byte("PORTED:\x1b\\")}}
	if at, ok := arrival(answers, "ENOTSUPPORTED"); !ok || !at.Equal(time.Unix(2, 0)) {
		t.Errorf("refusal arrived %v %v", at, ok)
	}
	if _, ok := arrival(answers, "EBADF"); ok {
		t.Error("found an answer that never came")
	}
}

// Grid of plain cells from rows of text, one rune per cell
func gridOf(rows ...string) [][]uv.Cell {
	out := make([][]uv.Cell, len(rows))
	for y, r := range rows {
		for _, c := range r {
			out[y] = append(out[y], uv.Cell{Content: string(c), Width: 1})
		}
	}
	return out
}

// Text grid of plain cells
func textOf(g [][]uv.Cell) [][]string {
	out := make([][]string, len(g))
	for y, row := range g {
		for _, c := range row {
			out[y] = append(out[y], c.Content)
		}
	}
	return out
}

func TestFindCardAndWorld(t *testing.T) {
	g := gridOf(
		"┌─ ARENA ───────┐",
		"│               │",
		"│   Dock Log    │",
		"│   ▀▄▀▄▀▄▀▄▀   │",
		"│   ▄▀ Dir ▀▄   │",
		"│   ▀▄▀▄▀▄▀▄▀   │",
		"│               │",
		"└───────────────┘",
	)
	frame, ok := findCard(textOf(g), "ARENA")
	if !ok || frame != image.Rect(0, 0, 17, 8) {
		t.Fatalf("frame %v %v", frame, ok)
	}
	if c := content(frame); c != image.Rect(3, 2, 14, 6) {
		t.Errorf("content %v", c)
	}
	box := worldBox(frame)
	if box != image.Rect(3, 3, 14, 6) {
		t.Errorf("world box %v", box)
	}
	mask := worldMask(g, box, false)
	for _, p := range mask {
		if p.Y == 4 && p.X >= 6 && p.X <= 10 {
			t.Errorf("cell %v beside the label was judged", p)
		}
	}
	if len(mask) != box.Dx()*box.Dy()-5 {
		t.Errorf("judged %d cells", len(mask))
	}
	blank := gridOf("               ", "   x           ", "               ")
	blank[2][4].Style = uv.Style{Attrs: 1}
	if got := worldMask(blank, image.Rect(0, 0, 15, 3), true); len(got) != 45-3-1 {
		t.Errorf("image mask judged %d cells", len(got))
	}
}
