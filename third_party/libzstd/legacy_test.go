package libzstd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Bytes or libzstd error name the C decoder produced
type cOutcome struct {
	SHA   string `json:"sha"`
	Size  int    `json:"size"`
	Error string `json:"error"`
}

func outcomeOf(data []byte, err ErrorCode) cOutcome {
	if err != 0 {
		return cOutcome{Error: err.Error()}
	}
	sum := sha256.Sum256(data)
	return cOutcome{SHA: hex.EncodeToString(sum[:]), Size: len(data)}
}

// BUFFER_BLOCK_SIZE of CPython pycore_blocks_output_buffer.h
var cpythonBlockSizes = []int{32 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20, 8 << 20, 16 << 20, 16 << 20, 32 << 20, 32 << 20, 32 << 20, 32 << 20, 64 << 20, 64 << 20, 128 << 20, 128 << 20, 256 << 20}

// Decodes src through dc as compression.zstd.ZstdDecompressor().decompress does with no length limit
func streamLikeCPython(dc *DCtx, src []byte) ([]byte, ErrorCode) {
	var out []byte
	pos := 0
	for k := 0; ; k++ {
		block := make([]byte, cpythonBlockSizes[min(k, len(cpythonBlockSizes)-1)])
		filled := 0
		for filled < len(block) {
			n, np, hint, err := dc.DecompressStream(block[filled:], src, pos)
			if err != 0 {
				return nil, err
			}
			filled, pos = filled+n, np
			if hint == 0 || (filled < len(block) && pos == len(src)) {
				return append(out, block[:filled]...), 0
			}
		}
		out = append(out, block...)
	}
}

// Recorded frame of legacy.json, a base frame and its damage edits, with the host libzstd results
type legacyCase struct {
	Name    string   `json:"name"`
	Base    int      `json:"base"`
	Damage  [][]any  `json:"damage"`
	Cap     int      `json:"cap"`
	Oneshot cOutcome `json:"oneshot"`
	Stream  cOutcome `json:"stream"`
}

// Base frames of a blob file and the damaged frames built from them
type frameSet struct {
	Bases []struct{ Offset, Length int } `json:"bases"`
	blob  []byte
}

// Frame k of the blob with set, flip and cut edits applied in order
func (s *frameSet) frame(k int, ops [][]any) []byte {
	b := append([]byte(nil), s.blob[s.Bases[k].Offset:s.Bases[k].Offset+s.Bases[k].Length]...)
	for _, op := range ops {
		switch op[0].(string) {
		case "flip":
			b[int(op[1].(float64))] ^= 1 << uint(op[2].(float64))
		case "set":
			b[int(op[1].(float64))] = byte(op[2].(float64))
		default:
			b = b[:int(op[1].(float64))]
		}
	}
	return b
}

// Real and damaged v0.5 to v0.7 frames decode one shot like ZSTD_decompress and streamed like ZSTD_decompressStream of libzstd 1.5.7
func TestLegacyFramesMatchCPython(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "legacy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		frameSet
		Cases []legacyCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.blob, err = os.ReadFile(filepath.Join("testdata", "legacy_frames.bin")); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) != 286 {
		t.Fatalf("%d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			frame := doc.frame(c.Base, c.Damage)
			dst := make([]byte, c.Cap)
			n, e := NewDStream().Decompress(dst, frame)
			if got := outcomeOf(dst[:n], e); got != c.Oneshot {
				t.Errorf("oneshot %+v, want %+v", got, c.Oneshot)
			}
			out, e := streamLikeCPython(NewDStream(), frame)
			if got := outcomeOf(out, e); got != c.Stream {
				t.Errorf("stream %+v, want %+v", got, c.Stream)
			}
		})
	}
}
