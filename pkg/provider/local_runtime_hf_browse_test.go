package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf16"
)

const lr3GiB = int64(1) << 30

func lr3Budget(vramGiB, ramGiB float64) HardwareBudget {
	return HardwareBudget{UsableVRAMBytes: int64(vramGiB * float64(lr3GiB)), TotalDeviceBytes: int64(vramGiB * float64(lr3GiB)),
		RAMAvailableBytes: int64(ramGiB * float64(lr3GiB))}
}

func lr3CannedHF(t *testing.T, canned any) {
	previous := hfGetJSON
	hfGetJSON = func(string) (any, error) { return canned, nil }
	t.Cleanup(func() { hfGetJSON = previous })
}

func TestSearchParsesHFHits(t *testing.T) {
	lr3CannedHF(t, []any{
		map[string]any{"id": "unsloth/Qwen3.8-27B-GGUF", "downloads": 872724, "likes": 47, "lastModified": "2026-08-18", "gated": false},
		map[string]any{"id": "bartowski/whatever-GGUF", "downloads": 5, "likes": 0, "lastModified": "2026-01-01", "gated": "auto"},
	})
	hits, err := SearchModels("qwen", 20)
	if err != nil {
		t.Fatal(err)
	}
	if hits[0].Repo != "unsloth/Qwen3.8-27B-GGUF" || hits[0].Downloads != 872724 || !hits[1].Gated {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestRepoFilesGroupsSplitsAndExcludesCompanions(t *testing.T) {
	lr3CannedHF(t, []any{
		map[string]any{"path": "Qwen3.8-27B-Q4_K_M.gguf", "size": int(17 * lr3GiB)},
		map[string]any{"path": "mmproj-BF16.gguf", "size": int(1 * lr3GiB)},
		map[string]any{"path": "UD-Q8/model-00001-of-00002.gguf", "size": int(30 * lr3GiB)},
		map[string]any{"path": "UD-Q8/model-00002-of-00002.gguf", "size": int(12 * lr3GiB)},
		map[string]any{"path": "README.md", "size": 1000},
		map[string]any{"path": "dspark-draft-Q8_0.gguf", "size": int(9 * lr3GiB)},
	})
	groups, err := RepoFiles("any/repo")
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]HFFileGroup{}
	var split *HFFileGroup
	for i, g := range groups {
		labels[g.Label] = g
		if len(g.Paths) == 2 && split == nil {
			split = &groups[i]
		}
		for _, p := range g.Paths {
			if strings.Contains(p, "mmproj") || strings.Contains(p, "dspark") {
				t.Fatalf("companion listed: %s", p)
			}
		}
	}
	if g, ok := labels["Q4_K_M"]; !ok || g.TotalBytes != 17*lr3GiB {
		t.Fatalf("Q4_K_M group = %+v", g)
	}
	if split == nil || split.TotalBytes != 42*lr3GiB || !strings.HasSuffix(split.Paths[0], "00001-of-00002.gguf") {
		t.Fatalf("split group = %+v", split)
	}
	if groups[0].TotalBytes < groups[len(groups)-1].TotalBytes {
		t.Fatalf("not largest first: %+v", groups)
	}
}

func TestRoughFitBands(t *testing.T) {
	b := lr3Budget(29.6, 64)
	for size, want := range map[int64]string{20 * lr3GiB: "fits-gpu", 28 * lr3GiB: "needs-ram", 120 * lr3GiB: "too-big"} {
		if got := RoughFit(size, b); got != want {
			t.Errorf("RoughFit(%d) = %s, want %s", size, got, want)
		}
	}
}

func TestHFGetJSONDecodesPythonEncodingsAndCaches(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") != "icebath-local-models" {
			http.Error(w, "agent", http.StatusForbidden)
			return
		}
		body := []byte{0xFF, 0xFE}
		for _, u := range utf16.Encode([]rune(`[{"id":"a/b"}]`)) {
			body = append(body, byte(u), byte(u>>8))
		}
		w.Write(body)
	}))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		data, err := hfFetchJSON(srv.URL + "/api/models")
		if err != nil {
			t.Fatal(err)
		}
		if list, ok := data.([]any); !ok || len(list) != 1 || list[0].(map[string]any)["id"] != "a/b" {
			t.Fatalf("decoded = %#v", data)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("server hit %d times, want one cached fetch", n)
	}
}
