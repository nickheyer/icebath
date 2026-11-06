package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Windows launcher fixture whose fake curl.exe serves the files of a release directory
type cmdFixture struct {
	l                                           *engineLauncher
	env, path                                   map[string]string
	root, scripts, profile, cache, release, req string
	stderr                                      bytes.Buffer
}

func newCmdFixture(t *testing.T, arch string, manifest ...string) *cmdFixture {
	root := t.TempDir()
	f := &cmdFixture{root: root, scripts: filepath.Join(root, "skill scripts"), profile: filepath.Join(root, "profile"),
		cache: filepath.Join(root, "cache"), release: filepath.Join(root, "release"), req: filepath.Join(root, "requests.txt")}
	for _, d := range []string{f.scripts, f.profile, f.release, filepath.Join(root, "tools")} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(f.scripts, "VERSION"), []byte("9.9.9\r\n"), 0o644)
	os.WriteFile(filepath.Join(f.scripts, "engine-checksums.txt"), []byte("# digests\r\n\r\n"+strings.Join(manifest, "\r\n")), 0o644)
	curl := filepath.Join(root, "tools", "curl.exe")
	os.WriteFile(curl, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\n[ \"$1 $2\" = '-fsSL -o' ] || exit 2\n"+
		"src='%s'/\"${4##*/}\"\n[ -f \"$src\" ] || exit 22\nif [ -f \"$src.remove\" ]; then rm -f \"$3\"; exit 0; fi\ncat \"$src\" > \"$3\"\n", f.req, f.release)), 0o755)
	f.env = map[string]string{"USERPROFILE": f.profile, "IMPECCABLE_HOME": f.cache, "PROCESSOR_ARCHITECTURE": arch, "IMPECCABLE_DOWNLOAD_BASE": "https://release.test"}
	f.path = map[string]string{"curl.exe": curl}
	f.l = newEngineLauncher(f.scripts)
	f.l.GOOS, f.l.GOARCH = "windows", "amd64"
	f.l.Getenv = func(k string) string { return f.env[k] }
	f.l.LookPath = func(n string) (string, error) {
		if p, ok := f.path[n]; ok {
			return p, nil
		}
		return "", exec.ErrNotFound
	}
	f.l.Stderr = &f.stderr
	return f
}

func (f *cmdFixture) cacheDir() string { return filepath.Join(f.cache, "bin", "9.9.9") }
func (f *cmdFixture) cached() string   { return filepath.Join(f.cacheDir(), "impeccable.exe") }

func (f *cmdFixture) publish(asset, body string) {
	os.WriteFile(filepath.Join(f.release, asset), []byte(body), 0o644)
}

func (f *cmdFixture) requests() []string {
	data, _ := os.ReadFile(f.req)
	var urls []string
	for _, line := range strings.Fields(string(data)) {
		if strings.HasPrefix(line, "https://") {
			urls = append(urls, line)
		}
	}
	return urls
}

