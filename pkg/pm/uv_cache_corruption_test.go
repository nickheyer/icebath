package pm

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestUVCacheOtherFailuresAreNotCacheCorruption(t *testing.T) {
	for i, output := range []string{
		"error: No solution found when resolving dependencies:\n  Because alpha==2 was not found ...",
		"error: Failed to fetch: `https://pypi.org/simple/alpha/`\n  Caused by: Request failed after 3 retries",
		"error: Failed to install: alpha-1.0-py3-none-any.whl (alpha==1.0)\n" +
			"  cause: failed to hardlink file from /c/archive-v0/abc/alpha/__init__.py to /v/lib/alpha/__init__.py: " +
			"No space left on device (os error 28)",
		"error: Failed to install: alpha-1.0-py3-none-any.whl (alpha==1.0)\n" +
			"  cause: failed to create directory `/v/lib/alpha`: Permission denied (os error 13)",
		"  Caused by: The wheel is invalid: Invalid Wheel-Version",
		"error: Failed to install: --force-1.0-py3-none-any.whl (x==1)\n  cause: The wheel is invalid: x",
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			isolateHome(t)
			if got := FailedCachedPackages(output); len(got) != 0 {
				t.Fatalf("FailedCachedPackages(%q) = %v", output, got)
			}
		})
	}
}

// Corrupt cache failures name packages exactly as the Python pattern does, Unicode boundaries included
func TestUVCacheFailedCachedPackagesMatchesPythonPatterns(t *testing.T) {
	isolateHome(t)
	for output, want := range map[string][]string{
		"error: Failed to install: lxml-6.1.3-cp314-cp314-linux_x86_64.whl (lxml==6.1.3)\n  Caused by: The wheel is invalid: Missing .dist-info directory": {"lxml"},
		"error: Failed to install: Alpha_Dep-1.0-py3-none-any.whl (x)\n  Caused by: `RECORD` file is invalid":                                              {"alpha-dep"},
		"Failed to install: a.whlé.whl (x) failed to install: b-1.WHL\nthe wheel is invalid":                                                               {"b"},
		"Failed to install: a-1.whlé (x)\nthe wheel is invalid":                                                                                            nil,
		"Failed to install: a-1.whl_x b-1.whl\nthe wheel is invalid":                                                                                       nil,
		"Failed to install: x-1.whl\nTHE WHEEL İS INVALID":                                                                                                 nil,
		"Failed to install: y-1.whl\n  cause: /c/archive-v0/ abc/y: No such file or directory (os error 2)":                                                nil,
		"Failed to install: z-1.whl\n  cause: /c/archive-v0/abc/z: No such file or directory (os error 2)":                                                 {"z"},
		"Failed to install: w x-1.whl\nthe wheel is invalid":                                                                                               nil,
		"failed to install: (q-1.whl) failed to install: r-1.whl\nthe wheel is invalid":                                                                    {"r"},
	} {
		got := sortedKeys(FailedCachedPackages(output))
		if len(got) != len(want) || (len(want) > 0 && strings.Join(got, ",") != strings.Join(want, ",")) {
			t.Fatalf("FailedCachedPackages(%q) = %v, want %v", output, got, want)
		}
	}
}

// Unpacked uv cache entry with the three dist-info files uv reads first
func rptCacheEntry(t *testing.T, cache, bucket, name string) string {
	t.Helper()
	info := filepath.Join(cache, "archive-v0", bucket, name+"-1.0.dist-info")
	writeFile(t, filepath.Join(info, "WHEEL"), []byte("Wheel-Version: 1.0\nGenerator: fixture\nRoot-Is-Purelib: true\nTag: py3-none-any\n"), 0o666)
	writeFile(t, filepath.Join(info, "METADATA"), []byte("Metadata-Version: 2.1\nName: "+name+"\nVersion: 1.0\n"), 0o666)
	writeFile(t, filepath.Join(info, "RECORD"), []byte(name+"/__init__.py,,\n"), 0o666)
	return info
}

// What a crash leaves, the file keeping its size while its data never reached the disk
func rptNulFill(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, make([]byte, max(st.Size(), 1)), 0o666)
}

// NUL filled cache entries are found by package and swept per package, the healthy cache reading clean
func TestUVCacheCorruptCachedPackagesSeesNulFilledEntries(t *testing.T) {
	tmp := isolateHome(t)
	cache := filepath.Join(tmp, "cache")
	alpha := rptCacheEntry(t, cache, "a1", "alpha_dep")
	beta := rptCacheEntry(t, cache, "b1", "beta_dep")
	if got := CorruptCachedPackages(cache); len(got) != 0 {
		t.Fatalf("healthy cache reported %v", got)
	}
	rptNulFill(t, filepath.Join(alpha, "WHEEL"))
	entries, err := os.ReadDir(beta)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		rptNulFill(t, filepath.Join(beta, e.Name()))
	}
	if got := sortedKeys(CorruptCachedPackages(cache)); strings.Join(got, ",") != "alpha-dep,beta-dep" {
		t.Fatalf("corrupt %v", got)
	}
	if err := RemoveCorruptEntries(cache, map[string]bool{"alpha-dep": true}); err != nil {
		t.Fatal(err)
	}
	want := "beta-dep"
	if runtime.GOOS == "windows" {
		want = "alpha-dep,beta-dep"
	}
	if got := sortedKeys(CorruptCachedPackages(cache)); strings.Join(got, ",") != want {
		t.Fatalf("after the sweep %v, want %s", got, want)
	}
}
