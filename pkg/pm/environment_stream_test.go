package pm

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var streamPieces = []string{"x", "X", "\n", "\r", "\r\n", " ", "No ", "SOLUTION", "solu", "tion found", "Conflicting ", "requirements", "conflicting urls",
	"Because only the following versions", "the build bac", "Kend returned an error", "the build backend returned an error", "failed to bytecode-compile",
	"İ", "é", "\U0001F600", "and your pyproject depends on", "\x1b[31m", "\t"}

// Random byte stream built from marker fragments, mixed case, newlines and multibyte or invalid sequences
func streamSample(r *rand.Rand) []byte {
	var b []byte
	for range r.IntN(400) {
		if r.IntN(12) == 0 {
			b = append(b, []byte{0xe2, 0x82, 0xff, 0xed, 0xa0, 0x80, 0xf0, 0x9f}[r.IntN(8)])
			continue
		}
		b = append(b, streamPieces[r.IntN(len(streamPieces))]...)
		if r.IntN(30) == 0 {
			b = append(b, strings.Repeat("y", r.IntN(5000))...)
		}
	}
	return b
}

// Splits data into random chunks the way pipe reads return them
func streamChunks(r *rand.Rand, data []byte) [][]byte {
	var chunks [][]byte
	for len(data) > 0 {
		n := 1 + r.IntN(min(len(data), 1+r.IntN(300)))
		chunks = append(chunks, data[:n])
		data = data[n:]
	}
	return chunks
}

// Python IncrementalNewlineDecoder over a utf-8 replace decoder, copying every chunk
type refNewlineDecoder struct {
	pending   []byte
	pendingCR bool
}

func (d *refNewlineDecoder) decode(data []byte, final bool) string {
	text, rest := decodeUTF8Replace(append(append([]byte(nil), d.pending...), data...), final)
	d.pending = rest
	if d.pendingCR && (text != "" || final) {
		text = "\r" + text
		d.pendingCR = false
	}
	if strings.HasSuffix(text, "\r") && !final {
		text = text[:len(text)-1]
		d.pendingCR = true
	}
	return universalNewlines(text)
}

func refLastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func TestEnvironmentStreamingMatchesWholeWindowScan(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for round := range 300 {
		data := streamSample(r)
		chunks := streamChunks(r, data)
		var ref refNewlineDecoder
		var dec newlineDecoder
		scan := newMarkerScan(streamTailMarkers)
		tail := streamTail{ring: make([]rune, streamTailChars)}
		refTail, refConflict, conflict := "", "", ""
		var refLive, live LiveTail
		for i := 0; i <= len(chunks); i++ {
			var chunk []byte
			if i < len(chunks) {
				chunk = chunks[i]
			}
			final := i == len(chunks)
			want, got := ref.decode(chunk, final), dec.decode(chunk, final)
			if want != got {
				t.Fatalf("round %d chunk %d decoded %q want %q", round, i, got, want)
			}
			if want == "" {
				continue
			}
			if refConflict == "" {
				lowered := pystr.Lower(refTail + want)
				for _, marker := range streamTailMarkers {
					if strings.Contains(lowered, marker) {
						refConflict = marker
						break
					}
				}
			}
			if conflict == "" {
				conflict = scan.scan(got)
			}
			if conflict != refConflict {
				t.Fatalf("round %d chunk %d conflict %q want %q", round, i, conflict, refConflict)
			}
			refTail = lastChars(refTail+want, streamTailChars)
			tail.push(got)
			if tail.String() != refTail {
				t.Fatalf("round %d chunk %d tail differs", round, i)
			}
			refLive.refWriteString(want)
			live.WriteString(got)
			if refLive.partial != live.partial || strings.Join(refLive.Tail, "\n") != strings.Join(live.Tail, "\n") {
				t.Fatalf("round %d chunk %d live tail %q %q want %q %q", round, i, live.partial, live.Tail, refLive.partial, refLive.Tail)
			}
		}
	}
	for _, s := range []string{"", "abc", "\xe2\x82abc", strings.Repeat("é", 5000), "a\xffb\xe2\x82"} {
		for _, n := range []int{0, 1, 3, 4096} {
			if got, want := lastRunes(s, n), refLastRunes(s, n); got != want {
				t.Fatalf("lastRunes(%q, %d) = %q want %q", s, n, got, want)
			}
		}
	}
}

// Line splitting of a live tail that joins every write onto the held partial line
func (t *LiveTail) refWriteString(text string) {
	joined := strings.ReplaceAll(strings.ReplaceAll(t.partial+text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(joined, "\n")
	t.partial = refLastRunes(lines[len(lines)-1], MaxLine)
	for _, line := range lines[:len(lines)-1] {
		t.line(line)
	}
}
