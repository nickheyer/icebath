package msalruntime

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// source is one archive Microsoft publishes with the native library inside it, both pinned by sha256
type source struct {
	url          string
	sha256       string
	member       string
	memberSHA256 string
	glibc        [2]int
}

const archiveHost = "https://files.pythonhosted.org/packages/"

// The archives of the pinned release for each platform, newest C library requirement first
var sources = map[string][]source{
	"darwin/arm64": {{url: archiveHost + "c3/4f/cef2ea4164b0d2313b02b8d95105cb2b75fb375a4f866eeba89e20cf5021/pymsalruntime-0.20.7-cp314-cp314-macosx_12_0_arm64.whl",
		sha256: "35b156fdadae4f2324a57bfb1e9636added10f5ae9eceaeb804946cae6467631", member: "pymsalruntime/libmsalruntime_arm64.dylib",
		memberSHA256: "ed6b1d058d7fb4354cc2f65bede6efe1c496d745ed8a875d39364834c4c5601f"}},
	"darwin/amd64": {{url: archiveHost + "0b/48/da3c7cf713c480738bd37858e73a4828e3d2f4b20e323957be49187bd07b/pymsalruntime-0.20.7-cp314-cp314-macosx_12_0_x86_64.whl",
		sha256: "f1ee868b94e593c318e1e7ceb28227cbdafc3e8eda63ce20f0c8f343d77dbe1f", member: "pymsalruntime/libmsalruntime_x86_64.dylib",
		memberSHA256: "fcbb4410ecbe403447f219268c7f989dcdc3856d3d97e7d8d15f55bb3509dfad"}},
	"linux/amd64": {
		{url: archiveHost + "96/68/08fd3a53a9cdfd03ca112830d583413cef3b74f1e9a440288f416129fc7d/pymsalruntime-0.20.7-cp314-cp314-manylinux_2_35_x86_64.whl",
			sha256: "4e014a2dc2740b460b439cdd5e44d4a5dec04d16b7352fe516d74905b88f9fa5", member: "pymsalruntime/libmsalruntime.so",
			memberSHA256: "6bea0ce01961a1edb9577c4da6946a6c05985be2f89787fb37f43c262a26e3b6", glibc: [2]int{2, 35}},
		{url: archiveHost + "55/75/28d4efe32e2f426b9c5e8eaf110a9b2a40455755ec717fe2a5c982cd635b/pymsalruntime-0.20.7-cp314-cp314-manylinux_2_24_x86_64.whl",
			sha256: "d5cf5d6300075c2256be86187b801a982787f515b127cdc660b9f305428cef12", member: "pymsalruntime/libmsalruntime.so",
			memberSHA256: "6ff94c70a760147ffd5df52f06e8bdd3d357a69815ae018283e90276bbc683aa", glibc: [2]int{2, 24}},
	},
	"windows/386": {{url: archiveHost + "c7/3d/dc25547e9dea2c71abca33a6ba3618074e05a7f9dd03ecc3e59e439840e8/pymsalruntime-0.20.7-cp314-cp314-win32.whl",
		sha256: "792473936741d61ea39e310b2d1c2d8cef2e2ebb334f73a137c99a1e17ad89b0", member: "pymsalruntime/msalruntime_x86.dll",
		memberSHA256: "5ce7f0bdebfd8376190f291981ff34145730939b554e88122fbe8009778e29c0"}},
	"windows/amd64": {{url: archiveHost + "a4/f5/cc6ffe6959a6dbb7e77b03c6d38948364bac73ae6a3a744815008ce1839e/pymsalruntime-0.20.7-cp314-cp314-win_amd64.whl",
		sha256: "93d21bdc85ce42ed0fed0de059298f916e657eb13b37df3781a0e4f310c39128", member: "pymsalruntime/msalruntime.dll",
		memberSHA256: "20fbeebae4fa6f94caafd3009ff38bb2030db879f351d0743ec793658b547437"}},
	"windows/arm64": {{url: archiveHost + "b5/41/35be1daa5b6768830f7a255a11d92b8a98334ec98a9938c2c0278f0894e5/pymsalruntime-0.20.7-cp314-cp314-win_arm64.whl",
		sha256: "b70225c0a18d89bd7faef7bf74c4d73dd10c185ac005a6d72f1b07c7add79e86", member: "pymsalruntime/msalruntime_arm64.dll",
		memberSHA256: "74a06ead92211a70b6622e54935f34e99f69e01209013b6a35cdf492709a05c5"}},
}

