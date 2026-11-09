package pyjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func TestWTF8DecodeRune(t *testing.T) {
	cases := []struct {
		in   string
		r    rune
		size int
	}{
		{"a", 'a', 1},
		{"\xc3\xa9", 0xE9, 2},
		{"\xed\xa0\x80", 0xD800, 3},
		{"\xed\xbf\xbf", 0xDFFF, 3},
		{"\xed\xb2\x80", 0xDC80, 3},
		{"\xed\x9f\xbf", 0xD7FF, 3},
		{"\x80", 0xDC80, 1},
		{"\xff", 0xDCFF, 1},
		{"\xed\xa0", 0xDCED, 1},
		{"\xf0\x9f\x98\x80", 0x1F600, 4},
	}
	for _, c := range cases {
		if r, size := pystr.DecodeRune(c.in + "z"); r != c.r || size != c.size {
			t.Errorf("forward %q = %U %d, want %U %d", c.in, r, size, c.r, c.size)
		}
		if c.in == "\xed\xa0" {
			continue
		}
		if r, size := pystr.DecodeLastRune("z" + c.in); r != c.r || size != c.size {
			t.Errorf("backward %q = %U %d, want %U %d", c.in, r, size, c.r, c.size)
		}
	}
	if r, size := pystr.DecodeLastRune("\xed\xa0"); r != 0xDCA0 || size != 1 {
		t.Errorf("backward truncated = %U %d", r, size)
	}
}

func TestWTF8RuneCountMatchesPythonLen(t *testing.T) {
	cases := map[string]int{
		"":                         0,
		"a\xed\xa0\x80b":           3,
		"\x80\x81":                 2,
		"h\xc3\xa9llo":             5,
		"\xed\xa0":                 2,
		"\xed\xb2\x80\xed\xb2\x80": 2,
		"\xed\x9f\xbf\xed\xa0\x80": 2,
		"\xf0\x9f\x98\x80\xed":     2,
	}
	for in, want := range cases {
		if got := pystr.Len(in); got != want || len(pystr.Runes(in)) != want {
			t.Errorf("%q = %d runes %d, want %d", in, got, len(pystr.Runes(in)), want)
		}
	}
	if got := pystr.Len(pystr.SurrogateEscapeDecode([]byte("caf\xc3\xa9\x80"))); got != 5 {
		t.Errorf("python len of the surrogateescape decode is 5, got %d", got)
	}
}

func TestWTF8RunesRoundTrip(t *testing.T) {
	for _, s := range []string{"plain", "\xed\xa0\x80\xed\xb8\x80", "a\xed\xb3\xbfb", "\xf0\x9f\x98\x80"} {
		if got := pystr.FromRunes(pystr.Runes(s)); got != s {
			t.Errorf("%q round tripped to %q", s, got)
		}
	}
	if got := pystr.FromRunes(pystr.Runes("a\x80")); got != "a\xed\xb2\x80" {
		t.Errorf("stray byte canonical form %q", got)
	}
}

func TestSurrogateEscapeDecodeMatchesPython(t *testing.T) {
	cases := map[string]string{
		"\xff\x00\xfe":       "\xed\xb3\xbf\x00\xed\xb3\xbe",
		"caf\xc3\xa9\x80":    "caf\xc3\xa9\xed\xb2\x80",
		"\xed\xa0\x80":       "\xed\xb3\xad\xed\xb2\xa0\xed\xb2\x80",
		"\xe2\x82":           "\xed\xb3\xa2\xed\xb2\x82",
		"valid \xe2\x82\xac": "valid \xe2\x82\xac",
	}
	for in, want := range cases {
		if got := pystr.SurrogateEscapeDecode([]byte(in)); got != want {
			t.Errorf("%q decoded %q, want %q", in, got, want)
		}
	}
}

func TestSurrogateEscapeRoundTripsEveryByteString(t *testing.T) {
	for b := 0; b < 256; b++ {
		in := []byte{byte(b)}
		if out, err := pystr.SurrogateEscapeEncode(pystr.SurrogateEscapeDecode(in)); err != nil || !bytes.Equal(out, in) {
			t.Fatalf("byte %#x: %q %v", b, out, err)
		}
	}
	rng := rand.New(rand.NewSource(7))
	alphabet := []byte{0x00, 'a', '\n', 0x7f, 0x80, 0xbf, 0xc2, 0xc3, 0xa9, 0xe2, 0x82, 0xac, 0xed, 0xa0, 0x9f, 0xb2, 0xf0, 0x90, 0x9f, 0x98, 0xf4, 0x8f, 0xff}
	for n := 0; n < 20000; n++ {
		in := make([]byte, rng.Intn(24))
		for i := range in {
			in[i] = alphabet[rng.Intn(len(alphabet))]
		}
		out, err := pystr.SurrogateEscapeEncode(pystr.SurrogateEscapeDecode(in))
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("%q: %q %v", in, out, err)
		}
	}
}

