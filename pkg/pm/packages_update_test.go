package pm

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Release document with one asset per name, the shape GitHub's releases API returns
func catRelease(tag string, names ...string) map[string]any {
	assets := []any{}
	for _, name := range names {
		assets = append(assets, map[string]any{"name": name})
	}
	return map[string]any{"tag_name": tag, "assets": assets}
}

// The ffmpeg indexes upstream advertises, a retained BtbN month end tag and martin-riedl release dirs
func catFfmpegIndexes(t *testing.T) (string, string, map[string]string) {
	tag, version := "autobuild-2026-09-30-12-00", "9.1.2"
	var assets []string
	for _, suffix := range []string{
		"linux64-gpl-9.1.tar.xz", "linuxarm64-gpl-9.1.tar.xz", "linux64-gpl-shared-9.1.tar.xz", "linux64-lgpl-9.1.tar.xz",
		"win64-gpl-shared-9.1.zip", "win64-lgpl-9.1.zip", "win64-gpl-9.1.zip", "winarm64-gpl-9.1.zip",
	} {
		assets = append(assets, fmt.Sprintf("ffmpeg-n%s-1-gabcdef-%s", version, suffix))
	}
	daily := func(release, date string) any {
		return catRelease("autobuild-"+date, fmt.Sprintf("ffmpeg-n%s-1-gabcdef-win64-gpl-9.1.zip", release))
	}
	releases := []any{daily("9.1.3", "2026-10-02-12-00"), catRelease(tag, assets...), daily("9.1.1", "2026-09-10-12-00")}
	catSwap(t, &getJSON, func(string) (any, error) { return releases, nil })
	paths := map[string]string{}
	for _, osPair := range [][2]string{{"linux", "linux"}, {"darwin", "macos"}} {
		for _, archPair := range [][2]string{{"x64", "amd64"}, {"arm64", "arm64"}} {
			paths[osPair[0]+"-"+archPair[0]] = fmt.Sprintf("%s/%s/1788300000_%s/ffmpeg.zip", osPair[1], archPair[1], version)
		}
	}
	catSwap(t, &getText, func(string, map[string]string) (string, error) {
		var links []string
		for _, path := range paths {
			links = append(links, fmt.Sprintf(`<a href="/download/%s">Ffmpeg</a>`, path))
		}
		return strings.Join(links, "\n"), nil
	})
	return version, tag, paths
}

func TestCatResolvedFfmpegVersionFetchesItsExactTargetAsset(t *testing.T) {
	for _, target := range []string{"win32-x64", "win32-arm64", "linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64"} {
		t.Run(target, func(t *testing.T) {
			version, tag, paths := catFfmpegIndexes(t)
			p := newFfmpeg()
			got, err := p.FetchURL(version, target)
			if err != nil {
				t.Fatal(err)
			}
			latest, err := p.LatestVersions(target, "")
			if err != nil || !slices.Contains(latest, version) {
				t.Fatalf("latest = %q, %v", latest, err)
			}
			want := "https://ffmpeg.martin-riedl.de/download/" + paths[target]
			if strings.HasPrefix(target, "win32") || strings.HasPrefix(target, "linux") {
				arch := "arm64"
				if strings.HasSuffix(target, "-x64") {
					arch = "64"
				}
				asset := fmt.Sprintf("ffmpeg-n%s-1-gabcdef-linux%s-gpl-9.1.tar.xz", version, arch)
				if strings.HasPrefix(target, "win32") {
					asset = fmt.Sprintf("ffmpeg-n%s-1-gabcdef-win%s-gpl-9.1.zip", version, arch)
				}
				want = fmt.Sprintf("https://github.com/BtbN/FFmpeg-Builds/releases/download/%s/%s", tag, asset)
			}
			if got != want {
				t.Fatalf("url = %q, want %q", got, want)
			}
		})
	}
}

func TestCatUnadvertisedFfmpegVersionRefusesInsteadOfRelabellingOldBytes(t *testing.T) {
	for _, target := range []string{"win32-x64", "win32-arm64", "linux-x64", "darwin-arm64"} {
		t.Run(target, func(t *testing.T) {
			catFfmpegIndexes(t)
			_, err := newFfmpeg().FetchURL("9.9.9", target)
			dsWantInstallError(t, err, `9\.9\.9`)
		})
	}
}

