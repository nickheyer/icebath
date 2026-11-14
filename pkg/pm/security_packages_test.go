package pm

import (
	"testing"
)

// Pinned archive file names come out the way Path of the urlparse path names them
func TestSecurityPackagesArchiveFilenameMatchesPython(t *testing.T) {
	isolateHome(t)
	for raw, want := range map[string]string{
		"https://x/a/b.tar.gz;p=1": "b.tar.gz",
		"https://x/a;p/b.tar.gz":   "b.tar.gz",
		"https://x/a%20b.tar.gz":   "a%20b.tar.gz",
		"https://x":                "",
		"https://x/a/b/.":          "b",
		"docker://x/y;z":           "y;z",
	} {
		if got, err := archiveFilename(map[string]any{"url": raw}); err != nil || got != want {
			t.Fatalf("archiveFilename(%q) = %q %v, want %q", raw, got, err, want)
		}
	}
	for _, tc := range []struct {
		archive map[string]any
		want    string
	}{
		{map[string]any{}, "KeyError: 'url'"},
		{map[string]any{"url": nil}, "TypeError: argument should be a str or an os.PathLike object where __fspath__ returns a str, not 'bytes'"},
		{map[string]any{"url": jsonInt(5)}, "AttributeError: 'int' object has no attribute 'decode'"},
		{map[string]any{"url": "https://[::1/x.tar.gz"}, "Invalid IPv6 URL"},
	} {
		if _, err := archiveFilename(tc.archive); err == nil || err.Error() != tc.want {
			t.Fatalf("archiveFilename(%v) error %v, want %s", tc.archive, err, tc.want)
		}
	}
}

// Iron proxy targets split into exactly a platform and an architecture, as the Python tuple unpack demands
func TestSecurityPackagesIronProxyFetchURLUnpacksTargets(t *testing.T) {
	isolateHome(t)
	proxy := &IronProxy{}
	if got, err := proxy.FetchURL("1.2.3", "linux-x64-musl"); err != nil || got != "https://github.com/paradigmxyz/iron-proxy/releases/download/v1.2.3/iron-proxy_1.2.3_linux_amd64.tar.gz" {
		t.Fatalf("musl url %q %v", got, err)
	}
	if got, err := proxy.FetchURL("1.2.3", "darwin-arm64"); err != nil || got != "https://github.com/paradigmxyz/iron-proxy/releases/download/v1.2.3/iron-proxy_1.2.3_darwin_arm64.tar.gz" {
		t.Fatalf("darwin url %q %v", got, err)
	}
	if _, err := proxy.FetchURL("1.2.3", "linux-arm64-bionic"); err == nil || err.Error() != "ValueError: too many values to unpack (expected 2, got 3)" {
		t.Fatalf("bionic error %v", err)
	}
	if _, err := proxy.FetchURL("1.2.3", "linux"); err == nil || err.Error() != "ValueError: not enough values to unpack (expected 2, got 1)" {
		t.Fatalf("bare platform error %v", err)
	}
}