func (f *cmdFixture) files() []string {
	entries, _ := os.ReadDir(f.cacheDir())
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (f *cmdFixture) resolve(t *testing.T) (string, []string, error) {
	t.Helper()
	return f.l.resolveWindows(context.Background())
}

// Fake engine answering engine-probe with a version only inside a launcher probe that exported its home
func writeProbeEngine(t *testing.T, p, version string, status int) {
	os.MkdirAll(filepath.Dir(p), 0o755)
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = engine-probe ] && [ \"$IMPECCABLE_LAUNCHER_PROBE\" = 1 ] && [ -n \"$IMPECCABLE_SELF\" ]; then echo impeccable-engine %s; exit %d; fi\nexit 9\n", version, status)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func setupLines(f *cmdFixture) string {
	return "Engine 9.9.9 setup needs network access and write permission to \"" + f.cacheDir() + "\".\n" +
		"Run this launcher (\"" + filepath.Join(f.scripts, "impeccable.cmd") + "\") with engine-probe in a terminal that has those permissions, then retry the original command.\n" +
		"Alternatively, set IMPECCABLE_HOME to a writable cache location, or IMPECCABLE_BIN to a preinstalled engine binary."
}

func TestImpeccableCmdArchSelectsAssetAndX64Fallback(t *testing.T) {
	sum := sumOf(fakeEngine)
	cases := []struct {
		name, arch string
		manifest   []string
		published  []string
		requests   []string
		failure    string
	}{
		{"amd64", "AMD64", []string{"9.9.9 impeccable-windows-x64.exe " + sum}, []string{"impeccable-windows-x64.exe"}, []string{"impeccable-windows-x64.exe"}, ""},
		{"x86 runs x64", "x86", []string{"9.9.9 impeccable-windows-x64.exe " + sum}, []string{"impeccable-windows-x64.exe"}, []string{"impeccable-windows-x64.exe"}, ""},
		{"arm64 ignoring case", "arm64", []string{"9.9.9 IMPECCABLE-WINDOWS-ARM64.EXE " + strings.ToUpper(sum)}, []string{"impeccable-windows-arm64.exe"}, []string{"impeccable-windows-arm64.exe"}, ""},
		{"arm64 with only an x64 line", "ARM64", []string{"9.9.9 impeccable-windows-x64.exe " + sum}, []string{"impeccable-windows-x64.exe"}, []string{"impeccable-windows-x64.exe"}, ""},
		{"arm64 asset missing", "ARM64", []string{"9.9.9 impeccable-windows-arm64.exe " + sumOf("arm"), "9.9.9 impeccable-windows-x64.exe " + sum}, []string{"impeccable-windows-x64.exe"}, []string{"impeccable-windows-arm64.exe", "impeccable-windows-x64.exe"}, ""},
		{"both assets missing", "ARM64", []string{"9.9.9 impeccable-windows-arm64.exe " + sum, "9.9.9 impeccable-windows-x64.exe " + sum}, nil, []string{"impeccable-windows-arm64.exe", "impeccable-windows-x64.exe"},
			"impeccable: could not download engine 9.9.9 from https://release.test/engine-v9.9.9/impeccable-windows-x64.exe; check network access and the release URL."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCmdFixture(t, c.arch, c.manifest...)
			for _, a := range c.published {
				f.publish(a, fakeEngine)
			}
			bin, _, err := f.resolve(t)
			var want []string
			for _, r := range c.requests {
				want = append(want, "https://release.test/engine-v9.9.9/"+r)
			}
			if got := f.requests(); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("requests %v want %v", got, want)
			}
			if c.failure != "" {
				if err == nil || err.Error() != c.failure+"\n"+setupLines(f) || f.files() != nil {
					t.Fatalf("failure %v files %v", err, f.files())
				}
				return
			}
			if err != nil || bin != f.cached() || strings.Join(f.files(), " ") != "impeccable.exe impeccable.exe.verified" {
				t.Fatalf("bin %s %v files %v", bin, err, f.files())
			}
			if stamp, _ := os.ReadFile(bin + ".verified"); string(stamp) != fmt.Sprintf("%s %d\r\n", sum, len(fakeEngine)) {
				t.Fatalf("stamp %q", stamp)
			}
		})
	}
}

