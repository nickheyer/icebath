package skills

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Fresh OSV state over a temp home and a fake endpoint answering with the given vulns
func osvFixture(t *testing.T, vulns []map[string]any, status int) (*int32, *[]map[string]any) {
	t.Helper()
	dir := t.TempDir()
	var calls int32
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if vulns == nil {
			vulns = []map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{"vulns": vulns})
	}))
	t.Cleanup(srv.Close)
	prevEndpoint, prevHome, prevTTL := osvEndpoint, osvHomeResolve, osvCacheTTL
	osvEndpoint, osvHomeResolve = srv.URL, func() string { return dir }
	osvCacheMu.Lock()
	osvCache, osvDiskLoaded = map[[3]string]osvEntry{}, false
	osvCacheMu.Unlock()
	t.Cleanup(func() {
		osvEndpoint, osvHomeResolve, osvCacheTTL = prevEndpoint, prevHome, prevTTL
		osvCacheMu.Lock()
		osvCache, osvDiskLoaded = map[[3]string]osvEntry{}, false
		osvCacheMu.Unlock()
	})
	return &calls, &bodies
}

func TestInferEcosystem(t *testing.T) {
	cases := map[string]string{
		"npx": "npm", "/usr/bin/npx": "npm", "npx.cmd": "npm", "NPX.CMD": "npm", "uvx.exe": "PyPI", "UVX.EXE": "PyPI",
		"pipx.exe": "PyPI", `C:\Program Files\nodejs\npx.cmd`: "npm", "C:/Program Files/nodejs/nPx.CmD": "npm",
		`C:\Users\u\.local\bin\UVX.EXE`: "PyPI", "C:/Users/u/.local/bin/uVx.ExE": "PyPI",
		"my-npx": "", "npx.exe": "", "npx.cmd.bak": "", "uvx.cmd.old": "", "node": "", "python": "", "/bin/bash": "",
	}
	for cmd, want := range cases {
		if got := inferEcosystem(cmd); got != want {
			t.Errorf("inferEcosystem(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestParsePackages(t *testing.T) {
	check := func(gotName, gotVer, wantName, wantVer string) {
		t.Helper()
		if gotName != wantName || gotVer != wantVer {
			t.Errorf("got (%q, %q), want (%q, %q)", gotName, gotVer, wantName, wantVer)
		}
	}
	n, v := parseNpmPackage("react")
	check(n, v, "react", "")
	n, v = parseNpmPackage("react@latest")
	check(n, v, "react", "")
	n, v = parsePypiPackage("requests")
	check(n, v, "requests", "")
	n, v = parsePypiPackage("mcp[cli]")
	check(n, v, "mcp", "")
	n, v = parsePackageFromArgs([]string{"-y", "@scope/pkg@1.0"}, "npm")
	check(n, v, "@scope/pkg", "1.0")
	n, _ = parsePackageFromArgs([]string{"--from", "mcp[cli]"}, "PyPI")
	check(n, "", "mcp", "")
	n, v = parsePackageFromArgs([]string{"-y", "react@18.3.1"}, "npm")
	check(n, v, "react", "18.3.1")
}

func TestCheckPackageForMalwareCleanAndBlocked(t *testing.T) {
	osvFixture(t, nil, 0)
	if got, err := CheckPackageForMalware("npx", []string{"-y", "@modelcontextprotocol/server-filesystem"}); err != nil || got != "" {
		t.Fatalf("clean package got %q %v", got, err)
	}
	osvFixture(t, []map[string]any{{"id": "MAL-2023-7938", "summary": "Malicious code in evil-pkg"}, {"id": "CVE-2023-1234", "summary": "Regular vuln"}}, 0)
	got, err := CheckPackageForMalware("npx", []string{"evil-pkg"})
	if err != nil || !strings.Contains(got, "BLOCKED") || !strings.Contains(got, "MAL-2023-7938") || strings.Contains(got, "CVE-2023-1234") {
		t.Fatalf("blocked verdict %q %v", got, err)
	}
}

func TestCheckPackageForMalwareUvxQueriesPyPI(t *testing.T) {
	_, bodies := osvFixture(t, nil, 0)
	CheckPackageForMalware("uvx", []string{"mcp-server-fetch"})
	pkg := (*bodies)[0]["package"].(map[string]any)
	if pkg["ecosystem"] != "PyPI" || pkg["name"] != "mcp-server-fetch" {
		t.Fatalf("query body %v", (*bodies)[0])
	}
}

func TestOSVCacheHitsAndBlockedVerdictCached(t *testing.T) {
	calls, _ := osvFixture(t, nil, 0)
	for i := 0; i < 3; i++ {
		if got, _ := CheckPackageForMalware("uvx", []string{"mcp-server-fetch"}); got != "" {
			t.Fatalf("clean got %q", got)
		}
	}
	if *calls != 1 {
		t.Fatalf("network calls %d", *calls)
	}
	calls, _ = osvFixture(t, []map[string]any{{"id": "MAL-1", "summary": "bad"}}, 0)
	first, _ := CheckPackageForMalware("npx", []string{"evil-pkg"})
	second, _ := CheckPackageForMalware("npx", []string{"evil-pkg"})
	if !strings.Contains(first, "BLOCKED") || first != second || *calls != 1 {
		t.Fatalf("blocked cache %q %q calls=%d", first, second, *calls)
	}
}

func TestOSVNetworkFailureNotCached(t *testing.T) {
	calls, _ := osvFixture(t, nil, http.StatusInternalServerError)
	if got, _ := CheckPackageForMalware("uvx", []string{"mcp-server-time"}); got != "" {
		t.Fatalf("fail open got %q", got)
	}
	if hit, _ := osvCacheGet([3]string{"PyPI", "mcp-server-time", ""}); hit {
		t.Fatal("network failure was cached")
	}
	CheckPackageForMalware("uvx", []string{"mcp-server-time"})
	if *calls != 2 {
		t.Fatalf("calls %d", *calls)
	}
}

func TestOSVCacheExpiryRequeries(t *testing.T) {
	calls, _ := osvFixture(t, nil, 0)
	osvCacheTTL = -1
	CheckPackageForMalware("uvx", []string{"mcp-server-fetch"})
	CheckPackageForMalware("uvx", []string{"mcp-server-fetch"})
	if *calls != 2 {
		t.Fatalf("calls %d", *calls)
	}
}

func TestOSVDiskCachePersistsAndReloads(t *testing.T) {
	calls, _ := osvFixture(t, nil, 0)
	CheckPackageForMalware("uvx", []string{"mcp-server-persist"})
	cacheFile := filepath.Join(osvHomeResolve(), "cache", "osv_check.json")
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("disk cache not written: %v", err)
	}
	osvCacheMu.Lock()
	osvCache, osvDiskLoaded = map[[3]string]osvEntry{}, false
	osvCacheMu.Unlock()
	CheckPackageForMalware("uvx", []string{"mcp-server-persist"})
	if *calls != 1 {
		t.Fatalf("disk cache must satisfy the second call, calls %d", *calls)
	}
}

func TestOSVDiskCacheRetriesAfterTransientError(t *testing.T) {
	osvFixture(t, nil, 0)
	cacheFile := filepath.Join(osvHomeResolve(), "cache", "osv_check.json")
	os.MkdirAll(cacheFile, 0o755)
	osvCacheMu.Lock()
	loadOSVDiskCache()
	loaded := osvDiskLoaded
	osvCacheMu.Unlock()
	if loaded {
		t.Fatal("transient read error latched the disk cache")
	}
	os.Remove(cacheFile)
	data, _ := json.Marshal(map[string]any{"version": 1, "entries": map[string]any{
		"PyPI|mcp-server-retry|": map[string]any{"expiry": osvNow() + 3600, "result": nil}}})
	os.WriteFile(cacheFile, data, 0o644)
	osvCacheMu.Lock()
	loadOSVDiskCache()
	_, has := osvCache[[3]string{"PyPI", "mcp-server-retry", ""}]
	loaded = osvDiskLoaded
	osvCacheMu.Unlock()
	if !loaded || !has {
		t.Fatalf("reload loaded=%v has=%v", loaded, has)
	}
}
