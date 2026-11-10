package libzstd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Every damaged frame of the pyzip fuzz table decodes one shot like ZSTD_decompress at full and half capacity
func TestFuzzFramesMatchZSTDDecompress(t *testing.T) {
	dir := filepath.Join("..", "..", "pkg", "internal", "pyzip", "testdata", "stream")
	raw, err := os.ReadFile(filepath.Join(dir, "fuzz.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		frameSet
		Cases []struct {
			Base   int     `json:"base"`
			Damage [][]any `json:"damage"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.blob, err = os.ReadFile(filepath.Join(dir, "fuzz_bases.bin")); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join("testdata", "fuzz_c.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want []struct {
		Cap     []int      `json:"cap"`
		Oneshot []cOutcome `json:"oneshot"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(doc.Cases) || len(want) < 1000 {
		t.Fatalf("%d recorded for %d cases", len(want), len(doc.Cases))
	}
	for k, c := range doc.Cases {
		frame := doc.frame(c.Base, c.Damage)
		for i, capacity := range want[k].Cap {
			dst := make([]byte, capacity)
			n, e := NewDStream().Decompress(dst, frame)
			if got := outcomeOf(dst[:n], e); got != want[k].Oneshot[i] {
				t.Errorf("case %d capacity %d: %+v, want %+v", k, capacity, got, want[k].Oneshot[i])
			}
		}
	}
}