func TestSurrogateEscapeEncodeErrorsMatchPython(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\xed\xa0\x80", "'utf-8' codec can't encode character '\\ud800' in position 0: surrogates not allowed"},
		{"\xed\xb2\x80\xed\xa0\x80", "'utf-8' codec can't encode character '\\ud800' in position 1: surrogates not allowed"},
		{"a\xed\xa0\x80\xed\xa0\x81b", "'utf-8' codec can't encode characters in position 1-2: surrogates not allowed"},
		{"\xed\xa0\x80\xed\xb2\x80", "'utf-8' codec can't encode characters in position 0-1: surrogates not allowed"},
		{"\xed\xb1\xbf", "'utf-8' codec can't encode character '\\udc7f' in position 0: surrogates not allowed"},
	}
	for _, c := range cases {
		_, err := pystr.SurrogateEscapeEncode(c.in)
		if err == nil || err.Error() != c.want {
			t.Errorf("%q: %v, want %s", c.in, err, c.want)
		}
	}
	if out, err := pystr.SurrogateEscapeEncode("\xed\xb2\x80\xed\xb2\x81"); err != nil || string(out) != "\x80\x81" {
		t.Errorf("escapable pair %q %v", out, err)
	}
}

func TestFirstUnencodableSurrogateIsTheLoneSurrogatePattern(t *testing.T) {
	for in, want := range map[string]rune{"\xed\xa0\x80": 0xD800, "\xed\xb1\xbf": 0xDC7F, "\xed\xb4\x80": 0xDD00, "\xed\xbf\xbf": 0xDFFF, "x\xed\xb2\x80\xed\xaf\xbf": 0xDBFF} {
		if r, ok := pystr.FirstUnencodableSurrogate(in); !ok || r != want {
			t.Errorf("%q = %U %v, want %U", in, r, ok, want)
		}
	}
	for _, in := range []string{"", "plain", "\xed\xb2\x80", "\xed\xb3\xbf", "\x80\xff", "\xed\x9f\xbf"} {
		if r, ok := pystr.FirstUnencodableSurrogate(in); ok {
			t.Errorf("%q matched %U", in, r)
		}
	}
}

func TestJSONDecodeWTF8KeepsLoneSurrogates(t *testing.T) {
	cases := map[string]any{
		`"\ud800"`:               "\xed\xa0\x80",
		`"\ud83d\ude00"`:         "\xf0\x9f\x98\x80",
		`"\ud800A"`:              "\xed\xa0\x80A",
		`"\ud800\u0041"`:         "\xed\xa0\x80A",
		`"\udc00\ud800"`:         "\xed\xb0\x80\xed\xa0\x80",
		`"\ud800\ud800\udc00"`:   "\xed\xa0\x80\xf0\x90\x80\x80",
		`"\uDC80"`:               "\xed\xb2\x80",
		"\"\xed\xa0\x80\"":       "\xed\xa0\x80",
		`"tab\t\u00e9\/\\"`:      "tab\t\xc3\xa9/\\",
		`["\udc80", 1, true]`:    []any{"\xed\xb2\x80", float64(1), true},
		`{"k\ud800": null}`:      map[string]any{"k\xed\xa0\x80": nil},
		`{"a": {"b": "\udfff"}}`: map[string]any{"a": map[string]any{"b": "\xed\xbf\xbf"}},
	}
	for in, want := range cases {
		got, err := DecodeWTF8([]byte(in))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v %v, want %#v", in, got, err, want)
		}
	}
}

