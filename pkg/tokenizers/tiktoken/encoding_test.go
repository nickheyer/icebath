package tiktoken

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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

func TestReadFileLocalSchemesAndStatus(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "ranks.tiktoken")
	os.WriteFile(local, []byte("YQ== 0\r\nYg== 1\n\nYWI= 2\n"), 0o644)
	ranks, err := LoadRanks(local, "")
	if err != nil || !reflect.DeepEqual(ranks, map[string]int{"a": 0, "b": 1, "ab": 2}) {
		t.Fatalf("%v %v", ranks, err)
	}
	if _, err := readFile("gs://bucket/x"); err == nil || strings.Contains(err.Error(), "blobfile") {
		t.Fatalf("%v", err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := readFile(srv.URL + "/x"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("%v", err)
	}
	bad := filepath.Join(dir, "bad.tiktoken")
	os.WriteFile(bad, []byte("YQ== 0\nYg==\n"), 0o644)
	if _, err := LoadRanks(bad, ""); err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), bad) {
		t.Fatalf("%v", err)
	}
}

func TestNewEncodingSplitsAndMerges(t *testing.T) {
	enc, err := New("toy", `\s+|\S+`, map[string]int{"a": 0, "b": 1, " ": 2, "ab": 3}, map[string]int{"<|end|>": 9})
	if err != nil {
		t.Fatal(err)
	}
	if got := enc.Encode("ab ba<|end|>a"); !reflect.DeepEqual(got, []int{3, 2, 1, 0, 9, 0}) {
		t.Fatalf("%v", got)
	}
	if _, err := New("bad", `(`, nil, nil); err == nil {
		t.Fatal("an unbalanced pattern compiled")
	}
}
