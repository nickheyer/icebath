package config

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const integrityConfig = facadeConfig + "approvals:\n  deny:\n  - rm -rf /\n"

// Counts parses of one config file and fails the chosen one with a transient EMFILE
type readFaults struct {
	path          string
	count, failAt int
}

func installReadFaults(t *testing.T, path string) *readFaults {
	t.Helper()
	f := &readFaults{path: path}
	prev := readYAMLFile
	readYAMLFile = func(name string) ([]byte, error) {
		if name == f.path {
			f.count++
			if f.count == f.failAt {
				return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EMFILE}
			}
		}
		return prev(name)
	}
	t.Cleanup(func() { readYAMLFile = prev })
	return f
}

func (f *readFaults) arm(failAt int) { f.count, f.failAt = 0, failAt }

// Text on a new inode with cold caches, the last good copy and backups dropped unless kept
func freshConfigProcess(t *testing.T, h, text string, keepLastKnownGood bool) {
	t.Helper()
	loadCacheMu.Lock()
	loadCache, rawCache = map[string]*loadEntry{}, map[string]rawEntry{}
	if !keepLastKnownGood {
		lastExpanded = map[string]map[string]any{}
	}
	loadCacheMu.Unlock()
	if !keepLastKnownGood {
		os.RemoveAll(filepath.Join(h, "backups"))
	}
	tmp := filepath.Join(h, ".config.yaml.new")
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(h, "config.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestNoSingleTransientReadErrorReachesTheFile(t *testing.T) {
	ctx := context.Background()
	writes := map[string]func(h string) error{
		"load_config": func(h string) error {
			cfg, err := Load(h, facadeLookup)
			if err != nil {
				return err
			}
			cfg.Map("display")["skin"] = "ares"
			return SaveConfig(ctx, h, cfg, SaveOptions{})
		},
		"read_raw_config": func(h string) error {
			raw, err := ReadRawConfig(ctx, h)
			if err != nil {
				return err
			}
			ensureDict(raw, "display")["skin"] = "ares"
			return SaveConfig(ctx, h, raw, SaveOptions{})
		},
	}
	for name, write := range writes {
		for _, lastKnownGood := range []string{"none", "stale"} {
			t.Run(name+"/"+lastKnownGood, func(t *testing.T) {
				h, path := facadeHome(t, integrityConfig)
				before := integrityConfig + "timezone: Europe/Paris\n"
				faults := installReadFaults(t, path)
				run := func(k int) bool {
					freshConfigProcess(t, h, before, false)
					if lastKnownGood == "stale" {
						freshConfigProcess(t, h, integrityConfig, false)
						if _, err := Load(h, facadeLookup); err != nil {
							t.Fatal(err)
						}
						freshConfigProcess(t, h, before, true)
					}
					faults.arm(k)
					return write(h) == nil
				}
				if !run(0) {
					t.Fatal("the fault free write was refused")
				}
				goodAfter, _ := os.ReadFile(path)
				reads := faults.count
				if !bytes.Contains(goodAfter, []byte("skin: ares")) || reads < 1 {
					t.Fatalf("reads %d saved %s", reads, goodAfter)
				}
				for k := 1; k <= reads; k++ {
					if !run(k) {
						if now, _ := os.ReadFile(path); string(now) != before {
							t.Fatalf("read %d/%d failed, file changed\n%s", k, reads, now)
						}
						faults.arm(0)
						if err := write(h); err != nil {
							t.Fatalf("read %d/%d retry poisoned: %v", k, reads, err)
						}
					}
					if now, _ := os.ReadFile(path); !bytes.Equal(now, goodAfter) {
						t.Fatalf("read %d/%d failed, the saved file differs\n%s\nwant\n%s", k, reads, now, goodAfter)
					}
				}
			})
		}
	}
}

func TestAuthoredNullsSurviveAMigration(t *testing.T) {
	seed := "_config_version: " + strconv.Itoa(latestConfigVersion()-1) + "\nprompt_caching:\n  cache_ttl: null\nruntime:\n  nofile_soft_limit: null\nx_null_preservation:\n  keep: value\n  nested:\n    optional: null\n"
	h, path := facadeHome(t, seed)
	captureFacade(t)
	if _, err := MigrateConfig(context.Background(), h, false, true); err != nil {
		t.Fatal(err)
	}
	raw, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	rt := Tree(raw)
	for _, key := range []string{"runtime.nofile_soft_limit", "prompt_caching.cache_ttl", "x_null_preservation.nested.optional"} {
		if v, ok := rt.Lookup(key); !ok || v != nil {
			t.Fatalf("%s null lost: %v", key, raw)
		}
	}
	if rt.String("x_null_preservation.keep", "") != "value" || !pyEq(raw["_config_version"], latestConfigVersion()) {
		t.Fatalf("migrated %v", raw)
	}
	for _, section := range []string{"terminal", "agent"} {
		if _, has := raw[section]; has {
			text, _ := os.ReadFile(path)
			t.Fatalf("a %s section the user never wrote\n%s", section, strings.TrimSpace(string(text)))
		}
	}
}

// Same map value, the identity check a cached hit passes
func sameMap(a, b any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func TestAnUnchangedFileStaysOneParseWhileFreshAndOnceSettled(t *testing.T) {
	h, path := facadeHome(t, "")
	tick := time.Now().UnixNano()
	now := tick
	prevSig, prevRacy, prevDigest := fileSignature, mtimeIsRacy, readDigestBytes
	fileSignature = func(fi os.FileInfo) [4]int64 {
		real := FileSignature(fi)
		return [4]int64{tick, real[1], real[2], tick}
	}
	mtimeIsRacy = func(mtimeNS int64) bool { return now-mtimeNS < RacyMtimeWindowNS }
	var opened []string
	readDigestBytes = func(name string) ([]byte, error) {
		opened = append(opened, name)
		return prevDigest(name)
	}
	t.Cleanup(func() { fileSignature, mtimeIsRacy, readDigestBytes = prevSig, prevRacy, prevDigest })
	if err := os.WriteFile(path, []byte(`{"model": {"provider": "custom", "base_url": "http://127.0.0.1:3/v1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	readers := []func() any{
		func() any { cfg, _ := LoadReadonly(context.Background(), h, facadeLookup); return map[string]any(cfg) },
		func() any { doc, _ := LoadYAMLFileReadonly(path); return doc },
	}
	for _, read := range readers {
		read()
	}
	opened = nil
	for i, read := range readers {
		if !sameMap(read(), read()) {
			t.Fatalf("reader %d missed its cache inside the racy window", i)
		}
	}
	if !slices.Contains(opened, path) {
		t.Fatalf("a fresh file was keyed without its digest %v", opened)
	}
	now += RacyMtimeWindowNS + 1
	for _, read := range readers {
		read()
	}
	opened = nil
	for i, read := range readers {
		if !sameMap(read(), read()) {
			t.Fatalf("reader %d missed its settled cache", i)
		}
	}
	if slices.Contains(opened, path) {
		t.Fatalf("a settled hit opened the file %v", opened)
	}
}
