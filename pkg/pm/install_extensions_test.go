package pm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Sorted relative paths under a tree
func ieTreeListing(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && path != root {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestInstallMissingBundleToolIsInstalledInWritableStore(t *testing.T) {
	e := newIEEnv(t)
	ctx := context.Background()
	shipped := filepath.Join(e.tmp, "payload", "tools")
	writeFile(t, filepath.Join(e.tmp, "payload", "manifest.json"), []byte(`{"repo":"core","store":"tools","venv":"venv"}`), 0o644)
	os.MkdirAll(shipped, 0o777)
	t.Setenv(EnvRuntimeDir, shipped)
	if sealed, _ := Sealed(ctx); !sealed {
		t.Fatal("the payload layout is not sealed")
	}
	before := ieTreeListing(t, shipped)
	runner, err := ensure(ctx, "faketool", EnsureOptions{Explicit: true, BaseEnv: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !ieInstalled(t, "faketool") || strings.Join(ieTreeListing(t, shipped), ",") != strings.Join(before, ",") {
		t.Fatal("the install touched the shipped store")
	}
	writable, _ := WritableStoreRoot(ctx)
	facts, _ := NewFacts(filepath.Join(writable, "facts.json"), false)
	fact, ok := facts.Get("faketool")
	if !ok || !isRegularFile(filepath.Join(writable, pyrepr.Str(fact["entry"]), "bin", "faketool")) || !strings.Contains(runner.Env["PATH"], writable) {
		t.Fatalf("writable store fact %v", fact)
	}
	if env := EnvFor([]string{"faketool"}, map[string]string{}); env["PATH"] != runner.Env["PATH"] {
		t.Fatal("env_for disagrees with the runner")
	}
}

func TestInstallRegisteredBionicStagePreservesHostFacts(t *testing.T) {
	var embedded map[string]any
	json.Unmarshal(embeddedLock, &embedded)
	for _, c := range []struct {
		name, main string
		onPath     bool
	}{{"python", "", true}, {"uv", "bin/uv", false}, {"node", "bin/node", true}} {
		t.Run(c.name, func(t *testing.T) {
			tmp := isolateHome(t)
			ctx := context.Background()
			target := "linux-arm64-bionic"
			t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "runtime"))
			lockPath := filepath.Join(tmp, "lock.json")
			ieEmptyLock(t, lockPath)
			previous := lockfilePath
			lockfilePath = func() string { return lockPath }
			defer func() { lockfilePath = previous }()
			factsPath, _ := FactsPath(ctx)
			writeFile(t, factsPath, []byte(`{"sentinel": "host state must not change"}`), 0o644)
			before := readFile(t, factsPath)
			main := c.main
			if main == "" {
				row := embedded["packages"].(map[string]any)["python"].(map[string]any)["artifacts"].(map[string]any)[target].(map[string]any)
				url := pyrepr.Str(row["url"])
				version := strings.Split(url[strings.LastIndex(url, "/")+1:], "_")[1]
				parts := strings.Split(version, ".")
				main = "bin/python" + parts[0] + "." + parts[1]
			}
			relative := "data/data/com.termux/files/usr/" + main
			p, _ := GetPackage(c.name)
			storeRoot, _ := StoreRoot(ctx)
			store := NewStore(storeRoot)
			archive := func(files map[string][]byte) {
				deb := filepath.Join(tmp, "fixture.deb")
				var headers []*tar.Header
				for _, name := range sortedKeys(files) {
					headers = append(headers, &tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeReg})
				}
				buildDeb(t, deb, []member{{Name: "Package", Data: []byte(c.name)}, {Name: "Version", Data: []byte("1.0")}}, headers, files, "gz")
				payload := readFile(t, deb)
				lock := NewLockfile(lockPath)
				lock.SetPin(c.name, "1.0", map[string]any{target: map[string]any{"url": "https://example.test/fixture.deb", "sha256": hexSHA256(payload)}})
				lock.Save()
				ieSeedFetch(t, store, "fixture.deb", payload)
			}
			ieRefuseExec(t)
			archive(map[string][]byte{relative: []byte("bionic-payload")})
			entry, err := stageOnly(ctx, c.name, target, StageOnlyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(entry, filepath.FromSlash(relative))
			if string(readFile(t, binary)) != "bionic-payload" || p.Binary(entry, target) != binary {
				t.Fatal("staged binary")
			}
			path, has := p.Env(entry, target)["PATH"]
			dirs, _ := envList(path)
			if has != c.onPath || (has && (len(dirs) != 1 || dirs[0] != filepath.Dir(binary))) {
				t.Fatalf("PATH %v", path)
			}
			if again, err := stageOnly(ctx, c.name, target, StageOnlyOptions{}); err != nil || again != entry {
				t.Fatalf("restage %v", err)
			}
			archive(map[string][]byte{"unrelated": []byte("not the main executable")})
			_, err = stageOnly(ctx, c.name, target, StageOnlyOptions{})
			ieWantInstallError(t, err, "missing")
			if string(readFile(t, binary)) != "bionic-payload" || !bytes.Equal(readFile(t, factsPath), before) {
				t.Fatal("a failed restage changed the entry or host facts")
			}
		})
	}
}

// Stored zip of one member
func ieOneFileZip(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	w.Write(data)
	zw.Close()
	return buf.Bytes()
}

// Writes a raw response on a hijacked connection and closes it
func ieHijack(w http.ResponseWriter, raw string) {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	buf.WriteString(raw)
	buf.Flush()
	conn.Close()
}

func TestInstallRecoversFromTransientHTTPFailure(t *testing.T) {
	cases := [][2]string{{"probe", "install"}, {"probe-disconnect", "install"}, {"probe-empty-body", "install"}, {"ranged", "install"},
		{"single", "install"}, {"interrupted", "install"}, {"single-interrupted", "install"}, {"ranged", "stage"}}
	for _, c := range cases {
		phase, route := c[0], c[1]
		t.Run(phase+"-"+route, func(t *testing.T) {
			tmp := isolateHome(t)
			ctx := context.Background()
			body := ieOneFileZip(t, "tool.txt", []byte("verified tool bytes"))
			server := newRangeServer(t)
			server.set("/tool.zip", body)
			server.noRange = strings.HasPrefix(phase, "single")
			etag := "ETag: \"" + hexSHA256(body) + "\"\r\n"
			var mu sync.Mutex
			var requests []string
			failed := false
			server.intercept = func(w http.ResponseWriter, r *http.Request) bool {
				rangeHeader := r.Header.Get("Range")
				mu.Lock()
				requests = append(requests, rangeHeader)
				fail := !failed && (rangeHeader == "bytes=0-0") == strings.HasPrefix(phase, "probe")
				if fail {
					failed = true
				}
				mu.Unlock()
				if !fail {
					return false
				}
				switch {
				case phase == "probe-disconnect":
					ieHijack(w, "")
				case phase == "probe-empty-body":
					ieHijack(w, fmt.Sprintf("HTTP/1.1 206 Partial Content\r\nContent-Range: bytes 0-0/%d\r\nContent-Length: 1\r\n%s\r\n", len(body), etag))
				case strings.HasSuffix(phase, "interrupted"):
					status := "206 Partial Content\r\nContent-Range: bytes 0-" + fmt.Sprint(len(body)-1) + "/" + fmt.Sprint(len(body))
					if server.noRange {
						status = "200 OK"
					}
					ieHijack(w, fmt.Sprintf("HTTP/1.1 %s\r\nContent-Length: %d\r\n%s\r\n%s", status, len(body), etag, body[:len(body)/2]))
				default:
					http.Error(w, "boom", 500)
				}
				return true
			}
			p := &BasePackage{Name: "retry-tool"}
			ieSwapRegistry(t, p)
			store := NewStore(filepath.Join(tmp, "store"))
			facts, _ := NewFacts(filepath.Join(store.Root, "facts.json"), false)
			lockPath := filepath.Join(tmp, "lock.json")
			ieEmptyLock(t, lockPath)
			lock := NewLockfile(lockPath)
			lock.SetPin("retry-tool", "1.0", map[string]any{"any": map[string]any{"url": server.url("/tool.zip"), "sha256": hexSHA256(body)}})
			target, _ := CurrentTarget()
			var entry string
			if route == "install" {
				if _, err := InstallPackage(ctx, p, lock, facts, store, target, InstallPackageOptions{}); err != nil {
					t.Fatal(err)
				}
				fact, _ := facts.Get("retry-tool")
				entry = store.Entry(pyrepr.Str(fact["entry"]))
			} else {
				lock.Save()
				previous := lockfilePath
				lockfilePath = func() string { return lockPath }
				defer func() { lockfilePath = previous }()
				t.Setenv(EnvRuntimeDir, store.Root)
				var err error
				if entry, err = stageOnly(ctx, "retry-tool", target, StageOnlyOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, ok := facts.Get("retry-tool"); ok {
					t.Fatal("a stage recorded facts")
				}
			}
			if string(readFile(t, filepath.Join(entry, "tool.txt"))) != "verified tool bytes" || len(requests) < 2 {
				t.Fatalf("requests %v", requests)
			}
			if phase == "interrupted" && requests[len(requests)-1] != fmt.Sprintf("bytes=%d-%d", len(body)/2, len(body)-1) {
				t.Fatalf("resume requested %v", requests)
			}
			for _, pattern := range []string{"*.part", "*.ranges"} {
				if matches, _ := filepath.Glob(filepath.Join(PartialsRoot(), pattern)); len(matches) > 0 {
					t.Fatalf("partials left %v", matches)
				}
			}
		})
	}
}

func TestInstallColdConsumersRecoverAfterUpstreamRemoval(t *testing.T) {
	for _, mode := range []string{"install", "stage"} {
		t.Run(mode, func(t *testing.T) {
			tmp := isolateHome(t)
			ctx := context.Background()
			body := ieOneFileZip(t, "tool.txt", []byte("pinned and preserved"))
			digest := hexSHA256(body)
			server := newRangeServer(t)
			previousPrefix := MirrorPublicPrefix
			MirrorPublicPrefix = server.url("/archive/")
			defer func() { MirrorPublicPrefix = previousPrefix }()
			server.set("/archive/"+digest, body)
			p := &BasePackage{Name: "mirror-tool"}
			ieSwapRegistry(t, p)
			store := NewStore(filepath.Join(tmp, "store"))
			facts, _ := NewFacts(filepath.Join(store.Root, "facts.json"), false)
			lockPath := filepath.Join(tmp, "lock.json")
			ieEmptyLock(t, lockPath)
			lock := NewLockfile(lockPath)
			lock.SetPin("mirror-tool", "1.0", map[string]any{"any": map[string]any{"url": server.url("/removed.zip"), "sha256": digest}})
			var result string
			if mode == "install" {
				target, _ := CurrentTarget()
				if _, err := InstallPackage(ctx, p, lock, facts, store, target, InstallPackageOptions{}); err != nil {
					t.Fatal(err)
				}
				fact, _ := facts.Get("mirror-tool")
				result = store.Entry(pyrepr.Str(fact["entry"]))
			} else {
				lock.Save()
				previous := lockfilePath
				lockfilePath = func() string { return lockPath }
				defer func() { lockfilePath = previous }()
				t.Setenv(EnvRuntimeDir, store.Root)
				var err error
				if result, err = stageOnly(ctx, "mirror-tool", "linux-arm64-bionic", StageOnlyOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if string(readFile(t, filepath.Join(result, "tool.txt"))) != "pinned and preserved" {
				t.Fatal("mirror bytes were not installed")
			}
			used := false
			for _, seen := range server.seen() {
				used = used || seen.Path == "/archive/"+digest
			}
			if !used {
				t.Fatal("the mirror was never asked")
			}
		})
	}
}
