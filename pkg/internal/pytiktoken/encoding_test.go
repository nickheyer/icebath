package pytiktoken

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// Serves the cl100k rank subset the vectors need and points the loader at it with an empty cache
func serveSubset(t *testing.T) (*atomic.Int32, string) {
	t.Helper()
	data, err := os.ReadFile("testdata/cl100k_subset.tiktoken")
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(data)
	prevBlob, prevHash := CL100kBlob, CL100kHash
	CL100kBlob, CL100kHash = srv.URL+"/encodings/cl100k_base.tiktoken", hex.EncodeToString(sum[:])
	cache := t.TempDir()
	t.Setenv("TIKTOKEN_CACHE_DIR", cache)
	resetEncodings()
	t.Cleanup(func() {
		CL100kBlob, CL100kHash = prevBlob, prevHash
		resetEncodings()
	})
	return &hits, cache
}

func resetEncodings() {
	encodingsMu.Lock()
	encodings = map[string]*Encoding{}
	encodingsMu.Unlock()
}

func TestCL100kEncodeMatchesTiktoken(t *testing.T) {
	serveSubset(t)
	raw, err := os.ReadFile("testdata/cl100k_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Text   string `json:"text"`
			Tokens []int  `json:"tokens"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	enc, err := CL100kBase()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range vectors.Cases {
		got, err := enc.Encode(c.Text)
		if err != nil || !reflect.DeepEqual(got, c.Tokens) {
			t.Fatalf("%q: %v %v want %v", c.Text, got, err, c.Tokens)
		}
	}
}

func TestEncodeRejectsDisallowedSpecialTokens(t *testing.T) {
	serveSubset(t)
	enc, err := CL100kBase()
	if err != nil {
		t.Fatal(err)
	}
	_, err = enc.Encode("x <|endoftext|> y")
	want := "ValueError: Encountered text corresponding to disallowed special token '<|endoftext|>'.\nIf you want this text to be encoded as a special token, pass it to `allowed_special`, e.g. `allowed_special={'<|endoftext|>', ...}`.\nIf you want this text to be encoded as normal text, disable the check for this token by passing `disallowed_special=(enc.special_tokens_set - {'<|endoftext|>'})`.\nTo disable this check for all special tokens, pass `disallowed_special=()`.\n"
	if err == nil || err.Error() != want {
		t.Fatalf("%v", err)
	}
}

func TestBytePairMergePathsAgree(t *testing.T) {
	ranks := map[string]int{"a": 0, "b": 1, "c": 2, "ab": 3, "bc": 4, "abc": 5, "aa": 6}
	for _, piece := range []string{"ab", "abc", "aab", "cab", "abcabc", "aaaa"} {
		small := bytePairEncode([]byte(piece), ranks)
		large := bytePairMergeLarge(ranks, []byte(piece))
		if !reflect.DeepEqual(small, large) {
			t.Fatalf("%s: %v %v", piece, small, large)
		}
	}
	if got := bytePairEncode([]byte("abc"), ranks); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("abc %v", got)
	}
	if got := bytePairEncode([]byte("aaaa"), ranks); !reflect.DeepEqual(got, []int{6, 6}) {
		t.Fatalf("aaaa %v", got)
	}
}

func TestReadFileCachedUsesAndRepairsTheCache(t *testing.T) {
	hits, cache := serveSubset(t)
	if _, err := CL100kBase(); err != nil {
		t.Fatal(err)
	}
	key := sha1.Sum([]byte(CL100kBlob))
	cachePath := filepath.Join(cache, hex.EncodeToString(key[:]))
	if _, err := os.Stat(cachePath); err != nil || hits.Load() != 1 {
		t.Fatalf("cache %v hits %d", err, hits.Load())
	}
	if _, err := ReadFileCached(CL100kBlob, CL100kHash); err != nil || hits.Load() != 1 {
		t.Fatalf("cached read %v hits %d", err, hits.Load())
	}
	if err := os.WriteFile(cachePath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFileCached(CL100kBlob, CL100kHash)
	if err != nil || hits.Load() != 2 || !checkHash(data, CL100kHash) {
		t.Fatalf("refetch %v hits %d", err, hits.Load())
	}
	if again, _ := os.ReadFile(cachePath); !checkHash(again, CL100kHash) {
		t.Fatal("cache not rewritten")
	}
	entries, _ := os.ReadDir(cache)
	if len(entries) != 1 {
		t.Fatalf("leftover temp files %v", entries)
	}
}

func TestReadFileCachedHashMismatchAndCacheDirs(t *testing.T) {
	hits, _ := serveSubset(t)
	_, err := ReadFileCached(CL100kBlob, strings.Repeat("0", 64))
	want := "ValueError: Hash mismatch for data downloaded from " + CL100kBlob + " (expected " + strings.Repeat("0", 64) + "). This may indicate a corrupted download. Please try again."
	if err == nil || err.Error() != want {
		t.Fatalf("%v", err)
	}
	t.Setenv("TIKTOKEN_CACHE_DIR", "")
	before := hits.Load()
	for i := 0; i < 2; i++ {
		if _, err := ReadFileCached(CL100kBlob, CL100kHash); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != before+2 {
		t.Fatalf("empty cache dir cached, hits %d", hits.Load())
	}
	os.Unsetenv("TIKTOKEN_CACHE_DIR")
	gym := t.TempDir()
	t.Setenv("DATA_GYM_CACHE_DIR", gym)
	if _, err := ReadFileCached(CL100kBlob, CL100kHash); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(gym); len(entries) != 1 {
		t.Fatalf("data gym cache %v", entries)
	}
}

func TestReadFileLocalSchemesAndStatus(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "ranks.tiktoken")
	os.WriteFile(local, []byte("YQ== 0\r\nYg== 1\n\nYWI= 2\n"), 0o644)
	ranks, err := LoadTiktokenBPE(local, "")
	if err != nil || !reflect.DeepEqual(ranks, map[string]int{"a": 0, "b": 1, "ab": 2}) {
		t.Fatalf("%v %v", ranks, err)
	}
	if _, err := readFile("gs://bucket/x"); err == nil || strings.Contains(err.Error(), "blobfile") {
		t.Fatalf("%v", err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := readFile(srv.URL + "/x"); err == nil || err.Error() != "404 Client Error: Not Found for url: "+srv.URL+"/x" {
		t.Fatalf("%v", err)
	}
	bad := filepath.Join(dir, "bad.tiktoken")
	os.WriteFile(bad, []byte("YQ== 0\nYg==\n"), 0o644)
	if _, err := LoadTiktokenBPE(bad, ""); err == nil || err.Error() != "ValueError: Error parsing line b'Yg==' in "+bad {
		t.Fatalf("%v", err)
	}
}

func TestNewEncodingSplitsAndMerges(t *testing.T) {
	enc := NewEncoding("toy", `\s+|\S+`, map[string]int{"a": 0, "b": 1, " ": 2, "ab": 3}, map[string]int{"<|end|>": 9})
	got, err := enc.Encode("ab ba")
	if err != nil || !reflect.DeepEqual(got, []int{3, 2, 1, 0}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := enc.Encode("a<|end|>"); err == nil || !strings.Contains(err.Error(), "'<|end|>'") {
		t.Fatalf("%v", err)
	}
}
