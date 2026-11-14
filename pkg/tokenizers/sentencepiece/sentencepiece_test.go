package sentencepiece

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func lines(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func gigaspeech(t *testing.T) *Processor {
	t.Helper()
	p, err := Load(filepath.Join("testdata", "gigaspeech_bpe.model"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEncodeMatchesTheKeywordFilesShippedWithTheModel(t *testing.T) {
	p := gigaspeech(t)
	if p.proto.modelType != modelUnigram || len(p.proto.pieces) != 500 || p.table.unkID != 2 {
		t.Fatalf("model type %d pieces %d unk %d", p.proto.modelType, len(p.proto.pieces), p.table.unkID)
	}
	raw, want := lines(t, "keywords_raw.txt"), lines(t, "keywords.txt")
	raw = append(raw, "LIGHT UP", "LOVELY CHILD", "FOREVER")
	want = append(want, lines(t, "test_keywords.txt")...)
	if len(raw) != 12 || len(want) != 12 {
		t.Fatalf("%d phrases against %d tokenizations", len(raw), len(want))
	}
	batch, err := p.EncodeBatchAsPieces(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i, phrase := range raw {
		if got := strings.Join(batch[i], " "); got != want[i] {
			t.Errorf("%q encoded as %q, the model's keyword file has %q", phrase, got, want[i])
		}
	}
}

func TestNormalizationBeforeEncoding(t *testing.T) {
	p := gigaspeech(t)
	want, err := p.EncodeAsPieces("HELLO WORLD")
	if err != nil || !reflect.DeepEqual(want, []string{"▁HE", "LL", "O", "▁WORLD"}) {
		t.Fatalf("plain phrase %v %v", want, err)
	}
	for _, variant := range []string{"  HELLO   WORLD  ", "HELLO\tWORLD", "HELLO　WORLD", "ＨＥＬＬＯ ＷＯＲＬＤ", "HELLO WORLD\n"} {
		if got, err := p.EncodeAsPieces(variant); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q encoded as %v %v", variant, got, err)
		}
	}
	for _, blank := range []string{"", " ", " \t　 "} {
		if got, err := p.EncodeAsPieces(blank); err != nil || len(got) != 0 || got == nil {
			t.Errorf("blank %q encoded as %#v %v", blank, got, err)
		}
	}
	got, err := p.EncodeAsPieces("HELLO 你好 WORLD")
	if err != nil || !reflect.DeepEqual(got, []string{"▁HE", "LL", "O", "▁", "你好", "▁WORLD"}) {
		t.Errorf("unknown run %v %v", got, err)
	}
	if n := p.normalizer.normalize("\xffA"); n != "▁\xEF\xBF\xBDA" {
		t.Errorf("invalid byte normalized to %q", n)
	}
	if n := p.normalizer.normalize("A <blk>  B"); n != "▁A▁<blk>▁B" {
		t.Errorf("user defined symbol normalized to %q", n)
	}
	got, err = p.EncodeAsPieces("A<sos/eos>B")
	if err != nil || !reflect.DeepEqual(got, []string{"▁A", "<sos/eos>", "B"}) {
		t.Errorf("user defined symbol pieces %v %v", got, err)
	}
}

func protoField(num, wire int, payload []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(num<<3|wire))
	if wire == 2 {
		out = binary.AppendUvarint(out, uint64(len(payload)))
	}
	return append(out, payload...)
}

func protoPiece(piece string, score float32, kind int) []byte {
	body := protoField(1, 2, []byte(piece))
	body = append(body, protoField(2, 5, binary.LittleEndian.AppendUint32(nil, math.Float32bits(score)))...)
	body = append(body, protoField(3, 0, []byte{byte(kind)})...)
	return protoField(1, 2, body)
}

type testPiece struct {
	piece string
	score float32
	kind  int
}

func buildModel(modelType int, trainerExtra []byte, pieces ...testPiece) []byte {
	var out []byte
	for _, p := range pieces {
		out = append(out, protoPiece(p.piece, p.score, p.kind)...)
	}
	out = append(out, protoField(2, 2, append(protoField(3, 0, []byte{byte(modelType)}), trainerExtra...))...)
	return append(out, protoField(3, 2, protoField(1, 2, []byte("identity")))...)
}

func mustLoad(t *testing.T, serialized []byte) *Processor {
	t.Helper()
	p, err := New(serialized)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBPEMergesBestScoringPairsLeftmostFirst(t *testing.T) {
	p := mustLoad(t, buildModel(modelBPE, nil, testPiece{"<unk>", 0, pieceUnknown}, testPiece{"▁", -1, pieceNormal},
		testPiece{"a", -2, pieceNormal}, testPiece{"b", -3, pieceNormal}, testPiece{"ab", -0.5, pieceNormal},
		testPiece{"▁ab", -0.25, pieceNormal}, testPiece{"abab", -4, pieceUnused}, testPiece{"ba", -0.5, pieceNormal},
		testPiece{"<sym>", 0, pieceUserDefined}))
	for input, want := range map[string][]string{
		"ab":       {"▁ab"},
		"abab":     {"▁ab", "ab"},
		"aba":      {"▁ab", "a"},
		"bab":      {"▁", "ba", "b"},
		"ababab":   {"▁ab", "ab", "ab"},
		"a<sym>b":  {"▁", "a", "<sym>", "b"},
		"abc":      {"▁ab", "c"},
		"ab cc ab": {"▁ab", "▁", "cc", "▁ab"},
	} {
		if got, err := p.EncodeAsPieces(input); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q encoded as %v %v, want %v", input, got, err, want)
		}
	}
	unused := mustLoad(t, buildModel(modelBPE, nil, testPiece{"<unk>", 0, pieceUnknown}, testPiece{"▁", -1, pieceNormal},
		testPiece{"a", -2, pieceNormal}, testPiece{"b", -3, pieceNormal}, testPiece{"ab", -0.5, pieceUnused}, testPiece{"▁a", -0.75, pieceNormal}))
	if got, err := unused.EncodeAsPieces("b ab"); err != nil || !reflect.DeepEqual(got, []string{"▁", "b", "▁", "a", "b"}) {
		t.Errorf("unused merge %v %v", got, err)
	}
	if got, err := unused.EncodeAsPieces("bab"); err != nil || !reflect.DeepEqual(got, []string{"▁", "b", "a", "b"}) {
		t.Errorf("unused piece split back %v %v", got, err)
	}
}

func TestPairOrderComparesScoreBitsAsFloats(t *testing.T) {
	bits := func(f float32) int32 { return int32(math.Float32bits(f)) }
	scores := []float32{-3.5, -1, -0.25, 0, 0.5, 2}
	for i, a := range scores {
		for j, b := range scores {
			if got := pairLess(symbolPair{score: bits(a)}, symbolPair{score: bits(b)}); got != (i < j) {
				t.Errorf("%v below %v reported %v", a, b, got)
			}
		}
	}
	if !pairLess(symbolPair{score: bits(-1), left: 5}, symbolPair{score: bits(-1), left: 2}) ||
		pairLess(symbolPair{score: bits(-1), left: 2}, symbolPair{score: bits(-1), left: 5}) {
		t.Error("equal scores must rank the leftmost pair highest")
	}
}

func TestWordAndCharModels(t *testing.T) {
	word := mustLoad(t, buildModel(modelWord, nil, testPiece{"<unk>", 0, pieceUnknown}, testPiece{"▁hello", -1, pieceNormal}, testPiece{"</s>", 0, pieceControl}))
	if got, err := word.EncodeAsPieces("hello  there hello"); err != nil || !reflect.DeepEqual(got, []string{"▁hello", "▁there", "▁hello"}) {
		t.Errorf("word model %v %v", got, err)
	}
	if got, err := word.EncodeAsPieces("x y"); err != nil || !reflect.DeepEqual(got, []string{"▁x▁y"}) {
		t.Errorf("word model unknown run %v %v", got, err)
	}
	char := mustLoad(t, buildModel(modelChar, nil, testPiece{"<unk>", 0, pieceUnknown}, testPiece{"▁", -1, pieceNormal}, testPiece{"a", -1, pieceNormal},
		testPiece{"<x>", 0, pieceUserDefined}))
	if got, err := char.EncodeAsPieces("a<x>éüa"); err != nil || !reflect.DeepEqual(got, []string{"▁", "a", "<x>", "éü", "a"}) {
		t.Errorf("char model %v %v", got, err)
	}
}

func TestByteFallbackSpellsUnknownPiecesAsBytes(t *testing.T) {
	pieces := []testPiece{{"<unk>", 0, pieceUnknown}, {"▁", -1, pieceNormal}, {"a", -1, pieceNormal}}
	for i := 0; i < 256; i++ {
		pieces = append(pieces, testPiece{byteToPiece(byte(i)), 0, pieceByte})
	}
	p := mustLoad(t, buildModel(modelUnigram, protoField(35, 0, []byte{1}), pieces...))
	if got, err := p.EncodeAsPieces("aé"); err != nil || !reflect.DeepEqual(got, []string{"▁", "a", "<0xC3>", "<0xA9>"}) {
		t.Errorf("byte fallback %v %v", got, err)
	}
}

func TestLoadErrors(t *testing.T) {
	wantErr := func(err error, msg string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Fatalf("error %v, want one holding %q", err, msg)
		}
	}
	absent := filepath.Join(t.TempDir(), "absent.model")
	_, err := Load(absent)
	wantErr(err, absent)
	_, err = New([]byte{0x0a, 0x7f, 0x01})
	wantErr(err, "not a serialized model")
	_, err = New(nil)
	wantErr(err, "unk is not defined.")
	unk := testPiece{"<unk>", 0, pieceUnknown}
	_, err = New(buildModel(modelUnigram, nil, unk, unk))
	wantErr(err, "<unk> is already defined.")
	_, err = New(buildModel(modelBPE, nil, unk, testPiece{"<unk2>", 0, pieceUnknown}))
	wantErr(err, "unk is already defined.")
	_, err = New(buildModel(modelUnigram, nil, unk, testPiece{"", 0, pieceNormal}))
	wantErr(err, "piece must not be empty.")
	_, err = New(buildModel(modelUnigram, nil, unk, testPiece{"a\x00b", 0, pieceNormal}))
	wantErr(err, "piece must not include null character.")
	_, err = New(buildModel(modelUnigram, nil, unk, testPiece{strings.Repeat("a", 8000), 0, pieceNormal}))
	wantErr(err, "piece is too long.")
	_, err = New(buildModel(modelUnigram, nil, unk, testPiece{"a", float32(math.NaN()), pieceNormal}))
	wantErr(err, "score is NaN or Inf.")
	_, err = New(buildModel(modelUnigram, nil, unk))
	wantErr(err, "no pieces are loaded.")
	_, err = New(buildModel(modelUnigram, nil, unk, testPiece{"<0x41>", 0, pieceByte}))
	wantErr(err, "byte piece <0x41> is found although `byte_fallback` is false.")
	_, err = New(buildModel(modelUnigram, protoField(35, 0, []byte{1}), unk, testPiece{"<0x41>", 0, pieceByte}))
	wantErr(err, "there are not 256 byte pieces although `byte_fallback` is true.")
	_, err = New(buildModel(modelUnigram, protoField(35, 0, []byte{1}), unk, testPiece{"<0xZZ>", 0, pieceByte}))
	wantErr(err, "byte piece <0xZZ> is invalid.")
	charsmap := func(blob []byte) []byte {
		model := protoPiece("<unk>", 0, pieceUnknown)
		model = append(model, protoPiece("a", 0, pieceNormal)...)
		return append(model, protoField(3, 2, protoField(2, 2, blob))...)
	}
	_, err = New(charsmap([]byte{1, 2, 3}))
	wantErr(err, "Blob for normalization rule is broken.")
	_, err = New(charsmap([]byte{0, 4, 0, 0, 1}))
	wantErr(err, "Trie data size exceeds the input blob size.")
	_, err = New(charsmap(append([]byte{4, 0, 0, 0}, make([]byte, 8)...)))
	wantErr(err, "Trie data size is not divisible by 1024.")
	_, err = New(charsmap(append(append([]byte{0, 4, 0, 0}, make([]byte, 1024)...), 'x')))
	wantErr(err, "normalized block must be null terminated.")
}