func TestJSONDecodeWTF8RejectsStrayBytesLikePython(t *testing.T) {
	cases := map[string]string{
		"\"a\x80\"":            "'utf-8' codec can't decode byte 0x80 in position 2: invalid start byte",
		"\"a\xe2\x82\"":        "'utf-8' codec can't decode bytes in position 2-3: invalid continuation byte",
		"\"a\xe2\x82":          "'utf-8' codec can't decode bytes in position 2-3: unexpected end of data",
		"\"\xed\xa0\"":         "'utf-8' codec can't decode byte 0xed in position 1: invalid continuation byte",
		"\xff":                 "'utf-8' codec can't decode byte 0xff in position 0: invalid start byte",
		"\"a\xf0\x90\x80\"":    "'utf-8' codec can't decode bytes in position 2-4: invalid continuation byte",
		"\"\xc0\x80\"":         "'utf-8' codec can't decode byte 0xc0 in position 1: invalid start byte",
		"\"\xf4\x90\x80\x80\"": "'utf-8' codec can't decode byte 0xf4 in position 1: invalid continuation byte",
		"\"\xe0\x80\x80\"":     "'utf-8' codec can't decode byte 0xe0 in position 1: invalid continuation byte",
	}
	for in, want := range cases {
		if _, err := DecodeWTF8([]byte(in)); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %s", in, err, want)
		}
	}
}

// Random JSON documents and syntax mutations of them for parity checks
func randomJSONCorpus(rng *rand.Rand, n int) [][]byte {
	var value func(depth int) any
	value = func(depth int) any {
		switch k := rng.Intn(9); {
		case k == 0 || depth > 3:
			return []any{nil, true, false, "", "x y", "\u00e9\u2028", -0.5, 1e21, 3, 1e-7, 12345678901234567890.0}[rng.Intn(11)]
		case k < 4:
			m := map[string]any{}
			for i := rng.Intn(4); i > 0; i-- {
				m[fmt.Sprintf("k%d\t", rng.Intn(5))] = value(depth + 1)
			}
			return m
		case k < 7:
			l := []any{}
			for i := rng.Intn(4); i > 0; i-- {
				l = append(l, value(depth+1))
			}
			return l
		}
		return fmt.Sprintf("s\\%d\"\n", rng.Intn(100))
	}
	noise := []byte(`{}[]:,"\ 0123456789.eE+-tfnul` + "\t\n\r\x01x")
	var out [][]byte
	for len(out) < n {
		doc, _ := json.MarshalIndent(value(0), "", " ")
		out = append(out, doc)
		for m := 0; m < 3; m++ {
			mut := append([]byte(nil), doc...)
			for k := rng.Intn(3) + 1; k > 0 && len(mut) > 0; k-- {
				at := rng.Intn(len(mut))
				switch rng.Intn(3) {
				case 0:
					mut = append(mut[:at], mut[at+1:]...)
				case 1:
					mut[at] = noise[rng.Intn(len(noise))]
				default:
					mut = append(mut[:at], append([]byte{noise[rng.Intn(len(noise))]}, mut[at:]...)...)
				}
			}
			out = append(out, mut)
		}
	}
	return out
}

func TestJSONDecodeWTF8ScannerMatchesEncodingJSON(t *testing.T) {
	corpus := randomJSONCorpus(rand.New(rand.NewSource(11)), 20000)
	corpus = append(corpus, [][]byte{
		[]byte(""), []byte(" "), []byte("01"), []byte("-"), []byte("-0"), []byte("1."), []byte("1e"), []byte("1E+2"),
		[]byte("0.5e-3"), []byte("1e999"), []byte("-1e999"), []byte("tru"), []byte("nul"), []byte("[1,]"), []byte(`{"a":1,}`),
		[]byte(`{"a" 1}`), []byte(`{"a":1 "b":2}`), []byte(`"\x"`), []byte(`"\u12"`), []byte(`"\u12G4"`), []byte("\"a\x01\""),
		[]byte(`[1] [2]`), []byte(`{"a":1,"a":2}`), []byte("\ufeff{}"), []byte(` {"a" : [ 1 , 2 ] } `), []byte("NaN"),
		[]byte(strings.Repeat("[", 10000) + strings.Repeat("]", 10000)),
		[]byte(strings.Repeat("[", 10001) + strings.Repeat("]", 10001)),
		[]byte(strings.Repeat(`{"a":`, 10001) + "1" + strings.Repeat("}", 10001)),
	}...)
	for _, doc := range corpus {
		if !utf8.Valid(doc) {
			if _, err := decodeWTF8JSON(doc, false, false); err == nil || !strings.HasPrefix(err.Error(), "'utf-8' codec can't decode") {
				t.Fatalf("%q stray bytes gave %v", doc, err)
			}
			continue
		}
		var want any
		wantErr := json.Unmarshal(doc, &want)
		got, gotErr := decodeWTF8JSON(doc, false, false)
		if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) || (wantErr == nil && !reflect.DeepEqual(got, want)) {
			t.Fatalf("%q: got %#v %v, encoding/json %#v %v", doc, got, gotErr, want, wantErr)
		}
		numbered, numErr := decodeWTF8JSON(doc, true, false)
		if json.Valid(doc) {
			dec := json.NewDecoder(bytes.NewReader(doc))
			dec.UseNumber()
			var wantNum any
			if err := dec.Decode(&wantNum); err != nil || numErr != nil || !reflect.DeepEqual(numbered, wantNum) {
				t.Fatalf("%q use number: got %#v %v, encoding/json %#v %v", doc, numbered, numErr, wantNum, err)
			}
		} else if rawErr := json.Unmarshal(doc, new(json.RawMessage)); numErr == nil || numErr.Error() != rawErr.Error() {
			t.Fatalf("%q use number error %v, encoding/json %v", doc, numErr, rawErr)
		}
	}
}

