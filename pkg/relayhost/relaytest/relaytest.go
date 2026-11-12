package relaytest

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/relayhost"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	provisionOnce sync.Once
	projectRoot   string
	provisionErr  error
	uvBinary      string
	pythonBinary  string
	indexFiles    map[string][]indexFile
)

// One artifact a loopback index lists for a package
type indexFile struct {
	name, sha string
	data      []byte
}

// Builds once before the network block a cached venv holding nemo_relay and fetches the index wheels
func Provision() {
	provisionOnce.Do(func() {
		if projectRoot, provisionErr = provision(); provisionErr == nil {
			provisionErr = provisionIndex()
		}
	})
}

// Pinned uv and interpreter the provisioning toolchain runs
func Toolchain(t testing.TB) (uv, python string) {
	t.Helper()
	Provision()
	if provisionErr != nil {
		t.Fatalf("nemo_relay test environment: %v", provisionErr)
	}
	return uvBinary, pythonBinary
}

// Loopback PEP 503 index serving the runtime set wheels of this host, its simple URL
func ServeIndex(t testing.TB) string {
	t.Helper()
	Provision()
	if provisionErr != nil {
		t.Fatalf("nemo_relay test environment: %v", provisionErr)
	}
	files := map[string][]byte{}
	mux := http.NewServeMux()
	for name, list := range indexFiles {
		var page strings.Builder
		page.WriteString("<!DOCTYPE html><html><body>\n")
		for _, f := range list {
			files[f.name] = f.data
			fmt.Fprintf(&page, "<a href=\"/files/%s#sha256=%s\">%s</a>\n", f.name, f.sha, f.name)
		}
		page.WriteString("</body></html>\n")
		body := page.String()
		mux.HandleFunc("/simple/"+name+"/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, body)
		})
	}
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[strings.TrimPrefix(r.URL.Path, "/files/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL + "/simple"
}

// Platform tokens a wheel filename carries for each target
var targetTokens = map[string][]string{
	"linux-x64": {"manylinux", "x86_64"}, "linux-arm64": {"manylinux", "aarch64"}, "linux-x64-musl": {"musllinux", "x86_64"},
	"linux-arm64-musl": {"musllinux", "aarch64"}, "darwin-arm64": {"macosx", "arm64"}, "darwin-x64": {"macosx", "x86_64"},
	"win32-x64": {"win_amd64"}, "win32-arm64": {"win_arm64"},
}

// Wheel of each runtime set package this host installs, downloaded into the shared cache
func provisionIndex() error {
	target, err := pm.CurrentTarget()
	if err != nil {
		return err
	}
	cache, err := cacheRoot()
	if err != nil {
		return err
	}
	reqs, err := pm.RuntimeRequirements()
	if err != nil {
		return err
	}
	indexFiles = map[string][]indexFile{}
	for _, r := range reqs {
		var chosen *pm.Wheel
		for i, f := range r.Files {
			base := f.URL[strings.LastIndex(f.URL, "/")+1:]
			if !strings.HasSuffix(base, ".whl") {
				continue
			}
			if strings.Contains(base, "-none-any.whl") || (wheelForPython(base) && hasAll(base, targetTokens[target])) {
				chosen = &r.Files[i]
				break
			}
		}
		if chosen == nil {
			return fmt.Errorf("no %s wheel of %s fits %s", r.Name, r.Version, target)
		}
		data, err := fetch(filepath.Join(cache, "wheels"), chosen.URL, chosen.SHA256)
		if err != nil {
			return err
		}
		base := chosen.URL[strings.LastIndex(chosen.URL, "/")+1:]
		indexFiles[r.Name] = append(indexFiles[r.Name], indexFile{name: base, sha: chosen.SHA256, data: data})
	}
	pythonBinary, err = pinnedPython(cache, target)
	if err != nil {
		return err
	}
	uvBinary, err = pinnedTool(cache, target, "uv")
	return err
}

func wheelForPython(base string) bool {
	return strings.Contains(base, "-cp314-cp314-") || strings.Contains(base, "-abi3-")
}

func hasAll(s string, tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, tok := range tokens {
		if !strings.Contains(s, tok) {
			return false
		}
	}
	return !(strings.Contains(s, "musllinux") && tokens[0] == "manylinux")
}

// Registers the real binding over the provisioned environment, failing when provisioning failed
func Use(t testing.TB) {
	t.Helper()
	Provision()
	if provisionErr != nil {
		t.Fatalf("nemo_relay test environment: %v", provisionErr)
	}
	python := pm.VenvPython(filepath.Join(projectRoot, "venv"))
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) { return relayhost.LoadFrom(python) })
	t.Cleanup(func() { tools.RegisterNemoRelayBinding(nil) })
}

func cacheRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "icebath-pm-tests"), nil
}

func provision() (string, error) {
	target, err := pm.CurrentTarget()
	if err != nil {
		return "", err
	}
	wheel, ok := pm.NemoRelayWheel(target)
	if !ok {
		return "", fmt.Errorf("no nemo_relay wheel is pinned for %s", target)
	}
	cache, err := cacheRoot()
	if err != nil {
		return "", err
	}
	python, err := pinnedPython(cache, target)
	if err != nil {
		return "", err
	}
	root := filepath.Join(cache, "relay-project-"+wheel.SHA256[:16])
	venv := filepath.Join(root, "venv")
	if marker := filepath.Join(root, ".complete"); fileExists(marker) {
		return root, nil
	}
	data, err := fetch(filepath.Join(cache, "wheels"), wheel.URL, wheel.SHA256)
	if err != nil {
		return "", err
	}
	os.RemoveAll(root)
	cmd := exec.Command(python, "-I", "-m", "venv", "--without-pip", venv)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("venv: %v\n%s", err, out)
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		return "", err
	}
	if err := unzip(data, site); err != nil {
		return "", err
	}
	return root, os.WriteFile(filepath.Join(root, ".complete"), nil, 0o644)
}

// Lockfile pinned interpreter of the target, unpacked once into the shared cache the pyhost suite uses
func pinnedPython(cache, target string) (string, error) {
	return pinnedTool(cache, target, "python")
}

// Lockfile pinned tool of the target, unpacked once into the shared cache the pm suites use
func pinnedTool(cache, target, pkg string) (string, error) {
	artifacts := pm.NewLockfile(pm.LockfilePath()).Artifacts(pkg, target)
	if len(artifacts) == 0 {
		return "", &fs.PathError{Op: "pin", Path: pkg + " " + target, Err: fs.ErrNotExist}
	}
	url, sha := pyrepr.Str(artifacts[0]["url"]), pyrepr.Str(artifacts[0]["sha256"])
	root := filepath.Join(cache, pkg+"-"+sha[:16])
	name := map[string]string{"python": "python3", "uv": "uv"}[pkg]
	if strings.HasPrefix(target, "win32") {
		name += ".exe"
		if pkg == "python" {
			name = "python.exe"
		}
	}
	if found := findFile(root, name); found != "" {
		return found, nil
	}
	data, err := fetch(filepath.Join(cache, "downloads"), url, sha)
	if err != nil {
		return "", err
	}
	staging := root + ".partial"
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o777); err != nil {
		return "", err
	}
	archive := filepath.Join(staging, filepath.Base(url))
	if err := os.WriteFile(archive, data, 0o644); err != nil {
		return "", err
	}
	if err := pm.Extract(archive, filepath.Join(staging, "tree")); err != nil {
		return "", err
	}
	os.Remove(archive)
	if err := os.Rename(staging, root); err != nil {
		return "", err
	}
	if found := findFile(root, name); found != "" {
		return found, nil
	}
	return "", &fs.PathError{Op: "find", Path: filepath.Join(root, name), Err: fs.ErrNotExist}
}

// Hash checked bytes of url, read from the cache directory or downloaded into it
func fetch(dir, url, sha string) ([]byte, error) {
	cached := filepath.Join(dir, url[strings.LastIndex(url, "/")+1:])
	data, err := os.ReadFile(cached)
	if err != nil {
		resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", url, resp.Status)
		}
		if data, err = io.ReadAll(resp.Body); err != nil {
			return nil, err
		}
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != sha {
		return nil, fmt.Errorf("%s does not match its pinned sha256", url)
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	return data, os.WriteFile(cached, data, 0o644)
}

// Installs a wheel by unpacking its archive into site-packages
func unzip(data []byte, site string) error {
	r, err := zip.NewReader(strings.NewReader(string(data)), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range r.File {
		dest := filepath.Join(site, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(dest, filepath.Clean(site)+string(os.PathSeparator)) {
			return fmt.Errorf("wheel member %s escapes site-packages", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o777); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
			return err
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		body, err := io.ReadAll(src)
		src.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, body, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func findFile(root, name string) string {
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name && found == "" {
			found = path
		}
		return nil
	})
	return found
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
