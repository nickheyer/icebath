package pm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// Stage fixture package whose unpack turns the verified archive bytes into one binary
type ieStagePackage struct{ BasePackage }

func (p *ieStagePackage) MissingReason(string) string { return "" }

func (p *ieStagePackage) Unpack(archive, staged, target string) error {
	data, err := os.ReadFile(archive)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(staged, "bin"), 0o777); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(staged, "bin", "tool"), data, 0o666)
}

func (p *ieStagePackage) Verify(entry, target string) string {
	if isRegularFile(filepath.Join(entry, "bin", "tool")) {
		return ""
	}
	return "bin/tool missing"
}

// Seeds the store's download cache so a fetch proves the digest from local bytes
func ieSeedFetch(t *testing.T, store *Store, name string, payload []byte) {
	t.Helper()
	writeFile(t, filepath.Join(store.Entry("fetch-"+hexSHA256(payload)), name), payload, 0o644)
}

// Points the lockfile at a fresh file pinning node and stage-test to the artifacts
func ieArmLock(t *testing.T, store *Store, artifacts []any) {
	t.Helper()
	path := filepath.Join(filepath.Dir(store.Root), "stage-lock.json")
	previous := lockfilePath
	lockfilePath = func() string { return path }
	t.Cleanup(func() { lockfilePath = previous })
	ieEmptyLock(t, path)
	lock := NewLockfile(path)
	lock.SetPin("node", "1.0", map[string]any{"any": artifacts})
	lock.SetPin("stage-test", "1.0", map[string]any{"any": artifacts})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
}

// Store of an isolated home with the runtime override set
func ieSandbox(t *testing.T) (*Store, string) {
	t.Helper()
	tmp := isolateHome(t)
	runtimeDir := filepath.Join(tmp, "runtime")
	t.Setenv(EnvRuntimeDir, runtimeDir)
	return NewStore(runtimeDir), tmp
}

// Fails a test the moment a catalogue probe would execute a binary
func ieRefuseExec(t *testing.T) {
	t.Helper()
	previous := runProcess
	runProcess = func(spec processSpec) (processResult, error) {
		t.Fatalf("cross-target stage attempted execution: %v", spec.Argv)
		return processResult{}, nil
	}
	t.Cleanup(func() { runProcess = previous })
}

