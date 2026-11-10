package pm

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Points the bundle staging at a lockfile pinning the given names
func bnLock(t *testing.T, pins map[string]string) {
	t.Helper()
	lock := NewLockfile(filepath.Join(t.TempDir(), "lock.json"))
	for name, version := range pins {
		lock.SetPin(name, version, map[string]any{"any": map[string]any{"url": "x", "sha256": strings.Repeat("0", 64)}})
	}
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	previous := bundleLockfile
	bundleLockfile = func() *Lockfile { return lock }
	t.Cleanup(func() { bundleLockfile = previous })
}

func TestBundleClosureUVStaysInternalNodeNPMShip(t *testing.T) {
	bnLock(t, map[string]string{"uv": "1", "node": "1", "npm": "1"})
	names, err := bundlePackageNames()
	if err != nil {
		t.Fatal(err)
	}
	for name, internal := range map[string]bool{"uv": true, "node": false, "npm": false} {
		p, err := GetPackage(name)
		if err != nil {
			t.Fatal(err)
		}
		if p.Base().Internal != internal {
			t.Fatalf("%s internal %v", name, p.Base().Internal)
		}
		if !slices.Contains(names, name) {
			t.Fatalf("%s missing from %v", name, names)
		}
	}
	if !slices.Contains(names, "python") {
		t.Fatalf("python missing from %v", names)
	}
}

func TestArchGuardAllowsEmulatedX64OnWin32ARM64(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	entry := filepath.Join(store, "chromium-1234")
	if err := os.MkdirAll(filepath.Join(entry, "chrome-win64"), 0o755); err != nil {
		t.Fatal(err)
	}
	pe := append([]byte("MZ"), make([]byte, 58)...)
	pe = binary.LittleEndian.AppendUint32(pe, 0x80)
	pe = append(pe, make([]byte, 64)...)
	pe = append(pe, []byte("PE\x00\x00")...)
	pe = binary.LittleEndian.AppendUint16(pe, 0x8664)
	pe = append(pe, make([]byte, 54)...)
	chrome := filepath.Join(entry, "chrome-win64", "chrome.exe")
	if err := os.WriteFile(chrome, pe, 0o755); err != nil {
		t.Fatal(err)
	}
	if match, known := MachineMatchesBinary(chrome, "win32-arm64"); !known || match {
		t.Fatalf("x64 PE matched win32-arm64: match %v known %v", match, known)
	}
	bnLock(t, map[string]string{"chromium": "1234+145.0.0.0"})
	previous := bundleTarget
	bundleTarget = func() (string, error) { return "win32-arm64", nil }
	t.Cleanup(func() { bundleTarget = previous })
	facts, err := NewFacts(filepath.Join(store, "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record("chromium", "1234+145.0.0.0", filepath.Base(entry), EnvDiff{}, store, FactRecord{}); err != nil {
		t.Fatal(err)
	}
	problems, err := archGuard(store)
	if err != nil || len(problems) != 0 {
		t.Fatalf("problems %v err %v", problems, err)
	}
	p, _ := GetPackage("chromium")
	if bin := p.Binary(entry, "win32-arm64"); bin != chrome {
		t.Fatalf("guard probed %q, not the staged chrome.exe", bin)
	}
}

const bnHelperEnv = "ICEBATH_TEST_BN_STAGE"

// Staging child behaviour, active only when re-executed by stageNative under the test seam
func TestBNStageNativeHelper(t *testing.T) {
	if os.Getenv(bnHelperEnv) == "" {
		return
	}
	os.Exit(bnStageChild())
}

// Records the child environment, checks the compiler homes and bumps a reuse counter in the cache
func bnStageChild() int {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	encoded, _ := json.Marshal(env)
	if err := os.WriteFile(os.Getenv(bnHelperEnv+"_OBSERVED"), encoded, 0o644); err != nil {
		return 90
	}
	for _, key := range []string{"CARGO_HOME", "RUSTUP_HOME"} {
		if data, err := os.ReadFile(filepath.Join(env[key], "fixture-state")); err != nil || string(data) != key {
			return 91
		}
	}
	cache := env["UV_CACHE_DIR"]
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return 92
	}
	previous, _ := os.ReadFile(filepath.Join(cache, "reused"))
	if err := os.WriteFile(filepath.Join(cache, "reused"), append(previous, 'x'), 0o644); err != nil {
		return 93
	}
	code, _ := strconv.Atoi(os.Getenv(bnHelperEnv + "_STATUS"))
	return code
}