func TestImpeccableCmdLocalSourcesAndProbe(t *testing.T) {
	sum := sumOf(fakeEngine)
	f := newCmdFixture(t, "AMD64", "9.9.9 impeccable-windows-x64.exe "+sum)
	f.publish("impeccable-windows-x64.exe", fakeEngine)
	dirBin := filepath.Join(f.root, "preinstalled")
	os.MkdirAll(dirBin, 0o755)
	f.env["IMPECCABLE_BIN"] = dirBin
	if bin, env, err := f.resolve(t); err != nil || bin != dirBin || env != nil {
		t.Fatalf("IMPECCABLE_BIN that exists %s %v %v", bin, env, err)
	}
	f.env["IMPECCABLE_BIN"] = filepath.Join(f.root, "absent.exe")
	sibling := filepath.Join(f.scripts, "bin", "windows-x64", "impeccable.exe")
	writeProbeEngine(t, sibling, "9.9.9", 0)
	if bin, _, err := f.resolve(t); err != nil || bin != sibling {
		t.Fatalf("sibling reporting the pin %s %v", bin, err)
	}
	writeProbeEngine(t, sibling, "1.0.0", 0)
	homeBin := filepath.Join(f.profile, ".impeccable", "bin", "impeccable.exe")
	writeProbeEngine(t, homeBin, "4.0.0", 0)
	if bin, _, err := f.resolve(t); err != nil || bin != homeBin {
		t.Fatalf("home bin after a stale sibling %s %v", bin, err)
	}
	if want := `impeccable: skipping "` + sibling + `": it reports engine 1.0.0 but this skill pins 9.9.9; delete it to stop this warning` + "\n"; f.stderr.String() != want {
		t.Fatalf("sibling warning %q", f.stderr.String())
	}
	f.stderr.Reset()
	writeProbeEngine(t, sibling, "9.9.9", 1)
	writeProbeEngine(t, homeBin, "4.0.0", 1)
	if bin, _, err := f.resolve(t); err != nil || bin != f.cached() || !strings.Contains(f.stderr.String(), "it reports engine unknown but this skill pins 9.9.9") {
		t.Fatalf("failed probes fall through to the download %s %v %q", bin, err, f.stderr.String())
	}
	os.Remove(f.cached())
	os.Remove(f.cached() + ".verified")
	f.env["IMPECCABLE_LAUNCHER_PROBE"] = "1"
	if bin, _, err := f.resolve(t); err != nil || bin != sibling {
		t.Fatalf("probe mode trusts the sibling %s %v", bin, err)
	}
	os.Remove(sibling)
	var probeErr *EngineError
	if _, _, err := f.resolve(t); !errors.As(err, &probeErr) || len(probeErr.Lines) != 0 || len(f.requests()) != 1 {
		t.Fatalf("probe mode skips the home bin and the download quietly %v %d", err, len(f.requests()))
	}
	delete(f.env, "IMPECCABLE_LAUNCHER_PROBE")
	os.WriteFile(filepath.Join(f.scripts, "VERSION"), []byte("\r\n9.9.9\r\n"), 0o644)
	writeProbeEngine(t, sibling, "1.0.0", 1)
	if bin, _, err := f.resolve(t); err != nil || bin != sibling {
		t.Fatalf("sibling without a pinned version %s %v", bin, err)
	}
	os.Remove(sibling)
	os.WriteFile(filepath.Join(f.scripts, "VERSION"), []byte("9.9.9\n"), 0o644)
	delete(f.env, "IMPECCABLE_HOME")
	onPath := filepath.Join(f.root, "path", "impeccable.exe")
	writeProbeEngine(t, onPath, "2.0.0", 0)
	f.path["impeccable"] = onPath
	bin, env, err := f.resolve(t)
	if want := "IMPECCABLE_HOME=" + filepath.Join(f.profile, ".impeccable"); err != nil || bin != onPath || strings.Join(env, " ") != want {
		t.Fatalf("PATH engine %s %v %v", bin, env, err)
	}
	writeProbeEngine(t, onPath, "2.0.0", 3)
	bin, env, err = f.resolve(t)
	if want := filepath.Join(f.profile, ".impeccable", "bin", "9.9.9", "impeccable.exe"); err != nil || bin != want ||
		strings.Join(env, " ") != "IMPECCABLE_HOME="+filepath.Join(f.profile, ".impeccable") {
		t.Fatalf("PATH engine failing the handshake downloads into the profile cache %s %v %v", bin, env, err)
	}
}

