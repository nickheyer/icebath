package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func secretCachePayload(t *testing.T, key string, secrets map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"key": key, "secrets": secrets, "fetched_at": nowEpoch()})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func previousSecretCache(t *testing.T, h, name string) string {
	t.Helper()
	path := filepath.Join(h, "cache", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func cachedSecretsAt(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	secrets, _ := payload["secrets"].(map[string]any)
	return secrets
}

func writeTestFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSecretCachesLiveAtTheTopOfTheProfileHome(t *testing.T) {
	h := t.TempDir()
	if bwsStore.Disk.Path(h) != filepath.Join(h, "bws_cache.json") || encryptedBwsCachePath(h) != filepath.Join(h, "bws_cache.enc.json") || opStore.Disk.Path(h) != filepath.Join(h, "op_cache.json") {
		t.Fatal("every secret cache sits at the top of the home")
	}
}

func TestSecretCacheWriteLandsInTheProfileHomeAtModeSixHundred(t *testing.T) {
	h := t.TempDir()
	NewSecretDiskCache("probe.json").Write("k", CachedFetch{Secrets: map[string]string{"A": "b"}, FetchedAt: nowEpoch()}, 60, h)
	if got := cachedSecretsAt(t, filepath.Join(h, "probe.json")); got["A"] != "b" || len(got) != 1 {
		t.Fatalf("the entry lands in the home, got %v", got)
	}
	if info, err := os.Stat(filepath.Join(h, "probe.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the cache is private, got %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(h, "cache")); !os.IsNotExist(err) {
		t.Fatal("no cache folder is created")
	}
}

func TestSecretCacheFirstReadDeletesAPreviousCopyUnread(t *testing.T) {
	h := t.TempDir()
	previous := previousSecretCache(t, h, "probe.json")
	writeTestFile(t, previous, secretCachePayload(t, "k", map[string]string{"A": "b"}))
	if _, found, err := NewSecretDiskCache("probe.json").Read("k", 60, h); found || err != nil {
		t.Fatal("a previous copy is never read")
	}
	if fileExists(previous) || fileExists(filepath.Join(h, "probe.json")) {
		t.Fatal("the previous copy is deleted and not moved")
	}
}

func TestSecretCacheCurrentWinsAndThePreviousCopyIsRemoved(t *testing.T) {
	h := t.TempDir()
	writeTestFile(t, filepath.Join(h, "probe.json"), secretCachePayload(t, "k", map[string]string{"A": "current"}))
	previous := previousSecretCache(t, h, "probe.json")
	writeTestFile(t, previous, secretCachePayload(t, "k", map[string]string{"A": "previous"}))
	entry, found, err := NewSecretDiskCache("probe.json").Read("k", 60, h)
	if !found || err != nil || entry.Secrets["A"] != "current" {
		t.Fatalf("the current cache wins, got %v %v", entry, err)
	}
	if fileExists(previous) {
		t.Fatal("the previous copy is removed")
	}
}

func TestSecretCacheWriteDeletesAPreviousCopy(t *testing.T) {
	h := t.TempDir()
	previous := previousSecretCache(t, h, "probe.json")
	writeTestFile(t, previous, secretCachePayload(t, "k", map[string]string{"A": "previous"}))
	NewSecretDiskCache("probe.json").Write("k", CachedFetch{Secrets: map[string]string{"A": "c"}, FetchedAt: nowEpoch()}, 60, h)
	if fileExists(previous) || cachedSecretsAt(t, filepath.Join(h, "probe.json"))["A"] != "c" {
		t.Fatal("the write replaces the previous copy")
	}
}

func TestSecretCachePreviousSymlinkIsRemovedAndItsTargetKept(t *testing.T) {
	h := t.TempDir()
	target := filepath.Join(h, "workspace", "leak.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, []byte("{}"))
	previous := previousSecretCache(t, h, "probe.json")
	if err := os.Symlink(target, previous); err != nil {
		t.Skipf("creating a symlink needs a privilege this host lacks: %v", err)
	}
	NewSecretDiskCache("probe.json").Write("k", CachedFetch{Secrets: map[string]string{"A": "c"}, FetchedAt: nowEpoch()}, 60, h)
	if _, err := os.Lstat(previous); !os.IsNotExist(err) {
		t.Fatal("the previous symlink is removed")
	}
	if raw, _ := os.ReadFile(target); string(raw) != "{}" {
		t.Fatal("the symlink target is kept")
	}
	if cachedSecretsAt(t, filepath.Join(h, "probe.json"))["A"] != "c" {
		t.Fatal("the write lands in the home")
	}
}

func TestSecretCachePreviousDirectoryIsLeftAlone(t *testing.T) {
	h := t.TempDir()
	inner := filepath.Join(previousSecretCache(t, h, "probe.json"), "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	NewSecretDiskCache("probe.json").Write("k", CachedFetch{Secrets: map[string]string{"A": "c"}, FetchedAt: nowEpoch()}, 60, h)
	if info, err := os.Stat(inner); err != nil || !info.IsDir() {
		t.Fatal("a previous directory is left alone")
	}
	if cachedSecretsAt(t, filepath.Join(h, "probe.json"))["A"] != "c" {
		t.Fatal("the write lands in the home")
	}
}

func TestSecretCacheClearRemovesBothLocations(t *testing.T) {
	h := t.TempDir()
	writeTestFile(t, filepath.Join(h, "probe.json"), secretCachePayload(t, "k", map[string]string{"A": "b"}))
	previous := previousSecretCache(t, h, "probe.json")
	writeTestFile(t, previous, secretCachePayload(t, "k", map[string]string{"A": "b"}))
	NewSecretDiskCache("probe.json").Clear(h)
	if fileExists(filepath.Join(h, "probe.json")) || fileExists(previous) {
		t.Fatal("clear removes both locations")
	}
}

func TestOnePasswordCacheDeletesThePreviousCopyUnread(t *testing.T) {
	h := t.TempDir()
	key := "auth-fp|account|refs-fp"
	previous := previousSecretCache(t, h, "op_cache.json")
	writeTestFile(t, previous, secretCachePayload(t, key, map[string]string{"OP_KEY": "v"}))
	if _, found, _ := opStore.Disk.Read(key, 60, h); found {
		t.Fatal("a previous copy is never read")
	}
	if fileExists(previous) || fileExists(filepath.Join(h, "op_cache.json")) {
		t.Fatal("the previous copy is deleted and not moved")
	}
}

func TestBitwardenEncryptedCacheDeletesThePreviousCopyUnread(t *testing.T) {
	h := t.TempDir()
	key := SecretFingerprint("0.t") + "|proj-1|"
	writeEncryptedBwsCache(key, "0.t", CachedFetch{Secrets: map[string]string{"K": "v"}, FetchedAt: nowEpoch()}, h)
	previous := previousSecretCache(t, h, "bws_cache.enc.json")
	if err := os.Rename(filepath.Join(h, "bws_cache.enc.json"), previous); err != nil {
		t.Fatal(err)
	}
	if _, found := readEncryptedBwsCache(key, "0.t", 3600, h); found {
		t.Fatal("a previous encrypted copy is never read")
	}
	if fileExists(previous) || fileExists(filepath.Join(h, "bws_cache.enc.json")) {
		t.Fatal("the previous copy is deleted and not moved")
	}
}

func TestBitwardenClearCachesRemovesPreviousCopies(t *testing.T) {
	h := t.TempDir()
	for _, name := range []string{"bws_cache.json", "bws_cache.enc.json"} {
		writeTestFile(t, previousSecretCache(t, h, name), []byte("{}"))
		writeTestFile(t, filepath.Join(h, name), []byte("{}"))
	}
	ClearBitwardenCaches(h)
	for _, dir := range []string{h, filepath.Join(h, "cache")} {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if !entry.IsDir() {
				t.Fatalf("leftover cache file %s", entry.Name())
			}
		}
	}
}

func TestSecretCacheReadKeepsPythonJSONAndRaisesOnUndecodableBytes(t *testing.T) {
	h := t.TempDir()
	cache := NewSecretDiskCache("probe.json")
	path := filepath.Join(h, "probe.json")
	writeTestFile(t, path, []byte("\xef\xbb\xbf"+`{"key": 7, "secrets": {"A": "b"}, "fetched_at": 1e12}`))
	if _, found, err := cache.Read("7", 60, h); found || err != nil {
		t.Fatal("a non string key never matches")
	}
	writeTestFile(t, path, []byte(`{"key": "k", "secrets": {"A": "b"}, "fetched_at": NaN}`))
	if _, found, err := cache.Read("k", 60, h); found || err != nil {
		t.Fatal("a NaN timestamp parses and is never fresh")
	}
	writeTestFile(t, path, []byte("{\"key\": \"k\xff\"}"))
	if _, _, err := cache.Read("k", 60, h); err == nil || !strings.Contains(err.Error(), "can't decode byte 0xff") {
		t.Fatalf("undecodable bytes raise the decode error, got %v", err)
	}
}
