package libzstd

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Frame decoded with a dictionary, recorded through CPython and the host libzstd
type dictCase struct {
	Name          string   `json:"name"`
	Frame         string   `json:"frame"`
	Dict          string   `json:"dict"`
	Cap           int      `json:"cap"`
	Stream        cOutcome `json:"stream"`
	UsingDict     cOutcome `json:"usingDict"`
	UsingDDict    cOutcome `json:"usingDDict"`
	UsingDDictRaw cOutcome `json:"usingDDictRaw"`
}

// One shot decode through a fresh DDict of the content type, no ddict when creation fails
func decodeWithDDict(frame, dict []byte, capacity int, ct DictContentType) cOutcome {
	d, err := NewDDict(dict, false, ct)
	if err != 0 {
		return cOutcome{Error: "no ddict"}
	}
	dst := make([]byte, capacity)
	n, e := NewDStream().DecompressUsingDDict(dst, frame, d)
	return outcomeOf(dst[:n], e)
}

// Modern and legacy frames decode with their dictionaries like ZSTD_decompress_usingDict, ZSTD_decompress_usingDDict and a ZstdDecompressor holding a ZstdDict
func TestDictionaryFramesMatchCPython(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "dict.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []dictCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 77 {
		t.Fatalf("%d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			frame, err := hex.DecodeString(c.Frame)
			if err != nil {
				t.Fatal(err)
			}
			dict, err := os.ReadFile(filepath.Join("testdata", c.Dict))
			if err != nil {
				t.Fatal(err)
			}
			dst := make([]byte, c.Cap)
			n, e := NewDStream().DecompressUsingDict(dst, frame, dict)
			if got := outcomeOf(dst[:n], e); got != c.UsingDict {
				t.Errorf("usingDict %+v, want %+v", got, c.UsingDict)
			}
			if got := decodeWithDDict(frame, dict, c.Cap, DictContentAuto); got != c.UsingDDict {
				t.Errorf("usingDDict %+v, want %+v", got, c.UsingDDict)
			}
			if got := decodeWithDDict(frame, dict, c.Cap, DictContentRaw); got != c.UsingDDictRaw {
				t.Errorf("usingDDict raw %+v, want %+v", got, c.UsingDDictRaw)
			}
			d, derr := NewDDict(dict, false, DictContentAuto)
			if derr != 0 {
				t.Fatalf("ddict %v", derr)
			}
			if wantID := uint32(0); len(dict) >= 8 && readLE32(dict, 0) == magicDictionary {
				if wantID = readLE32(dict, 4); d.DictID() != wantID {
					t.Errorf("dictID %d, want %d", d.DictID(), wantID)
				}
			}
			dc := NewDStream()
			if e := dc.RefDDict(d); e != 0 {
				t.Fatal(e)
			}
			out, e := streamLikeCPython(dc, frame)
			if got := outcomeOf(out, e); got != c.Stream {
				t.Errorf("stream %+v, want %+v", got, c.Stream)
			}
		})
	}
}