func TestStageOnlyBionicDebStagesRealPackagesWithoutHostExecution(t *testing.T) {
	for _, c := range []struct{ name, relative string }{{"python", "bin/python3.14"}, {"uv", "bin/uv"}, {"node", "bin/node"}} {
		t.Run(c.name, func(t *testing.T) {
			store, tmp := ieSandbox(t)
			ctx := context.Background()
			lockPath := filepath.Join(tmp, "lock.json")
			ieEmptyLock(t, lockPath)
			previous := lockfilePath
			lockfilePath = func() string { return lockPath }
			defer func() { lockfilePath = previous }()
			deb := filepath.Join(tmp, "tool.deb")
			main := "data/data/com.termux/files/usr/" + c.relative
			buildDeb(t, deb, []member{{Name: "Package", Data: []byte(c.name)}},
				[]*tar.Header{{Name: main, Mode: 0o755, Typeflag: tar.TypeReg}, {Name: "data/data/com.termux/files/usr/lib/keep", Mode: 0o644, Typeflag: tar.TypeReg}},
				map[string][]byte{main: []byte("bionic fixture"), "data/data/com.termux/files/usr/lib/keep": []byte("library")}, "gz")
			payload := readFile(t, deb)
			lock := NewLockfile(lockPath)
			lock.SetPin(c.name, "1.0", map[string]any{"linux-arm64-bionic": map[string]any{"url": "https://example.invalid/tool.deb", "sha256": hexSHA256(payload)}})
			lock.Save()
			ieSeedFetch(t, store, "tool.deb", payload)
			factsPath, _ := FactsPath(ctx)
			writeFile(t, factsPath, []byte(`{"schema":1,"packages":{}}`), 0o644)
			before := readFile(t, factsPath)
			ieRefuseExec(t)
			entry, err := stageOnly(ctx, c.name, "linux-arm64-bionic", StageOnlyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			p, _ := GetPackage(c.name)
			if got := p.Binary(entry, "linux-arm64-bionic"); got != filepath.Join(entry, filepath.FromSlash(main)) {
				t.Fatalf("binary %s", got)
			}
			if string(readFile(t, filepath.Join(entry, main))) != "bionic fixture" || !bytes.Equal(readFile(t, factsPath), before) {
				t.Fatal("stage changed host facts or bytes")
			}
			os.Remove(filepath.Join(entry, main))
			if p.Verify(entry, "linux-arm64-bionic") == "" {
				t.Fatal("a missing binary verified")
			}
		})
	}
}

func TestStageOnlyRealNodeForeignStageChecksBytesWithoutExec(t *testing.T) {
	if target, _ := CurrentTarget(); target != "linux-x64" {
		t.Skip("linux x86_64 host")
	}
	store, _ := ieSandbox(t)
	ctx := context.Background()
	elf := make([]byte, 64)
	copy(elf, "\x7fELF\x02\x01\x01")
	elf[18], elf[19] = 0xB7, 0
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	f, _ := zw.CreateHeader(&zip.FileHeader{Name: "node-v1.0-linux-arm64/bin/node", Method: zip.Store})
	f.Write(elf)
	zw.Close()
	data := archive.Bytes()
	ieArmLock(t, store, []any{map[string]any{"url": "https://example.test/node.zip", "sha256": hexSHA256(data)}})
	ieSeedFetch(t, store, "node.zip", data)
	ieRefuseExec(t)
	entry, err := stageOnly(ctx, "node", "linux-arm64", StageOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(entry, "bin/node")
	if !bytes.Equal(readFile(t, node), elf) {
		t.Fatal("node bytes differ")
	}
	if match, known := MachineMatchesBinary(node, "linux-arm64"); !match || !known {
		t.Fatal("staged node is not arm64")
	}
	if again, err := stageOnly(ctx, "node", "linux-arm64", StageOnlyOptions{}); err != nil || again != entry {
		t.Fatalf("restage %s %v", again, err)
	}
	if factsPath, _ := FactsPath(ctx); pathExists(factsPath) {
		t.Fatal("stage created host facts")
	}
	if pathExists(store.Entry("fetch-" + hexSHA256(data))) {
		t.Fatal("stage kept the archive after publishing")
	}
	elf[18] = 0x3E
	os.WriteFile(node, elf, 0o755)
	p, _ := GetPackage("node")
	if !strings.Contains(p.Verify(entry, "linux-arm64"), "not a linux-arm64 binary") {
		t.Fatal("wrong architecture verified")
	}
	os.Remove(node)
	if p.Verify(entry, "linux-arm64") == "" {
		t.Fatal("missing node verified")
	}
}

func TestStageOnlyRealNodeNativeInstallKeepsSmokeValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	for _, compression := range []string{"gz", "xz"} {
		t.Run(compression, func(t *testing.T) {
			store, tmp := ieSandbox(t)
			ctx := context.Background()
			probe := filepath.Join(tmp, "native-probes")
			script := []byte("#!/bin/sh\nprintf \"%s\\n\" \"$1\" >> \"" + probe + "\"\nexit 0\n")
			var raw bytes.Buffer
			tw := tar.NewWriter(&raw)
			tw.WriteHeader(&tar.Header{Name: "node-v1.0/bin/node", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
			tw.Write(script)
			tw.Close()
			var data bytes.Buffer
			if compression == "gz" {
				gz := gzip.NewWriter(&data)
				gz.Write(raw.Bytes())
				gz.Close()
			} else {
				xw, _ := xz.NewWriter(&data)
				xw.Write(raw.Bytes())
				xw.Close()
			}
			ieArmLock(t, store, []any{map[string]any{"url": "https://example.test/node.tar." + compression, "sha256": hexSHA256(data.Bytes())}})
			ieSeedFetch(t, store, "node.tar."+compression, data.Bytes())
			t.Setenv("PATH", "")
			p, _ := GetPackage("node")
			facts, err := storeFacts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			target, _ := CurrentTarget()
			entry, err := InstallPackage(ctx, p, currentLockfile(), facts, store, target, InstallPackageOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Split(strings.TrimSpace(string(readFile(t, probe))), "\n"); len(got) != 2 || got[0] != "--version" || got[1] != "--version" {
				t.Fatalf("probes %v", got)
			}
			if fact, _ := facts.Get("node"); fact["entry"] != filepath.Base(entry) {
				t.Fatalf("fact %v", fact)
			}
			os.WriteFile(filepath.Join(entry, "bin/node"), []byte("#!/bin/sh\nexit 23\n"), 0o755)
			if !strings.Contains(p.Verify(entry, target), "23") {
				t.Fatal("a failing smoke probe verified")
			}
		})
	}
}
