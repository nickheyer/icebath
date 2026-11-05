package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Package whose env names its own entry, the recorded env refresh fixture
type ieEnvTool struct{ BasePackage }

func (p *ieEnvTool) Env(entry, target string) EnvDiff { return EnvDiff{"TOOL_ROOT": entry} }

func TestBuildAdmissionPrepareRerecordsAnOutdatedEnvWithoutRefetching(t *testing.T) {
	tmp := isolateHome(t)
	ctx := context.Background()
	target, _ := CurrentTarget()
	store := filepath.Join(tmp, "tools")
	p := &ieEnvTool{BasePackage{Name: "env-tool"}}
	ieSwapRegistry(t, p)
	lockPath := filepath.Join(tmp, "lock.json")
	ieEmptyLock(t, lockPath)
	previous := lockfilePath
	lockfilePath = func() string { return lockPath }
	defer func() { lockfilePath = previous }()
	lock := NewLockfile(lockPath)
	lock.SetPin("env-tool", "1.0", map[string]any{target: map[string]any{"url": "https://invalid.test/tool.tgz", "sha256": strings.Repeat("a", 64)}})
	lock.Save()
	entry := filepath.Join(store, p.StoreEntry("1.0", target))
	writeFile(t, filepath.Join(entry, "data"), []byte("tool"), 0o644)
	digest, _ := TreeDigest(entry)
	facts, _ := NewFacts(filepath.Join(store, "facts.json"), false)
	stale := EnvDiff{"TOOL_ROOT": entry, "RETIRED_EXECUTABLE_PATH": filepath.Join(entry, "data")}
	if err := facts.Record("env-tool", "1.0", filepath.Base(entry), stale, store, FactRecord{Target: &target, Artifacts: []string{strings.Repeat("a", 64)}, Digest: &digest}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareTools(ctx, PrepareToolsOptions{Names: []string{"env-tool"}, Out: store, Target: target}); err != nil {
		t.Fatal(err)
	}
	facts, _ = NewFacts(filepath.Join(store, "facts.json"), false)
	if !envDiffEqual(facts.EnvFor("env-tool", store), p.Env(entry, target)) {
		t.Fatalf("env not re-recorded %v", facts.EnvFor("env-tool", store))
	}
	selected, err := ReadVerifiedTools([]string{"env-tool"}, store, target, NewLockfile(lockPath))
	if err != nil || selected.Entries["env-tool"].Path != entry || string(readFile(t, filepath.Join(entry, "data"))) != "tool" {
		t.Fatalf("admission %v %v", selected, err)
	}
}

func TestBuildAdmissionFactsRecordedWithMetadataInTheDigestStillVerify(t *testing.T) {
	tmp := isolateHome(t)
	target, _ := CurrentTarget()
	store := filepath.Join(tmp, "tools")
	lockPath := filepath.Join(tmp, "lock.json")
	ieEmptyLock(t, lockPath)
	lock := NewLockfile(lockPath)
	lock.SetPin("python", "1.0", map[string]any{target: map[string]any{"url": "https://invalid.test/python", "sha256": strings.Repeat("a", 64)}})
	p, _ := GetPackage("python")
	entry := filepath.Join(store, p.StoreEntry("1.0", target))
	binary := p.Binary(entry, target)
	writeFile(t, binary, []byte("runtime"), 0o755)
	writeFile(t, filepath.Join(entry, ".DS_Store"), []byte("desktop metadata"), 0o644)
	var files []string
	filepath.Walk(entry, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			rel, _ := filepath.Rel(entry, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	legacy := sha256.New()
	for _, rel := range files {
		legacy.Write(append([]byte(rel+"\x00"), readFile(t, filepath.Join(entry, rel))...))
	}
	digest := hex.EncodeToString(legacy.Sum(nil))
	env, err := PackageEnv(p, entry, target)
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := NewFacts(filepath.Join(store, "facts.json"), false)
	facts.Record("python", "1.0", filepath.Base(entry), env, store, FactRecord{Target: &target, Artifacts: []string{strings.Repeat("a", 64)}, Digest: &digest})
	selected, err := ReadVerifiedTools([]string{"python"}, store, target, lock)
	if err != nil || selected.Entries["python"].Binary != binary {
		t.Fatalf("legacy digest admission %v %v", selected, err)
	}
	os.WriteFile(binary, []byte("changed runtime"), 0o755)
	_, err = ReadVerifiedTools([]string{"python"}, store, target, lock)
	ieWantInstallError(t, err, "source failed verification")
}