// Largest archive the downloader reads, several times the biggest published one
const archiveLimit = 128 << 20

var (
	ensureMu sync.Mutex
	verified = map[string]string{}
	// C library version of the running system, replaced in tests
	systemGlibc = glibcVersion
)

// LibraryPath is where the native library of this platform lives under an icebath home
func LibraryPath(home string) (string, bool) {
	name, ok := LibraryName()
	return filepath.Join(home, "lib", "msalruntime", Version, name), ok
}

// The archive for this platform, on Linux the newest build the system C library can run
func pickSource() (source, error) {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	list := sources[platform]
	if len(list) == 0 {
		return source{}, fmt.Errorf("Microsoft publishes no msalruntime %s library for %s", Version, platform)
	}
	if runtime.GOOS != "linux" {
		return list[0], nil
	}
	major, minor, ok := systemGlibc()
	for _, src := range list {
		if ok && (major > src.glibc[0] || major == src.glibc[0] && minor >= src.glibc[1]) {
			return src, nil
		}
	}
	oldest := list[len(list)-1].glibc
	if !ok {
		return source{}, fmt.Errorf("the msalruntime %s library needs the GNU C library %d.%d or newer, and this system has none", Version, oldest[0], oldest[1])
	}
	return source{}, fmt.Errorf("the msalruntime %s library needs the GNU C library %d.%d or newer, this system has %d.%d", Version, oldest[0], oldest[1], major, minor)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// The archive bytes at a URL, refused when they exceed the size limit or miss the pinned sha256
func download(ctx context.Context, src source) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", src.url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, archiveLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > archiveLimit {
		return nil, fmt.Errorf("%s is larger than %d bytes", src.url, archiveLimit)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != src.sha256 {
		return nil, fmt.Errorf("%s has sha256 %s, the pinned archive has %s", src.url, hex.EncodeToString(sum[:]), src.sha256)
	}
	return data, nil
}

// Writes the pinned member of the archive to path through a temporary file, refusing bytes that miss its sha256
func extract(archive []byte, src source, path string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return err
	}
	member, err := zr.Open(src.member)
	if err != nil {
		return fmt.Errorf("the archive holds no %s: %w", src.member, err)
	}
	defer member.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.part")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), member)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != src.memberSHA256 {
		return fmt.Errorf("%s in the archive has sha256 %s, the pinned library has %s", src.member, sum, src.memberSHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Ensure returns the native library under an icebath home, fetching the pinned archive and extracting the library when it is absent or altered
func Ensure(ctx context.Context, home string) (string, error) {
	path, ok := LibraryPath(home)
	if !ok {
		return "", fmt.Errorf("Microsoft publishes no msalruntime %s library for %s/%s", Version, runtime.GOOS, runtime.GOARCH)
	}
	src, err := pickSource()
	if err != nil {
		return "", err
	}
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if verified[path] == src.memberSHA256 {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	if sum, err := fileSHA256(path); err != nil || sum != src.memberSHA256 {
		archive, err := download(ctx, src)
		if err != nil {
			return "", fmt.Errorf("the msalruntime library could not be fetched: %w", err)
		}
		if err := extract(archive, src, path); err != nil {
			return "", fmt.Errorf("the msalruntime library could not be installed: %w", err)
		}
	}
	verified[path] = src.memberSHA256
	return path, nil
}