func TestImpeccableCmdCacheStampRules(t *testing.T) {
	sum := sumOf(fakeEngine)
	size := fmt.Sprint(len(fakeEngine))
	past := time.Now().Add(-time.Minute)
	cases := []struct {
		name, arch, digest, stamp string
		binaryOlder, staleNewer   bool
		hashed, downloaded        bool
	}{
		{"unstamped", "AMD64", sum, "", false, false, true, false},
		{"trusted stamp skips hashing", "AMD64", strings.Repeat("1", 64), strings.Repeat("1", 64) + " " + size + "\r\n", true, false, false, false},
		{"last stamp line wins", "AMD64", strings.Repeat("1", 64), "; comment\r\n" + strings.Repeat("0", 64) + " 7\r\n" + strings.Repeat("1", 64) + " " + size, true, false, false, false},
		{"empty stamp", "AMD64", sum, "", false, false, true, false},
		{"foreign stamp", "AMD64", sum, strings.Repeat("0", 64) + " " + size, true, false, true, false},
		{"digest only stamp", "AMD64", sum, sum, true, false, true, false},
		{"stamp size differs", "AMD64", sum, sum + " 1", true, false, true, false},
		{"binary newer than stamp", "AMD64", sum, sum + " " + size, false, false, true, false},
		{"leftover newer than stamp", "AMD64", sum, sum + " " + size, true, true, true, false},
		{"arm64 cache holding the x64 build", "ARM64", sum, "", false, false, true, false},
		{"tampered", "AMD64", sumOf("other engine"), "", false, false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asset := "impeccable-windows-x64.exe"
			f := newCmdFixture(t, c.arch, "9.9.9 "+asset+" "+c.digest)
			f.publish(asset, "#!/bin/sh\necho other engine\n")
			if c.name == "tampered" {
				f.publish(asset, "other engine")
			}
			os.MkdirAll(f.cacheDir(), 0o755)
			os.WriteFile(f.cached(), []byte(fakeEngine), 0o755)
			if c.stamp != "" || c.name == "empty stamp" {
				os.WriteFile(f.cached()+".verified", []byte(c.stamp), 0o644)
			}
			if c.binaryOlder {
				os.Chtimes(f.cached(), past, past)
			} else if c.stamp != "" {
				os.Chtimes(f.cached()+".verified", past, past)
			}
			if c.staleNewer {
				os.WriteFile(filepath.Join(f.cacheDir(), "impeccable.exe.part"), []byte("stale"), 0o444)
			}
			hashes := 0
			f.l.HashFile = func(p string) string { hashes++; return sha256File(p) }
			bin, _, err := f.resolve(t)
			if err != nil || bin != f.cached() || (hashes > 0) != c.hashed || (len(f.requests()) == 1) != c.downloaded {
				t.Fatalf("bin %s %v hashes %d requests %v", bin, err, hashes, f.requests())
			}
			if c.downloaded {
				want := `impeccable: cannot verify the cached engine "` + f.cached() + `" against "` + filepath.Join(f.scripts, "engine-checksums.txt") + `"; not using it` + "\n"
				if got, _ := os.ReadFile(bin); f.stderr.String() != want || string(got) != "other engine" {
					t.Fatalf("tampered cache %q %q", f.stderr.String(), got)
				}
				return
			}
			if f.stderr.Len() != 0 {
				t.Fatalf("cache use stays quiet %q", f.stderr.String())
			}
			if stamp, _ := os.ReadFile(bin + ".verified"); c.hashed && string(stamp) != sum+" "+size+"\r\n" {
				t.Fatalf("rewritten stamp %q", stamp)
			}
		})
	}
}