func TestJSONDecodeWTF8OrderedKeepsInsertionOrder(t *testing.T) {
	v, err := DecodeWTF8Ordered([]byte(`{"b":1,"a":{"d":2.5,"c":["\ud800"]},"b":4}`))
	obj, ok := v.(*Object)
	if err != nil || !ok || !reflect.DeepEqual(obj.Keys, []string{"b", "a"}) || obj.Vals["b"] != json.Number("4") {
		t.Fatalf("%#v %v", v, err)
	}
	inner := obj.Vals["a"].(*Object)
	if !reflect.DeepEqual(inner.Keys, []string{"d", "c"}) || inner.Vals["d"] != json.Number("2.5") || !reflect.DeepEqual(inner.Vals["c"], []any{"\xed\xa0\x80"}) {
		t.Fatalf("inner %#v", inner)
	}
	if out, err := inner.MarshalJSON(); err != nil || string(out) != "{\"d\":2.5,\"c\":[\"\xed\xa0\x80\"]}" {
		t.Fatalf("ordered encode %q %v", out, err)
	}
}

func TestJSONEncodeWTF8MatchesEnsureASCIIFalse(t *testing.T) {
	cases := map[string]string{
		"\xed\xa0\x80":                 "\"\xed\xa0\x80\"",
		"\x80":                         "\"\xed\xb2\x80\"",
		"a\xed\xb2\x80b\xff":           "\"a\xed\xb2\x80b\xed\xb3\xbf\"",
		"a<b>&c":                       `"a<b>&c"`,
		"\u2028\u2029":                 "\"\u2028\u2029\"",
		"\x00\x1f\b\f\n\r\t\"\\\x7f":   `"\u0000\u001f\b\f\n\r\t\"\\` + "\x7f\"",
		"\xf0\x9f\x98\x80\xed\xa0\xbd": "\"\xf0\x9f\x98\x80\xed\xa0\xbd\"",
	}
	for in, want := range cases {
		if got, err := EncodeWTF8(in); err != nil || string(got) != want {
			t.Errorf("%q = %q %v, want %q", in, got, err, want)
		}
	}
	got, err := EncodeWTF8(map[string]any{"z\xed\xa0\x80": []any{"\x80", 1.5}, "a": nil})
	if err != nil || string(got) != "{\"a\":null,\"z\xed\xa0\x80\":[\"\xed\xb2\x80\",1.5]}" {
		t.Fatalf("map %q %v", got, err)
	}
}

func TestJSONEncodeWTF8ASCIIAndIndent(t *testing.T) {
	if got, err := EncodeWTF8ASCII("\xc3\xa9\xed\xa0\x80\xf0\x9f\x98\x80\x80"); err != nil || string(got) != `"\u00e9\ud800\ud83d\ude00\udc80"` {
		t.Fatalf("ascii %q %v", got, err)
	}
	got, err := EncodeWTF8Indent(map[string]any{"a": []any{1, "\xed\xa0\x80"}}, "", "  ")
	if err != nil || string(got) != "{\n  \"a\": [\n    1,\n    \"\xed\xa0\x80\"\n  ]\n}" {
		t.Fatalf("indent %q %v", got, err)
	}
}

