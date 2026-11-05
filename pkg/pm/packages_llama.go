package pm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// LlamaCpp is one llama.cpp backend build with every library it loads staged beside llama-server
type LlamaCpp struct {
	BinaryPackage
	Backend string
	Assets  map[string]string
	Libgomp map[string]string
	cudart  bool
}

// Ubuntu 22.04 libgomp debs whose glibc floor sits at or below every Linux engine build
var llamaLibgomp = map[string]string{
	"linux-x64":   "https://archive.ubuntu.com/ubuntu/pool/main/g/gcc-12/libgomp1_12-20220319-1ubuntu1_amd64.deb",
	"linux-arm64": "https://ports.ubuntu.com/ubuntu-ports/pool/main/g/gcc-12/libgomp1_12-20220319-1ubuntu1_arm64.deb",
}

func newLlamaCpp(name, backend string, assets map[string]string, cudart bool) *LlamaCpp {
	p := &LlamaCpp{
		BinaryPackage: BinaryPackage{
			BasePackage: BasePackage{Name: name, Optional: true, OffPath: true},
			BinaryRel:   map[string]string{"win32": "llama-server.exe", "posix": "llama-server"},
			NoFlatten:   true,
			ProbeCwd:    true,
		},
		Backend: backend,
		Assets:  assets,
		Libgomp: llamaLibgomp,
		cudart:  cudart,
	}
	p.bind(p)
	return p
}

// Every registered llama.cpp backend, CUDA pinning its cudart archive beside the engine
func llamaBackends() []*LlamaCpp {
	return []*LlamaCpp{
		newLlamaCpp("llamacpp-cuda", "cuda", map[string]string{
			"win32-x64": "win-cuda-13.4-x64", "win32-arm64": "win-cuda-13.4-arm64",
			"linux-x64": "ubuntu-cuda-13.4-x64", "linux-arm64": "ubuntu-cuda-13.4-arm64",
		}, true),
		newLlamaCpp("llamacpp-vulkan", "vulkan", map[string]string{
			"win32-x64": "win-vulkan-x64", "linux-x64": "ubuntu-vulkan-x64", "linux-arm64": "ubuntu-vulkan-arm64",
		}, false),
		newLlamaCpp("llamacpp-hip", "hip", map[string]string{
			"win32-x64": "win-rocm-10.0-x64", "linux-x64": "ubuntu-rocm-10.0-x64",
		}, false),
		newLlamaCpp("llamacpp-metal", "metal", map[string]string{
			"darwin-x64": "macos-x64", "darwin-arm64": "macos-arm64",
		}, false),
		newLlamaCpp("llamacpp-cpu", "cpu", map[string]string{
			"win32-x64": "win-cpu-x64", "win32-arm64": "win-cpu-arm64", "linux-x64": "ubuntu-x64",
			"linux-arm64": "ubuntu-arm64", "darwin-x64": "macos-x64", "darwin-arm64": "macos-arm64",
		}, false),
	}
}

// Every known target upstream ships no build of this backend for
func (p *LlamaCpp) MissingReason(target string) string {
	for _, known := range AllTargets {
		if known == target {
			if _, ok := p.Assets[target]; !ok {
				return fmt.Sprintf("llama.cpp publishes no %s build for %s", p.Backend, target)
			}
			return ""
		}
	}
	return ""
}

// Release asset names for a target, the engine archive then the CUDA runtime archive
func (p *LlamaCpp) assetNames(version, target string) ([]string, error) {
	infix, ok := p.Assets[target]
	if !ok {
		return nil, keyError(target)
	}
	ext := "tar.gz"
	if strings.HasPrefix(target, "win32") {
		ext = "zip"
	}
	names := []string{fmt.Sprintf("llama-b%s-bin-%s.%s", version, infix, ext)}
	if p.cudart {
		if strings.HasPrefix(target, "win32") {
			names = append(names, fmt.Sprintf("cudart-llama-bin-%s.zip", infix))
		} else {
			names = append(names, fmt.Sprintf("cudart-llama-b%s-bin-%s.tar.gz", version, infix))
		}
	}
	return names, nil
}