func TestBNNativeDispatchChildEnvironment(t *testing.T) {
	cases := []struct {
		cacheSource       string
		explicitCompilers bool
		status            int
	}{{"explicit", true, 17}, {"ambient", false, 0}, {"default", false, 17}}
	for _, tc := range cases {
		t.Run(tc.cacheSource, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", filepath.Join(tmp, "host"))
			t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "user"))
			t.Setenv("UV_CACHE_DIR", "")
			os.Unsetenv("UV_CACHE_DIR")
			ambient := filepath.Join(tmp, "ambient")
			if tc.cacheSource != "default" {
				t.Setenv("UV_CACHE_DIR", ambient)
			}
			cache := map[string]string{"explicit": filepath.Join(tmp, "explicit"), "ambient": ambient}[tc.cacheSource]
			if tc.cacheSource == "default" {
				dir, err := UvCacheDir(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				cache = dir
			}
			compilers := map[string]string{}
			for _, pair := range [][2]string{{"CARGO_HOME", ".cargo"}, {"RUSTUP_HOME", ".rustup"}} {
				parent := "host"
				if tc.explicitCompilers {
					parent = "custom"
				}
				home := filepath.Join(tmp, parent, pair[1])
				if err := os.MkdirAll(home, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, "fixture-state"), []byte(pair[0]), 0o644); err != nil {
					t.Fatal(err)
				}
				compilers[pair[0]] = home
				t.Setenv(pair[0], "")
				os.Unsetenv(pair[0])
				if tc.explicitCompilers {
					t.Setenv(pair[0], home)
				}
			}
			observed := filepath.Join(tmp, "child.json")
			t.Setenv(bnHelperEnv, "1")
			t.Setenv(bnHelperEnv+"_OBSERVED", observed)
			t.Setenv(bnHelperEnv+"_STATUS", strconv.Itoa(tc.status))
			previous := stageNativeArgv
			stageNativeArgv = func() ([]string, error) {
				return []string{os.Args[0], "-test.run=^TestBNStageNativeHelper$", "-test.count=1", "--"}, nil
			}
			t.Cleanup(func() { stageNativeArgv = previous })
			before := os.Environ()
			out := filepath.Join(tmp, "payload")
			var homes []string
			for range 2 {
				args := BundleArgs{Out: out, Ref: "HEAD"}
				if tc.cacheSource == "explicit" {
					args.Cache = cache
				}
				code, err := stageNative(context.Background(), args)
				if err != nil || code != tc.status {
					t.Fatalf("status %d err %v", code, err)
				}
				data, err := os.ReadFile(observed)
				if err != nil {
					t.Fatal(err)
				}
				var env map[string]string
				if err := json.Unmarshal(data, &env); err != nil {
					t.Fatal(err)
				}
				homes = append(homes, env["HOME"])
				if env["HOME"] != env["USERPROFILE"] || env["HOME"] == filepath.Join(tmp, "host") {
					t.Fatalf("HOME %q USERPROFILE %q", env["HOME"], env["USERPROFILE"])
				}
				for key, value := range compilers {
					if env[key] != value {
						t.Fatalf("%s %q want %q", key, env[key], value)
					}
				}
				if env["UV_CACHE_DIR"] != resolvePath(cache) || env[EnvRuntimeDir] != filepath.Join(resolvePath(out), "tools") ||
					env["ICEBATH_HOME"] != filepath.Join(homes[len(homes)-1], ".icebath") {
					t.Fatalf("cache %q runtime %q home %q", env["UV_CACHE_DIR"], env[EnvRuntimeDir], env["ICEBATH_HOME"])
				}
			}
			if data, _ := os.ReadFile(filepath.Join(cache, "reused")); string(data) != "xx" {
				t.Fatalf("reused %q", data)
			}
			for _, home := range homes {
				if _, err := os.Stat(home); !os.IsNotExist(err) {
					t.Fatalf("isolated home %s survived", home)
				}
			}
			if after := os.Environ(); !slices.Equal(before, after) {
				t.Fatal("parent environment changed")
			}
		})
	}
}

func TestBNStagedCacheSkipsBuildInputsBeforeCopying(t *testing.T) {
	for _, tc := range [][2]string{{"revision.http", ""}, {"revision.rev", "build-settings"}} {
		pointer, shard := tc[0], tc[1]
		t.Run(pointer, func(t *testing.T) {
			tmp := t.TempDir()
			cache := filepath.Join(tmp, "cache")
			revision := filepath.Join("sdists-v9", "index", "package", "revision")
			wheels := filepath.Join(revision, shard)
			waste := map[string]string{filepath.Join(revision, "src", "target", "release", "build.exe"): "build output"}
			kept := map[string]string{
				filepath.Join(wheels, "metadata.msgpack"):                                    "wheel metadata",
				filepath.Join(wheels, "cache_proof-1.0-py3-none-any.whl"):                    "built wheel ZIP ships for offline rebuilds",
				filepath.Join(filepath.Dir(revision), pointer):                               "revision pointer",
				filepath.Join(wheels, "cache_proof-1.0-py3-none-any", "src", "template.whl"): "package data",
				filepath.Join("archive-v0", "entry", "cache_proof", "src", "__init__.py"):    "archive data",
				filepath.Join("other-bucket", "src", "keep.whl"):                             "unrelated data",
			}
			for _, files := range []map[string]string{waste, kept} {
				for relative, data := range files {
					path := filepath.Join(cache, relative)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			shipped := filepath.Join(tmp, "payload", "uv-cache")
			if err := stageUVCache(cache, shipped); err != nil {
				t.Fatal(err)
			}
			for relative := range waste {
				if _, err := os.Lstat(filepath.Join(shipped, relative)); !os.IsNotExist(err) {
					t.Fatalf("build input %s shipped", relative)
				}
			}
			for relative, data := range kept {
				if got, err := os.ReadFile(filepath.Join(shipped, relative)); err != nil || string(got) != data {
					t.Fatalf("%s shipped %q %v", relative, got, err)
				}
			}
			for _, files := range []map[string]string{waste, kept} {
				for relative, data := range files {
					if got, _ := os.ReadFile(filepath.Join(cache, relative)); string(got) != data {
						t.Fatalf("source %s changed", relative)
					}
				}
			}
		})
	}
}
