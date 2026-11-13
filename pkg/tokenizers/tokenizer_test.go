package tokenizers

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type referenceCase struct {
	Name      string                     `json:"name"`
	Tokenizer map[string]json.RawMessage `json:"tokenizer"`
	Inputs    []string                   `json:"inputs"`
	IDs       [][]int                    `json:"ids"`
	Decoded   []string                   `json:"decoded"`
	Plain     []string                   `json:"plain"`
}

// Reads the reference ids and texts the tokenizers library produced for every component of the format
func referenceCases(t *testing.T) (map[string]json.RawMessage, []referenceCase) {
	t.Helper()
	f, err := os.Open("testdata/cases.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
		Cases  []referenceCase            `json:"cases"`
	}
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc.Models, doc.Cases
}

func TestReferenceCases(t *testing.T) {
	models, cases := referenceCases(t)
	if len(cases) < 80 {
		t.Fatalf("%d reference cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var ref string
			if json.Unmarshal(c.Tokenizer["model"], &ref) == nil {
				c.Tokenizer["model"] = models[ref]
			}
			data, err := json.Marshal(c.Tokenizer)
			if err != nil {
				t.Fatal(err)
			}
			tok, err := FromJSON(data)
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range c.IDs {
				if i < len(c.Inputs) {
					got, err := tok.Encode(c.Inputs[i])
					if err != nil {
						t.Errorf("Encode(%q): %v", c.Inputs[i], err)
						continue
					}
					if !slices.Equal(got, want) {
						t.Errorf("Encode(%q)\n got %v\nwant %v", c.Inputs[i], got, want)
						continue
					}
				}
				for _, mode := range []struct {
					skip bool
					want string
				}{{false, c.Decoded[i]}, {true, c.Plain[i]}} {
					got, err := tok.Decode(want, mode.skip)
					if err != nil || got != mode.want {
						t.Errorf("Decode(%v, %v) = %q, %v, want %q", want, mode.skip, got, err, mode.want)
					}
				}
			}
		})
	}
}