func TestImpeccableCmdRefusesWithLauncherDiagnostics(t *testing.T) {
	sum := sumOf(fakeEngine)
	url := "https://release.test/engine-v9.9.9/impeccable-windows-x64.exe"
	removed := "impeccable: download completed but the file was removed before execution: " + url + "; check your antivirus quarantine or logs. Refusing to continue; do not disable protection."
	empty := "impeccable: downloaded file is empty: " + url + "; refusing the unverified download"
	cases := []struct {
		name     string
		prepare  func(f *cmdFixture)
		want     func(f *cmdFixture) string
		requests int
	}{
		{"no pinned version", func(f *cmdFixture) { os.Remove(filepath.Join(f.scripts, "VERSION")) }, func(f *cmdFixture) string {
			cached := f.cache + string(filepath.Separator) + "bin" + string(filepath.Separator) + string(filepath.Separator) + "impeccable.exe"
			return "impeccable: no engine binary found (looked in " + filepath.Join(f.scripts, "bin", "windows-x64", "impeccable.exe") + ", " + cached + ", PATH).\n" +
				"Download impeccable-windows-x64.exe from https://github.com/pbakaus/impeccable/releases (tag engine-v) and save it as " + cached + ", or set IMPECCABLE_BIN to a preinstalled engine binary. Docs: https://impeccable.style"
		}, 0},
		{"no checksum line", func(f *cmdFixture) {
			os.WriteFile(filepath.Join(f.scripts, "engine-checksums.txt"), []byte("9.9.9 impeccable-windows-x64.exe\r\n9.9.90 impeccable-windows-x64.exe "+sum+"\r\n"), 0o644)
		}, func(f *cmdFixture) string {
			return `impeccable: no checksum for engine 9.9.9 (impeccable-windows-x64.exe) in "` + filepath.Join(f.scripts, "engine-checksums.txt") + `"; refusing the unverified download`
		}, 0},
		{"curl missing", func(f *cmdFixture) { delete(f.path, "curl.exe") }, func(f *cmdFixture) string {
			return "impeccable: cannot download engine 9.9.9; curl.exe is unavailable.\n" + setupLines(f)
		}, 0},
		{"cache directory blocked", func(f *cmdFixture) {
			os.MkdirAll(f.cache, 0o755)
			os.WriteFile(filepath.Join(f.cache, "bin"), []byte("blocked"), 0o644)
		}, func(f *cmdFixture) string {
			return `impeccable: engine 9.9.9 is not installed; cannot create cache directory: "` + f.cacheDir() + "\"\n" + setupLines(f)
		}, 0},
		{"staging claims refused", func(f *cmdFixture) { os.MkdirAll(f.cacheDir(), 0o755); os.Chmod(f.cacheDir(), 0o555) }, func(f *cmdFixture) string {
			return `impeccable: engine 9.9.9 is not installed; cannot write to cache directory: "` + f.cacheDir() + "\"\n" + setupLines(f)
		}, 0},
		{"download failed", func(f *cmdFixture) { os.Remove(filepath.Join(f.release, "impeccable-windows-x64.exe")) }, func(f *cmdFixture) string {
			return "impeccable: could not download engine 9.9.9 from " + url + "; check network access and the release URL.\n" + setupLines(f)
		}, 1},
		{"empty download", func(f *cmdFixture) { f.publish("impeccable-windows-x64.exe", "") }, func(*cmdFixture) string { return empty }, 1},
		{"quarantined download", func(f *cmdFixture) { f.publish("impeccable-windows-x64.exe.remove", "") }, func(*cmdFixture) string { return removed }, 1},
		{"hash failed", func(f *cmdFixture) { f.l.HashFile = func(string) string { return "" } }, func(*cmdFixture) string {
			return "impeccable: cannot verify " + url + " (certutil could not hash it); refusing the unverified download"
		}, 1},
		{"removed during hash", func(f *cmdFixture) { f.l.HashFile = func(p string) string { os.Remove(p); return "" } }, func(*cmdFixture) string { return removed }, 1},
		{"checksum mismatch", func(f *cmdFixture) { f.publish("impeccable-windows-x64.exe", "tampered") }, func(*cmdFixture) string {
			return "impeccable: checksum mismatch downloading " + url
		}, 1},
		{"move failed", func(f *cmdFixture) { f.l.Rename = func(string, string) error { return os.ErrPermission } }, func(*cmdFixture) string {
			return "impeccable: could not cache the verified download: " + url
		}, 1},
		{"removed before move", func(f *cmdFixture) {
			f.l.Rename = func(old, _ string) error { os.Remove(old); return os.ErrNotExist }
		}, func(*cmdFixture) string { return removed }, 1},
		{"removed after move", func(f *cmdFixture) {
			f.l.Rename = func(old, dst string) error { os.Rename(old, dst); return os.Remove(dst) }
		}, func(*cmdFixture) string { return removed }, 1},
		{"emptied after move", func(f *cmdFixture) {
			f.l.Rename = func(old, dst string) error { os.Rename(old, dst); return os.WriteFile(dst, nil, 0o644) }
		}, func(*cmdFixture) string { return empty }, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCmdFixture(t, "AMD64", "9.9.9 impeccable-windows-x64.exe "+sum)
			f.publish("impeccable-windows-x64.exe", fakeEngine)
			c.prepare(f)
			defer os.Chmod(f.cacheDir(), 0o755)
			_, _, err := f.resolve(t)
			if err == nil || err.Error() != c.want(f) {
				t.Fatalf("got %v\nwant %s", err, c.want(f))
			}
			if len(f.requests()) != c.requests || isFile(f.cached()) {
				t.Fatalf("requests %v cached %v", f.requests(), isFile(f.cached()))
			}
			for _, name := range f.files() {
				if strings.HasPrefix(name, ".impeccable.part.") {
					t.Fatalf("staging left behind %v", f.files())
				}
			}
		})
	}
}