func TestJSONEncodeWTF8RoundTripsThroughDecode(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	alphabet := []string{"a", "\"", "\\", "\n", "\x00", "\xc3\xa9", "\xed\xa0\x80", "\xed\xb2\x80", "\xed\xbf\xbf", "\xf0\x9f\x98\x80", "\u2028"}
	for n := 0; n < 5000; n++ {
		var b strings.Builder
		for k := rng.Intn(12); k > 0; k-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		s := b.String()
		enc, err := EncodeWTF8(map[string]any{s: []any{s}})
		if err != nil {
			t.Fatal(err)
		}
		dec, err := DecodeWTF8(enc)
		if err != nil || !reflect.DeepEqual(dec, map[string]any{s: []any{s}}) {
			t.Fatalf("%q -> %q -> %#v %v", s, enc, dec, err)
		}
	}
}

type wtf8Inner struct {
	Shared string `json:"shared"`
	Deep   int
}

type wtf8Embedded struct {
	wtf8Inner
	Promoted string
}

type wtf8Valuer struct{ s string }

func (v wtf8Valuer) MarshalJSON() ([]byte, error) {
	return []byte(` { "v" : ` + compactText(v.s) + ` } `), nil
}

type wtf8PtrValuer struct{ n int }

func (v *wtf8PtrValuer) MarshalJSON() ([]byte, error) { return []byte(fmt.Sprint(v.n)), nil }

type wtf8TextKey struct{ s string }

func (v wtf8TextKey) MarshalText() ([]byte, error) { return []byte("<" + v.s + ">"), nil }

type wtf8Zero struct{ n int }

func (z wtf8Zero) IsZero() bool { return z.n == 7 }

type wtf8DupA struct {
	Z int
	Y int
}

type wtf8DupB struct {
	Z int
	Y int `json:"Y"`
}

type wtf8Sample struct {
	Name      string              `json:"name"`
	Skip      string              `json:"-"`
	Dash      string              `json:"-,"`
	Empty     string              `json:"empty,omitempty"`
	ZeroInt   int                 `json:"zero_int,omitzero"`
	ZeroMeth  wtf8Zero            `json:"zero_meth,omitzero"`
	Quoted    int                 `json:"quoted,string"`
	QuotedStr string              `json:",string"`
	QuotedPtr *float64            `json:"qp,string"`
	Bytes     []byte              `json:"bytes"`
	NilBytes  []byte              `json:"nil_bytes"`
	Num       json.Number         `json:"num"`
	EmptyNum  json.Number         `json:"empty_num"`
	Raw       json.RawMessage     `json:"raw"`
	Any       any                 `json:"any"`
	NilPtr    *wtf8Inner          `json:"nil_ptr"`
	IntKeys   map[int]string      `json:"int_keys"`
	TextKeys  map[wtf8TextKey]int `json:"text_keys"`
	Valuer    wtf8Valuer          `json:"valuer"`
	PtrValuer wtf8PtrValuer       `json:"ptr_valuer"`
	Text      wtf8TextKey         `json:"text"`
	Arr       [2]float32          `json:"arr"`
	Floats    []float64           `json:"floats"`
	NilMap    map[string]string   `json:"nil_map"`
	Uint      uint8               `json:"uint"`
	unexp     int
	wtf8Embedded
	*wtf8Inner `json:"inner_ptr"`
}

