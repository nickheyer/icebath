package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestALongLivedSignatureDoesNotChangeWhenTheFileSettles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("model: a\n"), 0o600)
	fresh, err := FileCacheVersion(path, true)
	if err != nil {
		t.Fatal(err)
	}
	mtimeIsRacy = func(int64) bool { return false }
	t.Cleanup(func() { mtimeIsRacy = MtimeIsRacy })
	if again, _ := FileCacheVersion(path, true); !reflect.DeepEqual(again, fresh) {
		t.Fatalf("%v != %v", again, fresh)
	}
	if settled, _ := FileCacheVersion(path, false); settled[4] != "" {
		t.Fatalf("settled digest %v", settled)
	}
	os.WriteFile(path, []byte("model: b\n"), 0o600)
	if changed, _ := FileCacheVersion(path, true); reflect.DeepEqual(changed, fresh) {
		t.Fatal("rewrite kept the signature")
	}
}

func TestAnMtimeRestoringRewriteStillGetsAContentDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(path, []byte(`{"k": "aaaa"}`), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	os.WriteFile(path, []byte(`{"k": "bbbb"}`), 0o600)
	os.Chtimes(path, old, old)
	version, _ := FileCacheVersion(path, false)
	data, _ := os.ReadFile(path)
	if version[4] != ContentDigest(data) {
		t.Fatalf("digest %v", version)
	}
}

func TestAFrozenConfigWorkerNeverDigestsConfigYAML(t *testing.T) {
	freshWorkerPolicy(t)
	dir := t.TempDir()
	cfg, other := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "auth.json")
	os.WriteFile(cfg, []byte("agent:\n  max_turns: 17\n"), 0o600)
	os.WriteFile(other, []byte("{}"), 0o600)
	if v, _ := FileCacheVersion(cfg, false); v[4] == "" {
		t.Fatal("fresh config not digested outside a worker")
	}
	if _, err := BindSafeWorkerPolicy(true, true, map[string]any{"agent": map[string]any{"max_turns": 3}}); err != nil {
		t.Fatal(err)
	}
	for _, always := range []bool{false, true} {
		if v, _ := FileCacheVersion(cfg, always); v[4] != "" {
			t.Fatalf("frozen worker digested config.yaml %v", v)
		}
		if v, _ := FileCacheVersion(other, always); v[4] == "" {
			t.Fatalf("other file not digested %v", v)
		}
	}
	if v, _ := ConfigCacheVersion(context.Background(), cfg); v[4] != "" {
		t.Fatalf("config cache version digested %v", v)
	}
}

func TestSameSizeRewriteInOneTickChangesTheConfigCacheVersion(t *testing.T) {
	freshWorkerPolicy(t)
	t.Setenv(BackendEnv, "")
	ctx := context.Background()
	tick := time.Now().UnixNano()
	fileSignature = func(fi os.FileInfo) [4]int64 { s := FileSignature(fi); return [4]int64{tick, s[1], s[2], tick} }
	t.Cleanup(func() { fileSignature = FileSignature })
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("model: a\n"), 0o600)
	first, _ := ConfigCacheVersion(ctx, path)
	doc1, _ := ReadConfigDocReadonly(ctx, path)
	f, _ := os.OpenFile(path, os.O_WRONLY, 0)
	f.WriteAt([]byte("model: b\n"), 0)
	f.Close()
	second, _ := ConfigCacheVersion(ctx, path)
	if v1, _ := ConfigVersion(ctx, path); !reflect.DeepEqual(v1[:4], first[:4]) {
		t.Fatalf("stat signature moved %v %v", v1, first)
	}
	if reflect.DeepEqual(first, second) {
		t.Fatal("same tick rewrite kept the cache version")
	}
	doc2, _ := ReadConfigDocReadonly(ctx, path)
	if !reflect.DeepEqual(doc1, map[string]any{"model": "a"}) || !reflect.DeepEqual(doc2, map[string]any{"model": "b"}) {
		t.Fatalf("readonly parse %v %v", doc1, doc2)
	}
}