func TestImpeccableCmdStagingWriteFailureDiscardsStaging(t *testing.T) {
	sum := sumOf(fakeEngine)
	f := newCmdFixture(t, "AMD64", "9.9.9 impeccable-windows-x64.exe "+sum)
	f.publish("impeccable-windows-x64.exe", fakeEngine)
	deep := t.TempDir()
	for len(deep)+len("/bin/9.9.9/") < 4056-250 {
		deep = filepath.Join(deep, strings.Repeat("d", 200))
	}
	deep = filepath.Join(deep, strings.Repeat("e", 4056-len(deep)-len("/bin/9.9.9/")-1))
	f.cache, f.env["IMPECCABLE_HOME"] = deep, deep
	_, _, err := f.resolve(t)
	if want := `impeccable: engine 9.9.9 is not installed; cannot write to cache directory: "` + f.cacheDir() + "\"\n" + setupLines(f); err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
	if !pathExists(f.cacheDir()) || len(f.files()) != 0 || len(f.requests()) != 0 {
		t.Fatalf("staging kept %v requests %v", f.files(), f.requests())
	}
}

func TestImpeccableCmdDownloadStagesPerRunAndExports(t *testing.T) {
	sum := sumOf(fakeEngine)
	f := newCmdFixture(t, "AMD64", "9.9.9 impeccable-windows-x64.exe "+sum)
	f.publish("impeccable-windows-x64.exe", fakeEngine)
	delete(f.env, "IMPECCABLE_DOWNLOAD_BASE")
	os.MkdirAll(f.cacheDir(), 0o755)
	for _, name := range []string{"impeccable.exe.sha256", ".impeccable.part.1"} {
		os.WriteFile(filepath.Join(f.cacheDir(), name), []byte("stale"), 0o444)
	}
	bin, env, err := f.resolve(t)
	if err != nil || bin != f.cached() || strings.Join(env, " ") != "IMPECCABLE_DOWNLOAD_BASE="+defaultEngineDownloadBase {
		t.Fatalf("bin %s %v %v", bin, env, err)
	}
	if got := strings.Join(f.files(), " "); got != ".impeccable.part.1 impeccable.exe impeccable.exe.sha256 impeccable.exe.verified" {
		t.Fatalf("files %s", got)
	}
	args, _ := os.ReadFile(f.req)
	staged := regexp.MustCompile(`^-fsSL -o ` + regexp.QuoteMeta(f.cacheDir()) + `/\.impeccable\.part\.\d{8}-\d+/impeccable\.exe ` +
		regexp.QuoteMeta(defaultEngineDownloadBase) + `/engine-v9\.9\.9/impeccable-windows-x64\.exe\n$`)
	if !staged.Match(args) {
		t.Fatalf("curl arguments %q", args)
	}
	if again, _, err := f.resolve(t); err != nil || again != bin || len(f.requests()) != 1 || f.stderr.Len() != 0 {
		t.Fatalf("cached reuse %s %v %v %q", again, err, f.requests(), f.stderr.String())
	}
	if !regexp.MustCompile(`^\d{8}-\d{1,10}$`).MatchString(cmdStagingSuffix(time.Date(2026, 1, 2, 3, 4, 5, 60000000, time.UTC))) ||
		!strings.HasPrefix(cmdStagingSuffix(time.Date(2026, 1, 2, 3, 4, 5, 60000000, time.UTC)), "03040506-") {
		t.Fatal("staging suffix does not follow the TIME digits")
	}
	if got := strings.Join(f.l.exports(), " "); got != "IMPECCABLE_SKILL_DIR="+f.scripts+string(filepath.Separator)+".. IMPECCABLE_SELF="+filepath.Join(f.scripts, "impeccable.cmd") {
		t.Fatalf("exports %s", got)
	}
	exit := filepath.Join(f.root, "exit.exe")
	os.WriteFile(exit, []byte("#!/bin/sh\n[ \"$1 $2\" = '/d /c' ] && case \"$IMPECCABLE_SELF\" in *impeccable.cmd) exit 7;; esac\nexit 1\n"), 0o755)
	f.env["IMPECCABLE_BIN"] = exit
	cmd, err := f.l.command(context.Background(), "/d", "/c")
	if err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("engine exit code not forwarded %v", err)
	}
}