func TestCatOnlyRetainedMonthEndFfmpegBuildsArePinned(t *testing.T) {
	catFfmpegIndexes(t)
	versions, err := newFfmpeg().LatestVersions("win32-x64", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(versions, "9.1.2") || slices.Contains(versions, "9.1.3") || slices.Contains(versions, "9.1.1") {
		t.Fatalf("versions = %q", versions)
	}
}

func TestCatNodeCandidatesExcludePrereleases(t *testing.T) {
	catSwap(t, &getJSON, func(string) (any, error) {
		return []any{
			map[string]any{"version": "v26.8.0-alpha.0"}, map[string]any{"version": "v26.7.0"},
			map[string]any{"version": "v24.0.0-rc.1"}, map[string]any{"version": "v24.20.0"},
		}, nil
	})
	versions, err := catPackage(t, "node").LatestVersions("linux-x64", "")
	if err != nil || !slices.Equal(versions, []string{"26.7.0", "24.20.0"}) {
		t.Fatalf("node candidates = %q, %v", versions, err)
	}
}

func TestCatPythonCandidatePreservesTheAdvertisedPatchAndBuild(t *testing.T) {
	for target, triple := range map[string]string{
		"linux-x64": "x86_64-unknown-linux-gnu", "win32-arm64": "aarch64-pc-windows-msvc", "darwin-arm64": "aarch64-apple-darwin",
	} {
		t.Run(target, func(t *testing.T) {
			tag := "20990102"
			identity := "3.14.8+" + tag
			advertised := fmt.Sprintf("cpython-%s-%s-install_only.tar.gz", identity, triple)
			release := catRelease(tag,
				fmt.Sprintf("cpython-3.15.0+%s-%s-install_only.tar.gz", tag, triple),
				fmt.Sprintf("cpython-%s-%s-freethreaded-install_only.tar.gz", identity, triple),
				fmt.Sprintf("cpython-%s-freethreaded-%s-install_only.tar.gz", identity, triple),
				advertised)
			catSwap(t, &getJSON, func(string) (any, error) { return []any{release}, nil })
			p := newPython()
			result, err := p.LatestVersions(target, "3.14.7+20981231")
			if err != nil || !slices.Equal(result, []string{identity}) {
				t.Fatalf("candidates = %q, %v", result, err)
			}
			raw, err := p.FetchURL(result[0], target)
			if err != nil {
				t.Fatal(err)
			}
			download, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			segments := strings.Split(download.EscapedPath(), "/")
			name, err := url.PathUnescape(segments[len(segments)-1])
			if err != nil || name != advertised || segments[len(segments)-2] != tag {
				t.Fatalf("download path %q", download.EscapedPath())
			}
		})
	}
}

func TestCatPythonCandidatesRejectNonmatchingAssetsAndLeaveManualTargetsAlone(t *testing.T) {
	tag, triple := "20990102", "x86_64-unknown-linux-gnu"
	var calls []string
	for _, name := range []string{
		"cpython-3.14.8+20981231-" + triple + "-install_only.tar.gz",
		"cpython-3.15.0+" + tag + "-" + triple + "-install_only.tar.gz",
		"cpython-3.14.8+" + tag + "-" + triple + "-freethreaded-install_only.tar.gz",
		"cpython-3.14.8+" + tag + "-freethreaded-" + triple + "-install_only.tar.gz",
		"cpython-3.14.8+" + tag + "-" + triple + "-install_only.tar.gz.extra",
		"cpython-3.14.8+" + tag + "-" + triple + "-install_only.tar.gz\n",
	} {
		catSwap(t, &getJSON, func(u string) (any, error) {
			calls = append(calls, u)
			return []any{catRelease(tag, name)}, nil
		})
		if got, err := newPython().LatestVersions("linux-x64", "3.14.7+20981231"); err != nil || len(got) != 0 {
			t.Fatalf("%q yielded %q, %v", name, got, err)
		}
	}
	calls = nil
	if got, err := newPython().LatestVersions(bionicTarget, "3.14.7+20981231"); err != nil || len(got) != 0 || len(calls) != 0 {
		t.Fatalf("bionic candidates %q, %v after %d calls", got, err, len(calls))
	}
}