func TestJSONEncodeWTF8ReflectionEncoderMatchesEncodingJSON(t *testing.T) {
	half := 0.5
	values := []any{
		nil, true, 3, int8(-4), uint64(1 << 63), 1.0, float32(0.1), 1e21, 1e-7, -0.0, 123456789.0, "plain", []int(nil), []string{},
		map[string]any{"b": 1, "a": []any{map[string]any{}}}, [0]int{}, &half, []any{nil, "x", 2.5},
		wtf8Sample{Name: "n", Skip: "s", Dash: "d", ZeroMeth: wtf8Zero{7}, Quoted: 5, QuotedStr: "q\"<", QuotedPtr: &half,
			Bytes: []byte("hi\x00"), Num: "1e3", Raw: json.RawMessage(` [1, 2] `), Any: map[string]int{"k": 1},
			IntKeys: map[int]string{10: "a", -2: "b", 3: "c"}, TextKeys: map[wtf8TextKey]int{{"b"}: 1, {"a"}: 2},
			Valuer: wtf8Valuer{"v"}, PtrValuer: wtf8PtrValuer{9}, Text: wtf8TextKey{"t"}, Arr: [2]float32{1.5, 1e-7},
			Floats: []float64{1e20, 1e21, 0.000001, 0.0000001}, Uint: 200, unexp: 1,
			wtf8Embedded: wtf8Embedded{wtf8Inner: wtf8Inner{Shared: "e", Deep: 2}, Promoted: "p"}, wtf8Inner: &wtf8Inner{Shared: "x"}},
		&wtf8Sample{ZeroInt: 3, ZeroMeth: wtf8Zero{1}, Empty: "e"},
		map[string]any{"addr": &wtf8Sample{}},
		[]wtf8PtrValuer{{1}, {2}},
		struct {
			wtf8DupA
			wtf8DupB
			C int `json:"c"`
		}{wtf8DupA{1, 4}, wtf8DupB{2, 5}, 3},
	}
	for i, v := range values {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			t.Fatalf("%d encoding/json %v", i, err)
		}
		want := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
		want = bytes.ReplaceAll(bytes.ReplaceAll(want, []byte(`\u2028`), []byte("\u2028")), []byte(`\u2029`), []byte("\u2029"))
		want = canonicalJSONNumbers(want)
		e := &wtf8EncodeState{}
		if err := e.marshal(v); err != nil || !bytes.Equal(canonicalJSONNumbers(e.buf), want) {
			t.Fatalf("%d reflection encoder\n got %s %v\nwant %s", i, e.buf, err, want)
		}
		if got, err := EncodeWTF8(v); err != nil || !bytes.Equal(canonicalJSONNumbers(got), want) {
			t.Fatalf("%d pooled path %s %v", i, got, err)
		}
	}
	bads := []struct {
		v    any
		want string
	}{
		{make(chan int), "json: unsupported type: chan int"},
		{map[[2]int]int{{1, 2}: 3}, "json: unsupported type: map[[2]int]int"},
		{json.Number("x1"), `json: invalid number literal "x1"`},
	}
	for _, bad := range bads {
		e := &wtf8EncodeState{}
		if err := e.marshal(bad.v); err == nil || err.Error() != bad.want {
			t.Fatalf("%#v: %v, want %s", bad.v, err, bad.want)
		}
		if _, wantErr := json.Marshal(bad.v); wantErr == nil {
			t.Fatalf("%#v encoding/json accepted it", bad.v)
		}
		if _, err := EncodeWTF8(bad.v); err == nil || err.Error() != bad.want {
			t.Fatalf("%#v %v", bad.v, err)
		}
	}
	type cyclic struct{ Next *cyclic }
	loop := &cyclic{}
	loop.Next = loop
	e := &wtf8EncodeState{}
	var uve *json.UnsupportedValueError
	if err := e.marshal(loop); !errors.As(err, &uve) {
		t.Fatalf("cycle %v", err)
	}
}