func (p *LlamaCpp) FetchURLs(version, target string) ([]string, error) {
	names, err := p.assetNames(version, target)
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, asset := range names {
		urls = append(urls, fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/b%s/%s", version, asset))
	}
	if deb, ok := p.Libgomp[target]; ok {
		urls = append(urls, deb)
	}
	return urls, nil
}

func (p *LlamaCpp) FetchURL(version, target string) (string, error) {
	urls, err := p.FetchURLs(version, target)
	if err != nil {
		return "", err
	}
	return urls[0], nil
}

// The llama.app latest pointer first, then the bucket tree, then GitHub releases when the bucket shows nothing
func (p *LlamaCpp) LatestVersions(target, locked string) ([]string, error) {
	if latest, ok := LlamaAppLatest(); ok {
		return append([]string{latest}, LlamaAppBucketVersions()...), nil
	}
	if bucket := LlamaAppBucketVersions(); len(bucket) > 0 {
		return bucket, nil
	}
	return GithubReleaseTags("ggml-org/llama.cpp", "b")
}

// Digest GitHub's release API publishes for the asset, so pinning skips the download
func (p *LlamaCpp) KnownSHA256(version, url string) (string, error) {
	digests, err := githubReleaseDigests("ggml-org/llama.cpp", "b"+version)
	if err != nil {
		return "", err
	}
	return digests[url[strings.LastIndex(url, "/")+1:]], nil
}

func (p *LlamaCpp) Unpack(archive, staged, target string) error {
	if strings.HasSuffix(filepath.Base(archive), ".deb") {
		return UnpackDeb(p.Name, archive, staged)
	}
	return p.BinaryPackage.Unpack(archive, staged, target)
}

// Hoists nested binaries beside llama-server, moves libgomp up and merges CUDA runtime directories in
func (p *LlamaCpp) Stage(store *Store, staged, version, target string) error {
	bin := p.dispatch().Binary(staged, target)
	if bin == "" {
		return fmt.Errorf("AttributeError: 'NoneType' object has no attribute 'name'")
	}
	server := filepath.Base(bin)
	if !isRegularFile(filepath.Join(staged, server)) {
		var found []string
		for _, path := range sortedDescendants(staged) {
			if osNormcase(filepath.Base(path)) == osNormcase(server) {
				found = append(found, path)
			}
		}
		if len(found) == 0 {
			return NewInstallError(p.Name, "archive contains no llama-server", "")
		}
		if err := MergeTree(filepath.Dir(found[0]), staged); err != nil {
			return err
		}
	}
	libDirs, err := globChildren(filepath.Join(staged, "usr", "lib"), "*", true)
	if err != nil {
		return err
	}
	var libs []string
	for _, dir := range libDirs {
		found, err := globChildren(dir, "libgomp.so.1", false)
		if err != nil {
			return err
		}
		libs = append(libs, found...)
	}
	sortPathlib(libs)
	if len(libs) > 0 {
		if err := copyFileContents(libs[0], filepath.Join(staged, "libgomp.so.1")); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(staged, "usr")); err != nil {
			return err
		}
	}
	if !p.cudart {
		return nil
	}
	extras, err := globChildren(staged, "cudart-*", false)
	if err != nil {
		return err
	}
	sortPathlib(extras)
	for _, extra := range extras {
		if !isDirectory(extra) {
			continue
		}
		if err := MergeTree(extra, staged); err != nil {
			return err
		}
		if err := os.RemoveAll(extra); err != nil {
			return err
		}
	}
	return nil
}

// Copies a file's bytes through symlinks onto a new or truncated destination like shutil copyfile
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyStreamTo(in, dst)
}

// Writes a stream to a new or truncated file
func copyStreamTo(in io.Reader, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var (
	releaseDigestMu    sync.Mutex
	releaseDigestCache = map[[2]string]map[string]string{}
)

// Asset name to sha256 digest of one GitHub release, empty when the release API cannot be read
func githubReleaseDigests(repo, tag string) (map[string]string, error) {
	key := [2]string{repo, tag}
	releaseDigestMu.Lock()
	cached, ok := releaseDigestCache[key]
	releaseDigestMu.Unlock()
	if ok {
		return cached, nil
	}
	data, err := getJSON(fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag))
	if err != nil {
		return map[string]string{}, nil
	}
	release, isMap := data.(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(data))
	}
	digests := map[string]string{}
	assets := []any{}
	if raw, present := release["assets"]; present {
		if assets, err = pyIterItems(raw); err != nil {
			return nil, err
		}
	}
	for _, item := range assets {
		asset, isAsset := item.(map[string]any)
		if !isAsset {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(item))
		}
		digest := ""
		if raw := asset["digest"]; pyrepr.Truthy(raw) {
			text, isText := raw.(string)
			if !isText {
				return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'partition'", pyrepr.TypeName(raw))
			}
			_, digest, _ = strings.Cut(text, "sha256:")
		}
		if digest == "" {
			continue
		}
		name, present := asset["name"]
		if !present {
			return nil, keyError("name")
		}
		digests[pyrepr.Str(name)] = digest
	}
	releaseDigestMu.Lock()
	releaseDigestCache[key] = digests
	releaseDigestMu.Unlock()
	return digests, nil
}

// WhisperCppCpu is native local speech to text for ARM64 Windows, where faster-whisper has no wheel
type WhisperCppCpu struct {
	BinaryPackage
}

func newWhisperCppCpu() *WhisperCppCpu {
	gaps := map[string]string{}
	for _, target := range AllTargets {
		if target != "win32-arm64" {
			gaps[target] = "uses the existing faster-whisper provider"
		}
	}
	p := &WhisperCppCpu{BinaryPackage{
		BasePackage: BasePackage{Name: "whispercpp-cpu", Optional: true, Gaps: gaps,
			URL: "https://github.com/ggml-org/whisper.cpp/releases/download/{version}/whisper-bin-win-cpu-arm64.zip"},
		BinaryRel: map[string]string{"win32-arm64": "whisper-cli.exe"},
		ProbeArgs: []string{"--help"},
	}}
	p.bind(p)
	return p
}

// Items a Python for loop visits, dict keys and string characters included
func pyIterItems(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case map[string]any:
		out := make([]any, 0, len(t))
		for _, key := range sortedKeys(t) {
			out = append(out, key)
		}
		return out, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(v))
}

// Children of dir whose names match a single component pattern, case folded where pathlib folds case
func globChildren(dir, pattern string, dirsOnly bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var out []string
	for _, e := range entries {
		matched, err := filepath.Match(osNormcase(pattern), osNormcase(e.Name()))
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, e.Name())
		if matched && (!dirsOnly || isDirectory(path)) {
			out = append(out, path)
		}
	}
	return out, nil
}
