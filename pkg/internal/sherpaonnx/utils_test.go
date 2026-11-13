package sherpaonnx

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypinyin"
)

const (
	gigaspeechTokens = "../../tokenizers/sentencepiece/testdata/gigaspeech_tokens.txt"
	gigaspeechBPE    = "../../tokenizers/sentencepiece/testdata/gigaspeech_bpe.model"
)

func textModule(t *testing.T) (*Module, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"pinyin_dict.json":  `{"23567": "xiǎo", "29233": "ài", "21516": "tóng,tòng", "23398": "xué", "20320": "nǐ", "22909": "hǎo,hào"}`,
		"phrases_dict.json": `{"同学": [["tóng"], ["xué"]], "你好": [["nǐ"], ["hǎo"]]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := &bytes.Buffer{}
	return &Module{env: Environment{Stdout: out,
		ImportPypinyin: func() (*pypinyin.Module, error) { return pypinyin.Import(dir) }}}, out
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func rows(lists ...[]string) [][]any {
	out := [][]any{}
	for _, list := range lists {
		row := []any{}
		for _, item := range list {
			row = append(row, item)
		}
		out = append(out, row)
	}
	return out
}

func TestText2TokenBPE(t *testing.T) {
	m, out := textModule(t)
	bpe := gigaspeechBPE
	got, err := m.Text2Token([]string{"HELLO WORLD", "HELLO 你好", "HEY SIRI"}, gigaspeechTokens, Text2TokenOptions{TokensType: "bpe", BPEModel: &bpe})
	if err != nil {
		t.Fatal(err)
	}
	if want := rows([]string{"▁HE", "LL", "O", "▁WORLD"}, []string{"▁HE", "Y", "▁S", "I", "RI"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens %v, want %v", got, want)
	}
	wantOut := "Can't find token 你好 in token table, check your tokens.txt see if 你好 in it. skipping text : ['▁HE', 'LL', 'O', '▁', '你好'].\n"
	if out.String() != wantOut {
		t.Fatalf("printed %q, want %q", out.String(), wantOut)
	}
	ids, err := m.Text2Token([]string{"HELLO WORLD"}, gigaspeechTokens, Text2TokenOptions{TokensType: "bpe", BPEModel: &bpe, OutputIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	table := map[string]int{}
	raw, _ := os.ReadFile(gigaspeechTokens)
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		table[strings.Fields(line)[0]] = i
	}
	if want := [][]any{{table["▁HE"], table["LL"], table["O"], table["▁WORLD"]}}; !reflect.DeepEqual(ids, want) || table["▁WORLD"] == 0 {
		t.Fatalf("ids %v, want %v", ids, want)
	}
}

func TestText2TokenCharacterAndPinyinTypes(t *testing.T) {
	m, out := textModule(t)
	cjk := writeFile(t, "你 0\r\n好 1\r\n")
	got, err := m.Text2Token([]string{" 你 好\t你 ", "你们", ""}, cjk, Text2TokenOptions{})
	if err != nil || !reflect.DeepEqual(got, rows([]string{"你", "好", "你"}, []string{})) {
		t.Fatalf("cjkchar %v %v", got, err)
	}
	if want := "Can't find token 们 in token table, check your tokens.txt see if 们 in it. skipping text : ['你', '们'].\n"; out.String() != want {
		t.Fatalf("printed %q", out.String())
	}
	out.Reset()
	pinyinTokens := writeFile(t, "x 0\niǎo 1\nài 2\nt 3\nóng 4\nué 5\nxiǎo 6\ntóng 7\nxué 8\nn 9\nǐ 10\nh 11\nǎo 12\nHH 13\nAH0 14\nL 15\nOW1 16\n")
	got, err = m.Text2Token([]string{"小爱同学"}, pinyinTokens, Text2TokenOptions{TokensType: "ppinyin"})
	if err != nil || !reflect.DeepEqual(got, rows([]string{"x", "iǎo", "ài", "t", "óng", "x", "ué"})) {
		t.Fatalf("ppinyin %v %v", got, err)
	}
	got, err = m.Text2Token([]string{"小爱同学", "小 爱"}, pinyinTokens, Text2TokenOptions{TokensType: "fpinyin"})
	if err != nil || !reflect.DeepEqual(got, rows([]string{"xiǎo", "ài", "tóng", "xué"})) {
		t.Fatalf("fpinyin %v %v", got, err)
	}
	if want := "Can't find token   in token table, check your tokens.txt see if   in it. skipping text : ['xiǎo', ' ', 'ài'].\n"; out.String() != want {
		t.Fatalf("printed %q", out.String())
	}
	out.Reset()
	lexicon := writeFile(t, "HELLO HH AH0 L OW1\n")
	got, err = m.Text2Token([]string{" HELLO 你好 ", "HELLO FOO 你好", "你好"}, pinyinTokens, Text2TokenOptions{TokensType: "phone+ppinyin", Lexicon: &lexicon})
	if err != nil || !reflect.DeepEqual(got, rows([]string{"HH", "AH0", "L", "OW1", "n", "ǐ", "h", "ǎo"}, []string{"n", "ǐ", "h", "ǎo"})) {
		t.Fatalf("phone+ppinyin %v %v", got, err)
	}
	if want := "Word FOO not in lexicon and it is not a CJK character, skipping text: HELLO FOO 你好.\n"; out.String() != want {
		t.Fatalf("printed %q", out.String())
	}
	raw, _ := os.ReadFile(gigaspeechTokens)
	mixed := writeFile(t, string(raw)+"你 500\n好 501\n")
	bpe := gigaspeechBPE
	got, err = m.Text2Token([]string{"你好 HELLO WORLD 好"}, mixed, Text2TokenOptions{TokensType: "cjkchar+bpe", BPEModel: &bpe})
	if err != nil || !reflect.DeepEqual(got, rows([]string{"你", "好", "▁HE", "LL", "O", "▁WORLD", "好"})) {
		t.Fatalf("cjkchar+bpe %v %v", got, err)
	}
}

func TestText2TokenErrors(t *testing.T) {
	m, out := textModule(t)
	bpe := gigaspeechBPE
	absent := filepath.Join(t.TempDir(), "absent")
	_, err := m.Text2Token(nil, absent, Text2TokenOptions{})
	wantPyError(t, err, "AssertionError", "File not exists, "+absent)
	_, err = m.Text2Token(nil, writeFile(t, "a 1 2\n"), Text2TokenOptions{})
	wantPyError(t, err, "AssertionError", "3")
	_, err = m.Text2Token(nil, writeFile(t, "a 1\n\nb 2\n"), Text2TokenOptions{})
	wantPyError(t, err, "AssertionError", "0")
	_, err = m.Text2Token(nil, writeFile(t, "a 1\na 2\n"), Text2TokenOptions{})
	wantPyError(t, err, "AssertionError", "Duplicate token: ['a', '2'] ")
	_, err = m.Text2Token(nil, writeFile(t, "a one\n"), Text2TokenOptions{})
	wantPyError(t, err, "ValueError", "invalid literal for int() with base 10: 'one'")
	_, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "bpe"})
	wantPyError(t, err, "TypeError", "argument should be a str or an os.PathLike object where __fspath__ returns a str, not 'NoneType'")
	_, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "bpe", BPEModel: &absent})
	wantPyError(t, err, "AssertionError", "File not exists, "+absent)
	_, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "words"})
	wantPyError(t, err, "AssertionError", "Supported tokens_type are cjkchar, bpe, cjkchar+bpe, ppinyin, fpinyin, phone+ppinyin given words")
	_, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "xbpe", BPEModel: &bpe})
	wantPyError(t, err, "AssertionError", "Supported tokens_type are cjkchar, bpe, cjkchar+bpe, ppinyin, fpinyin, phone+ppinyin given xbpe")
	_, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "phone+ppinyin"})
	wantPyError(t, err, "AssertionError", "File not exists, None")
	_, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "phone+ppinyin", Lexicon: &absent})
	wantPyError(t, err, "AssertionError", "File not exists, "+absent)
	lexicon := writeFile(t, "HELLO\n")
	_, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "phone+ppinyin", Lexicon: &lexicon})
	wantPyError(t, err, "AssertionError", "1")
	if out.Len() != 0 {
		t.Fatalf("printed %q", out.String())
	}
	missing := errors.New("No module named 'pypinyin'")
	m.env.ImportPypinyin = func() (*pypinyin.Module, error) { return nil, missing }
	if _, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "ppinyin"}); err != missing || out.Len() != 0 {
		t.Fatalf("pinyin dictionary failure gave %v, printed %q", err, out.String())
	}
	if _, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{}); err != nil {
		t.Fatalf("a token type without pinyin asked for the dictionaries: %v", err)
	}
	m.env.ImportPypinyin = nil
	if _, err = m.Text2Token(nil, gigaspeechTokens, Text2TokenOptions{TokensType: "fpinyin"}); err == nil || !strings.Contains(err.Error(), "needs pinyin dictionaries") {
		t.Fatalf("pinyin tokens without a dictionary source gave %v", err)
	}
}
