package tokenizers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadReadsEveryVocabularyLayout(t *testing.T) {
	entries, err := os.ReadDir("testdata/layouts")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 14 {
		t.Fatalf("%d layouts", len(entries))
	}
	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			dir := filepath.Join("testdata/layouts", entry.Name())
			var want struct {
				Inputs []string `json:"inputs"`
				IDs    [][]int  `json:"ids"`
			}
			data, err := os.ReadFile(filepath.Join(dir, "expect.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &want); err != nil {
				t.Fatal(err)
			}
			enc, err := Load(Dir(dir))
			if err != nil {
				t.Fatal(err)
			}
			for i, input := range want.Inputs {
				got, err := enc.Encode(input)
				if err != nil || !slices.Equal(got, want.IDs[i]) {
					t.Errorf("Encode(%q)\n got %v %v\nwant %v", input, got, err, want.IDs[i])
				}
			}
		})
	}
}

func TestLoadPrefersTokenizerJSONAndAddsDeclaredTokens(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tokenizer.json", `{"added_tokens":[{"id":0,"content":"<unk>","special":true}],"pre_tokenizer":{"type":"Whitespace"},
		"post_processor":{"type":"TemplateProcessing","single":[{"Sequence":{"id":"A","type_id":0}},{"SpecialToken":{"id":"<unk>","type_id":0}}],"special_tokens":{"<unk>":{"id":"<unk>","ids":[0],"tokens":["<unk>"]}}},
		"model":{"type":"WordLevel","vocab":{"<unk>":0,"hello":1,"world":2},"unk_token":"<unk>"}}`)
	write("vocab.txt", "ignored\n")
	write("tokenizer_config.json", `{"add_bos_token":true,"bos_token":"<s>","unk_token":null,"added_tokens_decoder":{"7":{"content":"<tool>","special":true}},"additional_special_tokens":["<end>"]}`)
	write("special_tokens_map.json", `{"unk_token":"<never>"}`)
	enc, err := Load(Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	got, err := enc.Encode("hello<tool>world <s> there<end><never>")
	if want := []int{1, 3, 2, 4, 0, 5, 0, 0, 0, 0}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v %v, want %v", got, err, want)
	}
}

func TestLoadNamesTheFormatsWhenARepositoryShipsNone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tokenizer_config.json"), []byte(`{"tokenizer_class":"ByT5Tokenizer","auto_map":{"AutoTokenizer":["tok.Custom",null]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(Dir(dir))
	if err == nil || !strings.Contains(err.Error(), Formats) {
		t.Fatalf("error %v", err)
	}
}