func TestImpeccableCmdParsesLikeCmd(t *testing.T) {
	if v, ok := parseCmdProbe("\r\n; banner here\r\nimpeccable-engine 3.1.0 extra\r\n"); !ok || v != "3.1.0" {
		t.Fatalf("probe %q %v", v, ok)
	}
	if v, ok := parseCmdProbe("noise\nimpeccable-engine 2.0\n"); !ok || v != "2.0" {
		t.Fatalf("for /f skips a line without a second token %q %v", v, ok)
	}
	if _, ok := parseCmdProbe("Unknown command: engine-probe\n"); ok {
		t.Fatal("the retired npm CLI passed the handshake")
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	if v := cmdSetPLine(write("VERSION", "1.2.3\r\nnext\r\n")); v != "1.2.3" || cmdSetPLine(write("LF", "4.5.6\n")) != "4.5.6" || cmdSetPLine(filepath.Join(dir, "absent")) != "" {
		t.Fatalf("set /p %q", v)
	}
	m := write("m.txt", "# c\r\n9.9.9 impeccable-windows-x64.exe\r\n9.9.9 Impeccable-Windows-X64.exe aaa\r\n9.9.9 impeccable-windows-x64.exe bbb\r\n9.9.9-RC impeccable-windows-arm64.exe ccc")
	if cmdManifestDigest(m, "9.9.9", "impeccable-windows-x64.exe") != "aaa" || cmdManifestDigest(m, "9.9.9-rc", "impeccable-windows-arm64.exe") != "" ||
		cmdManifestDigest(m, "9.9.9-RC", "impeccable-windows-arm64.exe") != "ccc" {
		t.Fatal("manifest digest")
	}
	if s, n := cmdStamp(write("s", "abc\r\n")); s != "abc" || n != "" {
		t.Fatalf("stamp %q %q", s, n)
	}
}

func TestImpeccableCmdLauncherSourceShape(t *testing.T) {
	for _, rel := range []string{"impeccable/skill/scripts/impeccable.cmd", "impeccable/plugin/skills/impeccable/scripts/impeccable.cmd"} {
		source, err := payloadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ReplaceAll(source, []byte("\r\n"), nil), []byte("\n")) {
			t.Errorf("%s: cmd.exe batch labels require CRLF line endings", rel)
		}
		if regexp.MustCompile(`%cached%\.(part|sha256)`).Match(source) || !regexp.MustCompile(`set "staging=[^"\n]*%RANDOM%`).Match(source) ||
			!regexp.MustCompile(`mkdir "%staging%" >nul 2>nul\r?\nif errorlevel 1 goto claim_staging`).Match(source) {
			t.Errorf("%s: does not stage each run under its own name", rel)
		}
	}
}
