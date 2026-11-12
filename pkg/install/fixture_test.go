package install

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var fixtureBinaries sync.Map

// Tiny Go program built with -X main.version=tag so buildinfo names its release
func fixtureBinary(t *testing.T, tag string) []byte {
	t.Helper()
	if cached, ok := fixtureBinaries.Load(tag); ok {
		return cached.([]byte)
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package main

import (
	"fmt"
	"os"
	"strings"
)

var version string

func main() {
	if strings.Join(os.Args[1:], " ") == "install-self --release-info" {
		fmt.Printf("{\"tag\":\"%s\",\"commit\":\"\"}\n", version)
		return
	}
	fmt.Println("icebath fixture " + version + " " + fmt.Sprint(os.Args[1:]))
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "icebath")
	cmd := exec.Command(gobin, "build", "-ldflags", "-X main.version="+tag, "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-trimpath", "CGO_ENABLED=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, msg)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	fixtureBinaries.Store(tag, data)
	return data
}

// Gzipped tar holding one executable desktop file at name
func desktopArchive(t *testing.T, name string) []byte {
	t.Helper()
	var buf strings.Builder
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\nexit 0\n")
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	gz.Close()
	return []byte(buf.String())
}

type fixtureRelease struct {
	Tag, Target string
	Assets      map[string][]byte
	Published   string
}

// Loopback release host serving the API, the latest redirect, smart HTTP refs and asset downloads of one repo
type releaseFixture struct {
	srv      *httptest.Server
	repo     string
	releases []fixtureRelease
	refs     map[string]string
	noAPI    bool
	mu       sync.Mutex
	hits     []string
}

func newReleaseFixture(t *testing.T, releases []fixtureRelease, refs map[string]string) *releaseFixture {
	t.Helper()
	f := &releaseFixture{releases: releases, refs: refs}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.repo = f.srv.URL + "/o/r.git"
	return f
}

func (f *releaseFixture) checksums(rel fixtureRelease) []byte {
	var b strings.Builder
	for name, data := range rel.Assets {
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	return []byte(b.String())
}

func (f *releaseFixture) apiJSON(rel fixtureRelease) map[string]any {
	assets := []map[string]any{}
	for name := range rel.Assets {
		assets = append(assets, map[string]any{"name": name, "browser_download_url": f.srv.URL + "/o/r/releases/download/" + rel.Tag + "/" + name})
	}
	assets = append(assets, map[string]any{"name": "checksums.txt", "browser_download_url": f.srv.URL + "/o/r/releases/download/" + rel.Tag + "/checksums.txt"})
	return map[string]any{"tag_name": rel.Tag, "target_commitish": rel.Target, "draft": false, "prerelease": false, "published_at": rel.Published, "assets": assets}
}

func (f *releaseFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/api/v3/repos/o/r/"):
		if f.noAPI {
			http.Error(w, "rate limited", http.StatusForbidden)
			return
		}
		rest := strings.TrimPrefix(path, "/api/v3/repos/o/r/")
		switch {
		case rest == "releases":
			out := []any{}
			if r.URL.Query().Get("page") == "1" {
				for _, rel := range f.releases {
					out = append(out, f.apiJSON(rel))
				}
			}
			json.NewEncoder(w).Encode(out)
		case rest == "releases/latest" && len(f.releases) > 0:
			json.NewEncoder(w).Encode(f.apiJSON(f.releases[0]))
		case strings.HasPrefix(rest, "releases/tags/"):
			for _, rel := range f.releases {
				if rel.Tag == strings.TrimPrefix(rest, "releases/tags/") {
					json.NewEncoder(w).Encode(f.apiJSON(rel))
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	case path == "/o/r/releases/latest" && len(f.releases) > 0:
		http.Redirect(w, r, f.srv.URL+"/o/r/releases/tag/"+f.releases[0].Tag, http.StatusFound)
	case path == "/o/r.git/info/refs":
		if f.refs == nil {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(pktLine("# service=git-upload-pack\n") + "0000"))
		first := true
		for ref, sha := range f.refs {
			line := sha + " " + ref
			if first {
				line += "\x00multi_ack"
				first = false
			}
			w.Write([]byte(pktLine(line + "\n")))
		}
		w.Write([]byte("0000"))
	case strings.HasPrefix(path, "/o/r/releases/download/"):
		parts := strings.SplitN(strings.TrimPrefix(path, "/o/r/releases/download/"), "/", 2)
		for _, rel := range f.releases {
			if len(parts) == 2 && rel.Tag == parts[0] {
				if parts[1] == "checksums.txt" {
					w.Write(f.checksums(rel))
					return
				}
				if data, ok := rel.Assets[parts[1]]; ok {
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					w.Write(data)
					return
				}
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// Hermetic home, user home and repo pointing at the fixture
func isolatedInstall(t *testing.T, repo string) (homeDir, userHome string) {
	t.Helper()
	base := t.TempDir()
	homeDir, userHome = filepath.Join(base, "icebath-home"), filepath.Join(base, "user")
	if err := os.MkdirAll(userHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("ICEBATH_REPO_URL", repo)
	t.Setenv("ICEBATH_INSTALL_CHANNEL", "")
	t.Setenv("ICEBATH_INSTALL_DIR", "")
	t.Setenv("CI", "true")
	return homeDir, userHome
}