func BenchmarkJSONDecodeWTF8(b *testing.B) {
	b.SetBytes(int64(len(wtf8BenchArgs)))
	for b.Loop() {
		if _, err := DecodeWTF8(wtf8BenchArgs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONDecodeStdlib(b *testing.B) {
	b.SetBytes(int64(len(wtf8BenchArgs)))
	for b.Loop() {
		var v any
		if err := json.Unmarshal(wtf8BenchArgs, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONDecodeWTF8Surrogate(b *testing.B) {
	data := bytes.Replace(wtf8BenchArgs, []byte("hello"), []byte(`h\udc80llo`), 1)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := DecodeWTF8(data); err != nil {
			b.Fatal(err)
		}
	}
}

var wtf8BenchResult = map[string]any{
	"content":     strings.Repeat("     1|package main\n     2|import \"fmt\" // héllo ✓\n", 60),
	"total_lines": 120, "file_size": 4096, "truncated": false, "is_binary": false, "hint": "Use offset=121 to continue",
	"similar_files": []string{"a.go", "b.go"}, "mtime": time.Unix(0, 0).UTC().Format(time.RFC3339),
}

func BenchmarkJSONEncodeWTF8(b *testing.B) {
	for b.Loop() {
		if _, err := EncodeWTF8(wtf8BenchResult); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONEncodeStdlib(b *testing.B) {
	for b.Loop() {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(wtf8BenchResult); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONEncodeWTF8SurrogatePath(b *testing.B) {
	value := map[string]any{"content": wtf8BenchResult["content"].(string) + "\xed\xb2\x80", "total_lines": 120}
	for b.Loop() {
		if _, err := EncodeWTF8(value); err != nil {
			b.Fatal(err)
		}
	}
}

// Compact WTF-8 JSON text of v for marshaler fixtures
func compactText(v any) string {
	b, err := EncodeWTF8(v)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

var wtf8BenchArgs = []byte(`{"path": "/home/user/project/src/main.go", "old_string": "func main() {\n\tfmt.Println(\"hello, world\")\n}\n", "new_string": "func main() {\n\tfmt.Println(\"héllo, wörld ✓\")\n\tos.Exit(0)\n}\n", "replace_all": false, "context": {"lines": [1, 2, 3, 4, 5], "note": "` + strings.Repeat("plain text ", 120) + `"}}`)

func TestJSONDecodeWTF8NumbersKeepsIntText(t *testing.T) {
	v, err := DecodeWTF8Numbers([]byte(`{"exit_code":1,"ratio":1.0,"nested":[2e3,"\udc80"]}`))
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["exit_code"] != json.Number("1") || m["ratio"] != json.Number("1.0") ||
		!reflect.DeepEqual(m["nested"], []any{json.Number("2e3"), "\xed\xb2\x80"}) {
		t.Fatalf("%#v %v", v, err)
	}
	if _, err := DecodeWTF8Numbers([]byte(`{"a":`)); err == nil {
		t.Fatal("truncated document decoded")
	}
}

// Rewrites every number token of a JSON text to one float32 rounded form so Python and Go digits compare equal
func canonicalJSONNumbers(text []byte) []byte {
	out := make([]byte, 0, len(text))
	inString := false
	for i := 0; i < len(text); {
		c := text[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(text) {
				out = append(out, text[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			i++
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			i++
			continue
		}
		if c == '-' || (c >= '0' && c <= '9') {
			j := i
			for j < len(text) && strings.IndexByte("-+.eE0123456789", text[j]) >= 0 {
				j++
			}
			f, err := strconv.ParseFloat(string(text[i:j]), 64)
			if err != nil {
				panic(err)
			}
			out = strconv.AppendFloat(out, float64(float32(f)), 'g', -1, 64)
			i = j
			continue
		}
		out = append(out, c)
		i++
	}
	return out
}

// json.dumps float text, float repr digits for finite values and NaN or the infinities spelled out
func TestJSONEncodeWTF8FloatsMatchJSONDumps(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{1.0, "1.0"}, {0.0, "0.0"}, {math.Copysign(0, -1), "-0.0"}, {123456789.0, "123456789.0"}, {0.5, "0.5"}, {1.5, "1.5"},
		{1e21, "1e+21"}, {1e20, "1e+20"}, {1e16, "1e+16"}, {1e15, "1000000000000000.0"}, {1e-7, "1e-07"}, {1e-6, "1e-06"},
		{0.0001, "0.0001"}, {0.00001, "1e-05"}, {float32(0.1), "0.10000000149011612"}, {math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"}, {math.Inf(-1), "-Infinity"},
		{struct {
			Arr    [2]float32 `json:"arr"`
			Floats []float64  `json:"floats"`
		}{[2]float32{1.5, 1e-7}, []float64{1e20, 1e21, 0.000001, 0.0000001}}, `{"arr":[1.5,1.0000000116860974e-07],"floats":[1e+20,1e+21,1e-06,1e-07]}`},
		{map[string]any{"cost_usd": 0.0, "n": 3}, `{"cost_usd":0.0,"n":3}`},
	}
	for _, tc := range cases {
		got, err := EncodeWTF8(tc.v)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%#v: %s %v, want %s", tc.v, got, err, tc.want)
		}
	}
	for _, v := range []any{math.NaN(), math.Inf(1), []any{math.Inf(-1)}} {
		if _, err := EncodeWTF8ASCII(v); !errors.Is(err, ErrOutOfRange) {
			t.Fatalf("%#v ascii wire form %v", v, err)
		}
	}
	if got, err := EncodeWTF8ASCII(map[string]any{"x": 2.5, "s": "\u00e9"}); err != nil || string(got) != `{"s":"\u00e9","x":2.5}` {
		t.Fatalf("ascii %s %v", got, err)
	}
}
