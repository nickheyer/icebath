package updaterecord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

func vtInt(n int64) *int64 { return &n }

func vtStr(s string) *string { return &s }

// Points stamp resolution at a written stamp and clears the identity cache around the test
func vtStamp(t *testing.T, stamp map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "install-stamp.json")
	data, _ := json.Marshal(stamp)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	vtSeams(t, func() string { return path }, "", nil)
	return path
}

// Swaps stamp resolution and build identity for the test and clears the cache
func vtSeams(t *testing.T, stamp func() string, version string, settings map[string]string) {
	t.Helper()
	previousStamp, previousBuild := resolveStampFile, buildIdentity
	if stamp != nil {
		resolveStampFile = stamp
	}
	buildIdentity = func() (string, map[string]string) {
		if settings == nil {
			return version, map[string]string{}
		}
		return version, settings
	}
	resetVersionInfoCache()
	t.Cleanup(func() {
		resolveStampFile, buildIdentity = previousStamp, previousBuild
		resetVersionInfoCache()
	})
}

func vtInfo(t *testing.T) VersionInfo {
	t.Helper()
	info, err := GetVersionInfo()
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestDerivedVersionShowsPlusQuestionForDirtyUnknownDistance(t *testing.T) {
	for _, tc := range []struct {
		distance *int64
		dirty    bool
		want     string
	}{{nil, true, "0.19.0+?"}, {nil, false, "0.19.0"}, {vtInt(5), true, "0.19.0+5"}, {vtInt(0), true, "0.19.0"}} {
		if got := derivedVersion("0.19.0", tc.distance, tc.dirty, ""); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func TestDisplayVersionNamesTheDistanceWithoutTheCommit(t *testing.T) {
	ahead := VersionInfo{BaseVersion: "0.21.5", DerivedVersion: "0.21.5+1913.gf83a9e9", Distance: vtInt(1913), Commit: vtStr("f83a9e9" + "000000000000000000000000000000000"), Branch: vtStr("main"), Source: "git"}
	if ahead.DisplayVersion() != "0.21.5+1913" {
		t.Fatal(ahead.DisplayVersion())
	}
	if got := (VersionInfo{BaseVersion: "0.21.5", DerivedVersion: "0.21.5", Distance: vtInt(0), Source: "git"}).DisplayVersion(); got != "0.21.5" {
		t.Fatal(got)
	}
}

func TestDisplayVersionKeepsTaglessStampIdentity(t *testing.T) {
	vtStamp(t, map[string]any{"commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "baseVersion": "unknown", "displayVersion": "git.aaaaaaa",
		"source": "git", "payload": "bootstrap", "updateMechanism": "self"})
	info := vtInfo(t)
	if info.BaseVersion != "unknown" || info.DisplayVersion() != "git.aaaaaaa" || *info.Commit != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("%+v", info)
	}
}

func TestDisplayVersionKeepsUnknownWhenNoIdentity(t *testing.T) {
	if got := (VersionInfo{BaseVersion: "unknown", DerivedVersion: "unknown", Source: "unknown"}).DisplayVersion(); got != "unknown" {
		t.Fatal(got)
	}
}

func TestStampVersionInfoReadsNixStamp(t *testing.T) {
	commit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	vtStamp(t, map[string]any{"schemaVersion": 2, "commit": commit, "branch": "feature/version", "baseVersion": "0.19.0",
		"displayVersion": "0.19.0+3", "distance": 3, "dirty": false, "source": "nix", "distribution": "nix", "updateMechanism": "external"})
	want := VersionInfo{BaseVersion: "0.19.0", DerivedVersion: "0.19.0+3", Distance: vtInt(3), Commit: vtStr(commit), Branch: vtStr("feature/version"), Source: "nix", Distribution: vtStr("nix")}
	if info := vtInfo(t); !reflect.DeepEqual(info, want) {
		t.Fatalf("%+v", info)
	}
}

func TestStampVersionInfoPreservesCIProvenanceAndDockerDistribution(t *testing.T) {
	vtStamp(t, map[string]any{"commit": "dddddddddddddddddddddddddddddddddddddddd", "source": "ci", "distribution": "docker", "updateMechanism": "external"})
	if info := vtInfo(t); info.Source != "ci" || info.Distribution == nil || *info.Distribution != "docker" {
		t.Fatalf("%+v", info)
	}
}

func TestStampVersionInfoPreservesMissingBranch(t *testing.T) {
	commit := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	vtStamp(t, map[string]any{"schemaVersion": 2, "commit": commit, "branch": nil, "baseVersion": "0.19.0", "displayVersion": "0.19.0+?",
		"distance": nil, "dirty": true, "source": "docker", "updateMechanism": "external"})
	want := VersionInfo{BaseVersion: "0.19.0", DerivedVersion: "0.19.0+?", Commit: vtStr(commit), Source: "docker", Dirty: true}
	if info := vtInfo(t); !reflect.DeepEqual(info, want) {
		t.Fatalf("%+v", info)
	}
}

func TestStampVersionInfoIgnoresFallbackCommit(t *testing.T) {
	vtStamp(t, map[string]any{"schemaVersion": 2, "commit": "0000000000000000000000000000000000000000", "branch": "main", "source": "fallback", "updateMechanism": "self"})
	if info := vtInfo(t); info.Source != "unknown" || info.Commit != nil {
		t.Fatalf("%+v", info)
	}
}

func TestStampVersionInfoReturnsNoneWhenFileMissing(t *testing.T) {
	vtSeams(t, func() string { return "" }, "", nil)
	if info, err := stampVersionInfo(); info != nil || err != nil {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestGetVersionInfoUnknownWhenNoStampAndNoBuildIdentity(t *testing.T) {
	vtSeams(t, func() string { return "" }, "", nil)
	info := vtInfo(t)
	if info.BaseVersion != "unknown" || info.DerivedVersion != "unknown" || info.Distance != nil || info.Commit != nil || info.Source != "unknown" {
		t.Fatalf("%+v", info)
	}
}

func TestGetVersionInfoDerivesIdentityFromReachableReleaseTag(t *testing.T) {
	revision := "abcdef0123456789abcdef0123456789abcdef01"
	vtSeams(t, func() string { return "" }, "v0.21.4-1-gabcdef0", map[string]string{"vcs.revision": revision, "vcs.time": "2026-09-21T10:00:00Z"})
	info := vtInfo(t)
	if info.BaseVersion != "0.21.4" || info.DerivedVersion != "0.21.4+1.gabcdef0" || info.Distance == nil || *info.Distance != 1 ||
		info.Commit == nil || *info.Commit != revision || info.Source != "git" || info.CommitDate == nil || *info.CommitDate != 1789984800 {
		t.Fatalf("%+v", info)
	}
}

func TestGetVersionInfoBuildIdentityShapes(t *testing.T) {
	revision := "1234567890abcdef1234567890abcdef12345678"
	for _, tc := range []struct {
		version, modified, base, derived, source string
		distance                                 *int64
	}{
		{"v1.2.3", "false", "1.2.3", "1.2.3", "build", vtInt(0)},
		{"v1.2.3+canary.20260921T100000Z", "false", "1.2.3", "1.2.3", "git", nil},
		{"v1.2.3-4-g1234567-dirty", "false", "1.2.3", "1.2.3+4.g1234567.dirty", "git", vtInt(4)},
		{"", "true", "unknown", "git.1234567.dirty", "git", nil},
		{"v2026.9.21", "false", "unknown", "git.1234567", "git", nil},
	} {
		vtSeams(t, func() string { return "" }, tc.version, map[string]string{"vcs.revision": revision, "vcs.modified": tc.modified})
		info := vtInfo(t)
		if info.BaseVersion != tc.base || info.DerivedVersion != tc.derived || info.Source != tc.source || !reflect.DeepEqual(info.Distance, tc.distance) {
			t.Fatalf("%s: %+v", tc.version, info)
		}
	}
}

func TestResolveStampFileHonorsInstallRoot(t *testing.T) {
	root := t.TempDir()
	data, _ := json.Marshal(map[string]any{"commit": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "source": "nix", "distribution": "nix", "updateMechanism": "external"})
	os.WriteFile(filepath.Join(root, "install-stamp.json"), data, 0o644)
	t.Setenv(pm.EnvInstallRoot, root)
	vtSeams(t, nil, "", nil)
	if got := resolveStampFile(); got != filepath.Join(root, "install-stamp.json") {
		t.Fatal(got)
	}
	if info := vtInfo(t); *info.Commit != "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" || info.Source != "nix" || *info.Distribution != "nix" {
		t.Fatalf("%+v", info)
	}
}

func TestResolveStampFileInstallRootWithoutStampIsNone(t *testing.T) {
	t.Setenv(pm.EnvInstallRoot, t.TempDir())
	if got := resolveStampFile(); got != "" {
		t.Fatal(got)
	}
}

func TestResolveStampFileFallsBackToCodeRootWhenEnvUnset(t *testing.T) {
	t.Setenv(pm.EnvInstallRoot, "")
	os.Unsetenv(pm.EnvInstallRoot)
	stamp := filepath.Join(pm.RepoRoot(), "install-stamp.json")
	if _, err := os.Lstat(stamp); err == nil {
		t.Fatalf("%s already exists", stamp)
	}
	if err := os.WriteFile(stamp, []byte(`{"commit": "ffffffffffffffffffffffffffffffffffffffff", "source": "docker", "updateMechanism": "external"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(stamp) })
	if got := resolveStampFile(); got != stamp {
		t.Fatal(got)
	}
}

func TestMalformedStampDegradesToTheBuildIdentity(t *testing.T) {
	for _, stamp := range []map[string]any{
		{"commit": "cccccccccccccccccccccccccccccccccccccccc", "updateMechanism": "bogus"},
		{"commit": "cccccccccccccccccccccccccccccccccccccccc", "updateMechanism": "self", "payload": "light"},
	} {
		path := vtStamp(t, stamp)
		if _, err := stampVersionInfo(); err == nil {
			t.Fatalf("%v accepted", stamp)
		} else if _, ok := err.(*StampError); !ok || !filepath.IsAbs(path) {
			t.Fatalf("%T %v", err, err)
		}
		if info := vtInfo(t); info.Source != "unknown" {
			t.Fatalf("%+v", info)
		}
	}
}

func TestCodeIdentityUserAgentAndCheckoutSHA(t *testing.T) {
	commit := "c0ffee0000000000000000000000000000000000"
	vtStamp(t, map[string]any{"commit": commit, "baseVersion": "9.8.7", "source": "ci", "updateMechanism": "external", "commitDate": 1700000000})
	identity, err := GetCodeIdentity(true)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := identity.MarshalJSON(); string(got) != `{"sha":"c0ffee0000000000000000000000000000000000","short_sha":"c0ffee00","version":"9.8.7","source":"ci","commit_date":1700000000}` {
		t.Fatal(string(got))
	}
	if agent, err := UserAgent("icebath-cli"); err != nil || agent != "icebath-cli/9.8.7" {
		t.Fatal(agent, err)
	}
	if sha, err := ReadCheckoutSHA(); err != nil || sha == nil || *sha != commit {
		t.Fatal(sha, err)
	}
	vtSeams(t, func() string { return "" }, "", map[string]string{"vcs.revision": "feed"})
	if sha, err := ReadCheckoutSHA(); err != nil || sha == nil || *sha != "feed" {
		t.Fatal(sha, err)
	}
	vtSeams(t, func() string { return "" }, "", nil)
	if sha, err := ReadCheckoutSHA(); err != nil || sha != nil {
		t.Fatal(sha, err)
	}
}
