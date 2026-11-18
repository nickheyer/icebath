//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

var rpBuilds sync.Map

// Tiny program answering install-self --release-info with tag and commit, the identity a release binary reports
func rpBinary(t *testing.T, tag, commit string) []byte {
	t.Helper()
	key := tag + "@" + commit
	if cached, ok := rpBuilds.Load(key); ok {
		return cached.([]byte)
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	dir := t.TempDir()
	src := "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"strings\"\n)\n\nvar version, commit string\n\nfunc main() {\n" +
		"\tif strings.Join(os.Args[1:], \" \") == \"install-self --release-info\" {\n" +
		"\t\tfmt.Printf(\"{\\\"tag\\\":\\\"%s\\\",\\\"commit\\\":\\\"%s\\\"}\\n\", version, commit)\n\t}\n}\n"
	for name, body := range map[string]string{"go.mod": "module fixture\n\ngo 1.21\n", "main.go": src} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "icebath")
	cmd := exec.Command(gobin, "build", "-ldflags", "-X main.version="+tag+" -X main.commit="+commit, "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-trimpath", "CGO_ENABLED=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, msg)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	rpBuilds.Store(key, data)
	return data
}

// One published release of the fake source
type rpRelease struct {
	tag, commit, target, published string
	latest                         bool
	binary                         []byte
}

// Loopback release source of one repository, its API, compare over a linear history, refs and downloads
type rpSource struct {
	srv      *httptest.Server
	repo     string
	releases []rpRelease
	history  []string
}

func newRPSource(t *testing.T, history []string, releases ...rpRelease) *rpSource {
	t.Helper()
	s := &rpSource{releases: releases, history: history}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	s.repo = s.srv.URL + "/o/r.git"
	return s
}

func (s *rpSource) asset(tag string) string {
	return install.ReleaseArchiveName(tag, runtime.GOOS, runtime.GOARCH)
}

// Release archive holding binary at its root under the name the host's archive gives it
func rpArchive(binary []byte) []byte {
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("icebath.exe")
		w.Write(binary)
		zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "icebath", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	tw.Write(binary)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func (s *rpSource) apiJSON(rel rpRelease) map[string]any {
	base := s.srv.URL + "/o/r/releases/download/" + rel.tag + "/"
	return map[string]any{"tag_name": rel.tag, "target_commitish": rel.target, "draft": false, "prerelease": false, "published_at": rel.published,
		"assets": []any{map[string]any{"name": s.asset(rel.tag), "browser_download_url": base + s.asset(rel.tag)},
			map[string]any{"name": "checksums.txt", "browser_download_url": base + "checksums.txt"}}}
}

func (s *rpSource) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	rest, api := strings.CutPrefix(path, "/api/v3/repos/o/r/")
	switch {
	case api && rest == "releases":
		out := []any{}
		if r.URL.Query().Get("page") == "1" {
			for _, rel := range s.releases {
				out = append(out, s.apiJSON(rel))
			}
		}
		json.NewEncoder(w).Encode(out)
	case api && rest == "releases/latest":
		for _, rel := range s.releases {
			if rel.latest {
				json.NewEncoder(w).Encode(s.apiJSON(rel))
				return
			}
		}
		http.NotFound(w, r)
	case api && strings.HasPrefix(rest, "releases/tags/"):
		for _, rel := range s.releases {
			if rel.tag == strings.TrimPrefix(rest, "releases/tags/") {
				json.NewEncoder(w).Encode(s.apiJSON(rel))
				return
			}
		}
		http.NotFound(w, r)
	case api && strings.HasPrefix(rest, "compare/"):
		base, head, _ := strings.Cut(strings.TrimPrefix(rest, "compare/"), "...")
		from, to := slices.Index(s.history, base), slices.Index(s.history, head)
		status := map[bool]string{true: "ahead", false: "behind"}[to > from]
		if to == from {
			status = "identical"
		}
		if from < 0 || to < 0 {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status": status, "ahead_by": max(to-from, 0), "behind_by": max(from-to, 0)})
	case path == "/o/r.git/info/refs":
		pkt := func(line string) string { return fmt.Sprintf("%04x%s", len(line)+4, line) }
		io.WriteString(w, pkt("# service=git-upload-pack\n")+"0000")
		for _, rel := range s.releases {
			io.WriteString(w, pkt(rel.commit+" refs/tags/"+rel.tag+"\n"))
		}
		io.WriteString(w, "0000")
	case strings.HasPrefix(path, "/o/r/releases/download/"):
		tag, name, _ := strings.Cut(strings.TrimPrefix(path, "/o/r/releases/download/"), "/")
		for _, rel := range s.releases {
			if rel.tag != tag {
				continue
			}
			if name == "checksums.txt" {
				sum := sha256.Sum256(rpArchive(rel.binary))
				fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), s.asset(rel.tag))
				return
			}
			if name == s.asset(rel.tag) {
				w.Write(rpArchive(rel.binary))
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Release install of the fixture build of tag and commit, its home, root and the completion requests it hands on
type rpInstall struct {
	root      string
	completed []map[string]any
}

func newRPInstall(t *testing.T, tag, commit string) *rpInstall {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	inst := &rpInstall{root: t.TempDir()}
	if err := os.WriteFile(install.InstallBinaryPath(inst.root), rpBinary(t, tag, commit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := install.WriteInstallMethod(inst.root, install.MethodScript); err != nil {
		t.Fatal(err)
	}
	savedRoot, savedComplete := fleetProjectRoot, zipCompleteSourceUpdate
	t.Cleanup(func() { fleetProjectRoot, zipCompleteSourceUpdate = savedRoot, savedComplete })
	fleetProjectRoot = func() string { return inst.root }
	zipCompleteSourceUpdate = func(_ context.Context, _ io.Writer, request map[string]any) error {
		inst.completed = append(inst.completed, request)
		return nil
	}
	return inst
}

// Release identity the binary in place reports
func (inst *rpInstall) running(t *testing.T) install.ReleaseInfo {
	t.Helper()
	return install.InstalledRelease(context.Background(), install.InstallBinaryPath(inst.root))
}

// Output and exit code of update --check, nil error for a zero exit
func (inst *rpInstall) check(t *testing.T, channel *string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	err := checkReleaseUpdate(context.Background(), &out, "main", false, channel)
	var exit *pm.SystemExit
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exit):
		return out.String(), exit.Code
	}
	t.Fatal(err)
	return "", 0
}

// Output and exit code of the release update
func (inst *rpInstall) update(t *testing.T, args map[string]any) (string, int) {
	t.Helper()
	var out bytes.Buffer
	err := updateViaRelease(context.Background(), &out, args, map[string]any{})
	var exit *pm.SystemExit
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exit):
		return out.String(), exit.Code
	}
	t.Fatal(err)
	return "", 0
}

// Check and update keep the build over an older or equal release and swap in a newer one
func TestReleaseUpdateMovesOnlyToANewerBuild(t *testing.T) {
	c1, c2, c3 := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	history := []string{c1, c2, c3}
	for _, tc := range []struct {
		name, newest, check, update, after, sha string
	}{
		{"older", "v1.0.0", "✓ Already up to date.\n", "✓ Already up to date.\n", "v1.1.0", c2},
		{"equal", "v1.1.0", "✓ Already up to date.\n", "✓ Already up to date.\n", "v1.1.0", c2},
		{"newer", "v1.2.0", "⚕ Update available: 1 commit behind main.\n  Run 'icebath update' to install.\n", "→ Release: main v1.2.0 (333333333333)\n", "v1.2.0", c3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := newRPInstall(t, "v1.1.0", c2)
			published := map[string]string{"v1.0.0": "2026-09-01T00:00:00Z", "v1.1.0": "2026-10-01T00:00:00Z", "v1.2.0": "2026-11-01T00:00:00Z"}
			published[tc.newest] = "2026-12-01T00:00:00Z"
			var releases []rpRelease
			for tag, commit := range map[string]string{"v1.0.0": c1, "v1.1.0": c2, "v1.2.0": c3} {
				releases = append(releases, rpRelease{tag: tag, commit: commit, target: "main", published: published[tag], binary: rpBinary(t, tag, commit)})
			}
			src := newRPSource(t, history, releases...)
			t.Setenv("ICEBATH_REPO_URL", src.repo)
			check, code := inst.check(t, nil)
			if code != 0 || check != "→ Update channel: main\n"+tc.check {
				t.Fatalf("check %d %q", code, check)
			}
			update, code := inst.update(t, map[string]any{})
			if code != 0 || !strings.HasPrefix(update, "→ Update channel: main\n"+tc.update) {
				t.Fatalf("update %d %q", code, update)
			}
			if got := inst.running(t); got.Tag != tc.after || got.Commit != tc.sha {
				t.Fatalf("binary in place %+v", got)
			}
			request := inst.completed[0]
			if request["expected_sha"] != tc.sha || request["apply_mode"] != "release" {
				t.Fatalf("completion request %v", request)
			}
			if moved := tc.after != "v1.1.0"; moved == (request["completion_message"] == "✓ Already up to date!") {
				t.Fatalf("completion message %v", request["completion_message"])
			}
		})
	}
}

// The repository the install recorded serves the update, ICEBATH_REPO_URL overriding it
func TestReleaseUpdateReadsTheRepositoryTheInstallCameFrom(t *testing.T) {
	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	inst := newRPInstall(t, "v1.0.0", c1)
	recorded := newRPSource(t, []string{c1, c2}, rpRelease{tag: "v1.1.0", commit: c2, target: "main", published: "2026-11-01T00:00:00Z", binary: rpBinary(t, "v1.1.0", c2)})
	other := newRPSource(t, []string{c1, c2}, rpRelease{tag: "v1.0.0", commit: c1, target: "main", published: "2026-11-01T00:00:00Z", binary: rpBinary(t, "v1.0.0", c1)})
	if err := install.WriteInstalledRepository(inst.root, recorded.repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_REPO_URL", other.repo)
	if check, _ := inst.check(t, nil); !strings.Contains(check, "✓ Already up to date.") {
		t.Fatalf("override check %q", check)
	}
	t.Setenv("ICEBATH_REPO_URL", "")
	if check, _ := inst.check(t, nil); !strings.Contains(check, "Update available: 1 commit behind main") {
		t.Fatalf("recorded check %q", check)
	}
	if update, code := inst.update(t, map[string]any{}); code != 0 || inst.running(t).Tag != "v1.1.0" {
		t.Fatalf("recorded update %d %q", code, update)
	}
}

// A stable install riding the default waits for the next release, a chosen stable channel moves back to it
func TestStableReleaseOlderThanTheBuildMovesBackOnlyWithConsent(t *testing.T) {
	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	inst := newRPInstall(t, "v1.1.0", c2)
	t.Cleanup(pm.PatchRepoRoot(inst.root))
	updaterecord.SetBuildVersion("v1.1.0")
	t.Cleanup(func() { updaterecord.SetBuildVersion("") })
	src := newRPSource(t, []string{c1, c2},
		rpRelease{tag: "v1.0.0", commit: c1, target: "main", published: "2026-09-01T00:00:00Z", latest: true, binary: rpBinary(t, "v1.0.0", c1)},
		rpRelease{tag: "v1.1.0", commit: c2, target: "main", published: "2026-10-01T00:00:00Z", binary: rpBinary(t, "v1.1.0", c2)})
	t.Setenv("ICEBATH_REPO_URL", src.repo)
	held := "✓ No newer release: stable v1.0.0 (111111111111); this build is newer and waits for the next release (`icebath update --set-channel main` follows every commit).\n"
	if check, code := inst.check(t, nil); code != 0 || check != "→ Update channel: stable\n"+held {
		t.Fatalf("default check %d %q", code, check)
	}
	if update, code := inst.update(t, map[string]any{}); code != 0 || !strings.Contains(update, held) || inst.running(t).Tag != "v1.1.0" {
		t.Fatalf("default update %d %q", code, update)
	}
	if _, err := SetInstallChannel(context.Background(), ChannelStable, inst.root); err != nil {
		t.Fatal(err)
	}
	back := "→ This build is newer than the selected release: stable v1.0.0 (111111111111).\n" +
		"  `icebath update` moves it back to that release (`icebath update --set-channel main` follows every commit).\n"
	if check, code := inst.check(t, nil); code != 0 || check != "→ Update channel: stable\n"+back {
		t.Fatalf("chosen check %d %q", code, check)
	}
	if update, code := inst.update(t, map[string]any{}); code != 0 || inst.running(t).Tag != "v1.0.0" {
		t.Fatalf("chosen update %d %q", code, update)
	}
}

// A build naming neither a release tag nor a commit is refused alike by the check and the update
func TestReleaseUpdateRefusesABuildOfUnknownOrigin(t *testing.T) {
	c1 := strings.Repeat("1", 40)
	inst := newRPInstall(t, "", "")
	src := newRPSource(t, []string{c1}, rpRelease{tag: "v1.0.0", commit: c1, target: "main", published: "2026-09-01T00:00:00Z", binary: rpBinary(t, "v1.0.0", c1)})
	t.Setenv("ICEBATH_REPO_URL", src.repo)
	if check, code := inst.check(t, nil); code != 1 || check != "✗ Could not check for updates: the release this binary was built from is unknown\n" {
		t.Fatalf("check %d %q", code, check)
	}
	update, code := inst.update(t, map[string]any{})
	if code != 1 || !strings.HasPrefix(update, "✗ Release update failed: the release this binary was built from is unknown\n") || inst.running(t).Tag != "" || len(inst.completed) != 0 {
		t.Fatalf("update %d %q", code, update)
	}
}
