package pypinyin

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

// Entries copied from pinyin_dict.json and phrases_dict.json of pypinyin 0.55.0
const (
	singleDict = `{"20013": "zhōng,zhòng", "24515": "xīn", "20320": "nǐ", "22909": "hǎo,hào", "23567": "xiǎo", "29233": "ài",
"21516": "tóng,tòng", "23398": "xué", "38134": "yín", "34892": "xíng,háng,héng,xìng,hàng", "19968": "yī,yí,yì", "20010": "gè,gě,gàn",
"38899": "yīn", "20048": "lè,yuè", "24555": "kuài", "38271": "zhǎng,cháng", "37325": "zhòng,chóng,tóng", "24198": "qìng", "12295": "líng,yuán,xīng"}`
	phraseDict = `{"你好": [["nǐ"], ["hǎo"]], "同学": [["tóng"], ["xué"]], "银行": [["yín"], ["háng"]], "一个": [["yí"], ["gè"]],
"音乐": [["yīn"], ["yuè"]], "快乐": [["kuài"], ["lè"]], "重庆": [["chóng"], ["qìng"]], "一丁不识": [["yī"], ["dīng"], ["bù"], ["shí"]],
"中心思想": [["zhōng"], ["xīn"], ["sī"], ["xiǎng"]]}`
)

func testModule(t *testing.T) *Module {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"pinyin_dict.json": singleDict, "phrases_dict.json": phraseDict} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Import(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func first(t *testing.T, m *Module, hans string) []string {
	t.Helper()
	pys, err := m.Pinyin(hans)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, item := range pys {
		if len(item) != 1 {
			t.Fatalf("%q gave %v, want one reading per item", hans, pys)
		}
		out = append(out, item[0])
	}
	return out
}

func TestPinyinReadings(t *testing.T) {
	m := testModule(t)
	for hans, want := range map[string][]string{
		"中心":           {"zhōng", "xīn"},
		"银行":           {"yín", "háng"},
		"行":            {"xíng"},
		"银行行长":         {"yín", "háng", "xíng", "zhǎng"},
		"音乐快乐":         {"yīn", "yuè", "kuài", "lè"},
		"乐":            {"lè"},
		"小爱同学":         {"xiǎo", "ài", "tóng", "xué"},
		"重庆中心":         {"chóng", "qìng", "zhōng", "xīn"},
		"一丁":           {"yī", "丁"},
		"中心思":          {"zhōng", "xīn", "思"},
		"你好 world 一个":  {"nǐ", "hǎo", " world ", "yí", "gè"},
		"hello, world": {"hello, world"},
		"〇":            {"líng"},
		"":             {},
		"你好\n":         {"nǐ", "hǎo", "\n"},
	} {
		if got := first(t, m, hans); !reflect.DeepEqual(got, want) {
			t.Errorf("%q read as %q, want %q", hans, got, want)
		}
	}
}

func TestPhrasesDisabledByEnvironment(t *testing.T) {
	t.Setenv("PYPINYIN_NO_PHRASES", "1")
	m := testModule(t)
	if got := first(t, m, "银行"); !reflect.DeepEqual(got, []string{"yín", "xíng"}) {
		t.Fatalf("without phrases 银行 read as %q", got)
	}
}

func TestImportErrors(t *testing.T) {
	var pe *PyError
	if _, err := Import(t.TempDir()); !errors.As(err, &pe) || pe.Class != "FileNotFoundError" {
		t.Fatalf("empty package directory gave %v", err)
	}
	for body, want := range map[string][2]string{
		"":             {"JSONDecodeError", "Expecting value: line 1 column 1 (char 0)"},
		`{"x": "a"}`:   {"ValueError", "invalid literal for int() with base 10: 'x'"},
		`{"19968": 1}`: {"TypeError", "pinyin_dict.json entry '19968' is not a string"},
		"[]":           {"AttributeError", "pinyin_dict.json does not hold a JSON object"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "phrases_dict.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pinyin_dict.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Import(dir); !errors.As(err, &pe) || pe.Class != want[0] || pe.Msg != want[1] {
			t.Errorf("single dictionary %q gave %v, want %v", body, err, want)
		}
	}
}

func TestInitialsAndFinals(t *testing.T) {
	for _, c := range []struct{ pinyin, initial, final string }{
		{"zhōng", "zh", "ōng"}, {"xīn", "x", "īn"}, {"xiǎo", "x", "iǎo"}, {"ài", "", "ài"}, {"tóng", "t", "óng"}, {"xué", "x", "ué"},
		{"nǐ", "n", "ǐ"}, {"hǎo", "h", "ǎo"}, {"yí", "y", "í"}, {"wèn", "w", "èn"}, {"ér", "", "ér"}, {"lüè", "l", "üè"}, {"nǚ", "n", "ǚ"},
		{"liú", "l", "iú"}, {"guì", "g", "uì"}, {"shang", "sh", "ang"}, {"ń", "", "ń"}, {"ḿ", "", "ḿ"}, {"m̄", "m", "m̄"},
		{"ê̌", "", "ê̌"}, {"HELLO", "", "HELLO"}, {" world ", "", " world "}, {"", "", ""}, {"zh", "zh", "zh"},
		{"abc5", "", "abc"}, {"shang5", "sh", "ang"}, {"zho1ng", "zh", "ōng"},
	} {
		if got := ToInitialsNotStrict(c.pinyin); got != c.initial {
			t.Errorf("initial of %q is %q, want %q", c.pinyin, got, c.initial)
		}
		if got := ToFinalsToneNotStrict(c.pinyin); got != c.final {
			t.Errorf("final of %q is %q, want %q", c.pinyin, got, c.final)
		}
	}
}
