package builtin

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const defaultEngineDownloadBase = "https://github.com/pbakaus/impeccable/releases/download"

// Launcher failure carrying the stderr lines the shell launcher prints before exiting 127
type EngineError struct {
	Lines []string
}

func (e *EngineError) Error() string { return strings.Join(e.Lines, "\n") }

// Inputs of the engine resolution the launcher reads from its process
type engineLauncher struct {
	ScriptsDir string
	Launcher   string
	Getenv     func(string) string
	GOOS       string
	GOARCH     string
	Client     *http.Client
	LookPath   func(string) (string, error)
	HashFile   func(string) string
	Rename     func(string, string) error
	Stderr     io.Writer
}

// Launcher over the process environment for a scripts directory holding VERSION and engine-checksums.txt
func newEngineLauncher(scriptsDir string) *engineLauncher {
	return &engineLauncher{ScriptsDir: scriptsDir, Getenv: os.Getenv, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Client: &http.Client{Timeout: 5 * time.Minute}, LookPath: exec.LookPath, HashFile: sha256File, Rename: os.Rename, Stderr: os.Stderr}
}

// Engine command for the launcher arguments with IMPECCABLE_SKILL_DIR and IMPECCABLE_SELF exported
func EngineCommand(ctx context.Context, scriptsDir string, args ...string) (*exec.Cmd, error) {
	return newEngineLauncher(scriptsDir).command(ctx, args...)
}

func (l *engineLauncher) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	bin, extra, err := l.resolveHost(ctx)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(append(os.Environ(), l.exports()...), extra...)
	return cmd, nil
}

// Resolution of the launcher this host runs, impeccable.cmd on Windows and the sh launcher elsewhere
func (l *engineLauncher) resolveHost(ctx context.Context) (string, []string, error) {
	if l.GOOS == "windows" {
		return l.resolveWindows(ctx)
	}
	bin, err := l.resolve(ctx)
	return bin, nil, err
}

// Path of the launcher file the resolver stands in for
func (l *engineLauncher) launcherPath() string {
	if l.Launcher != "" {
		return l.Launcher
	}
	if l.GOOS == "windows" {
		return filepath.Join(l.ScriptsDir, "impeccable.cmd")
	}
	return filepath.Join(l.ScriptsDir, "impeccable")
}

// IMPECCABLE_SKILL_DIR and IMPECCABLE_SELF defaulted the way the launcher exports them
func (l *engineLauncher) exports() []string {
	skillDir := l.Getenv("IMPECCABLE_SKILL_DIR")
	if skillDir == "" && l.GOOS == "windows" {
		skillDir = l.ScriptsDir + string(filepath.Separator) + ".."
	} else if skillDir == "" {
		skillDir = filepath.Dir(l.ScriptsDir)
	}
	self := l.Getenv("IMPECCABLE_SELF")
	if self == "" {
		self = l.launcherPath()
	}
	return []string{"IMPECCABLE_SKILL_DIR=" + skillDir, "IMPECCABLE_SELF=" + self}
}

func (l *engineLauncher) platform() (string, string) {
	osName := "unknown"
	switch l.GOOS {
	case "darwin", "linux", "windows":
		osName = l.GOOS
	}
	arch := "unknown"
	switch l.GOARCH {
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "x64"
	}
	return osName, arch
}

func (l *engineLauncher) warn(format string, a ...any) {
	if l.Stderr != nil {
		fmt.Fprintf(l.Stderr, format+"\n", a...)
	}
}

// Engine version an engine-probe run reports, false when the binary does not answer as the engine
func (l *engineLauncher) probe(ctx context.Context, bin string) (string, bool) {
	cmd := exec.CommandContext(ctx, bin, "engine-probe")
	cmd.Env = append(append(os.Environ(), l.exports()...), "IMPECCABLE_LAUNCHER_PROBE=1")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	text := strings.TrimRight(string(out), "\n")
	if !strings.HasPrefix(text, "impeccable-engine") {
		return "", false
	}
	return strings.TrimPrefix(text, "impeccable-engine "), true
}

// Digest recorded in engine-checksums.txt for a version and asset, empty when absent
func (l *engineLauncher) manifestDigest(version, asset string) string {
	f, err := os.Open(filepath.Join(l.ScriptsDir, "engine-checksums.txt"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.FieldsFunc(sc.Text(), func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' })
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) >= 3 && fields[0] == version && fields[1] == asset {
			return fields[2]
		}
	}
	return ""
}

func sha256File(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode()&0o111 != 0
}

// Binary the launcher would exec, walking the bundled, home, cached, PATH and verified download sources
func (l *engineLauncher) resolve(ctx context.Context) (string, error) {
	probing := l.Getenv("IMPECCABLE_LAUNCHER_PROBE") != ""
	if b := l.Getenv("IMPECCABLE_BIN"); b != "" && isExecutable(b) {
		return b, nil
	}
	osName, arch := l.platform()
	version := ""
	if data, err := os.ReadFile(filepath.Join(l.ScriptsDir, "VERSION")); err == nil {
		version = strings.Join(strings.Fields(string(data)), "")
	}
	exe := ""
	if osName == "windows" {
		exe = ".exe"
	}
	bin := filepath.Join(l.ScriptsDir, "bin", osName+"-"+arch, "impeccable"+exe)
	if isFile(bin) && (isExecutable(bin) || os.Chmod(bin, 0o755) == nil) {
		probed, ok := "", false
		if !probing && version != "" {
			probed, ok = l.probe(ctx, bin)
		}
		if probing || version == "" || (ok && probed == version) {
			return bin, nil
		}
		if probed == "" {
			probed = "unknown"
		}
		l.warn("impeccable: skipping %s: it reports engine %s but this skill pins %s; delete it to stop this warning", bin, probed, version)
	}
	userHome := l.Getenv("HOME")
	if userHome == "" {
		userHome = "/nonexistent"
	}
	homeBin := filepath.Join(userHome, ".impeccable", "bin", "impeccable"+exe)
	if !probing && isExecutable(homeBin) {
		if _, ok := l.probe(ctx, homeBin); ok {
			return homeBin, nil
		}
	}
	cacheRoot := l.Getenv("IMPECCABLE_HOME")
	if cacheRoot == "" {
		cacheRoot = filepath.Join(userHome, ".impeccable")
	}
	cacheDir := filepath.Join(cacheRoot, "bin", version)
	cached := filepath.Join(cacheDir, "impeccable"+exe)
	asset := "impeccable-" + osName + "-" + arch + exe
	manifest := filepath.Join(l.ScriptsDir, "engine-checksums.txt")
	readExpected := func() (string, string) {
		x64 := ""
		if osName == "windows" && arch == "arm64" {
			x64 = l.manifestDigest(version, "impeccable-windows-x64.exe")
		}
		return l.manifestDigest(version, asset), x64
	}
	if version != "" && isExecutable(cached) {
		expected, expectedX64 := readExpected()
		size := fileSize(cached)
		if stamp, stampSize := readStamp(cached + ".verified"); stamp != "" && stampSize == size &&
			(stamp == expected || stamp == expectedX64) && !newerThan(cached, cached+".verified") {
			return cached, nil
		}
		if actual := l.HashFile(cached); actual != "" && (actual == expected || actual == expectedX64) {
			_ = os.WriteFile(cached+".verified", []byte(actual+" "+size+"\n"), 0o644)
			return cached, nil
		}
		l.warn("impeccable: cannot verify the cached engine %s against %s; not using it", cached, manifest)
	}
	if !probing && l.LookPath != nil {
		if found, err := l.LookPath("impeccable"); err == nil {
			if _, ok := l.probe(ctx, found); ok {
				return found, nil
			}
		}
	}
	setupHelp := []string{
		fmt.Sprintf("Engine %s setup needs network access and write permission to %s.", version, cacheDir),
		fmt.Sprintf("Run this launcher (%s) with engine-probe in a terminal that has those permissions, then retry the original command.", l.launcherPath()),
		"Alternatively, set IMPECCABLE_HOME to a writable cache location, or IMPECCABLE_BIN to a preinstalled engine binary.",
	}
	fail := func(lines ...string) (string, error) { return "", &EngineError{Lines: lines} }
	if probing {
		return fail()
	}
	if version == "" || osName == "unknown" || arch == "unknown" {
		return fail(fmt.Sprintf("impeccable: no engine binary for %s-%s found (looked in %s, %s, PATH).", osName, arch, bin, cached),
			fmt.Sprintf("Download impeccable-%s-%s from https://github.com/pbakaus/impeccable/releases (tag engine-v%s) into %s/bin/%s/impeccable%s (then chmod +x), or set IMPECCABLE_BIN to a preinstalled engine binary. Docs: https://impeccable.style", osName, arch, version, cacheRoot, version, exe))
	}
	base := l.Getenv("IMPECCABLE_DOWNLOAD_BASE")
	if base == "" {
		base = defaultEngineDownloadBase
	}
	url := base + "/engine-v" + version + "/" + asset
	expected, expectedX64 := readExpected()
	if expected == "" && expectedX64 == "" {
		return fail(fmt.Sprintf("impeccable: no checksum for engine %s (%s) in %s; refusing the unverified download", version, asset, manifest))
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fail(append([]string{fmt.Sprintf("impeccable: engine %s is not installed; cannot create cache directory: %s", version, cacheDir)}, setupHelp...)...)
	}
	tmp := filepath.Join(cacheDir, ".impeccable.part."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		return fail(append([]string{fmt.Sprintf("impeccable: engine %s is not installed; cannot write to cache directory: %s", version, cacheDir)}, setupHelp...)...)
	}
	fetched := false
	if expected != "" && l.fetch(ctx, url, tmp) == nil {
		fetched = true
	} else if expectedX64 != "" {
		url = base + "/engine-v" + version + "/impeccable-windows-x64.exe"
		expected = expectedX64
		fetched = l.fetch(ctx, url, tmp) == nil
	}
	if !fetched {
		_ = os.Remove(tmp)
		return fail(append([]string{fmt.Sprintf("impeccable: could not download engine %s from %s; check network access, the release URL, and curl or wget availability.", version, url)}, setupHelp...)...)
	}
	quarantined := func(p string) error {
		if !isFile(p) {
			return &EngineError{Lines: []string{fmt.Sprintf("impeccable: download completed but the file was removed before execution: %s; check your antivirus quarantine or logs. Refusing to continue; do not disable protection.", url)}}
		}
		if fileSize(p) == "0" {
			_ = os.Remove(p)
			return &EngineError{Lines: []string{fmt.Sprintf("impeccable: downloaded file is empty: %s; refusing the unverified download", url)}}
		}
		return nil
	}
	if err := quarantined(tmp); err != nil {
		return "", err
	}
	actual := l.HashFile(tmp)
	if actual == "" {
		_ = os.Remove(tmp)
		return fail(fmt.Sprintf("impeccable: cannot verify %s (no sha256 tool, or hashing failed); refusing the unverified download", url))
	}
	if actual != expected {
		_ = os.Remove(tmp)
		return fail(fmt.Sprintf("impeccable: checksum mismatch downloading %s", url))
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return fail(fmt.Sprintf("impeccable: could not make the verified download executable: %s", url))
	}
	if err := l.Rename(tmp, cached); err != nil {
		_ = os.Remove(tmp)
		return fail(fmt.Sprintf("impeccable: could not cache the verified download: %s", url))
	}
	if err := quarantined(cached); err != nil {
		return "", err
	}
	_ = os.WriteFile(cached+".verified", []byte(actual+" "+fileSize(cached)+"\n"), 0o644)
	return cached, nil
}

// Downloads a URL into a file following redirects, failing on HTTP errors and retrying twice like curl
func (l *engineLauncher) fetch(ctx context.Context, url, dest string) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if last = l.fetchOnce(ctx, url, dest); last == nil {
			return nil
		}
		var status *httpStatusError
		if errors.As(last, &status) && status.code < 500 && status.code != 408 && status.code != 429 {
			return last
		}
	}
	return last
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return "HTTP " + strconv.Itoa(e.code) }

func (l *engineLauncher) fetchOnce(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "icebath-agent")
	resp, err := l.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &httpStatusError{resp.StatusCode}
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func fileSize(p string) string {
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(info.Size(), 10)
}

// Digest and size recorded in a verified stamp file
func readStamp(p string) (string, string) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' })
	switch len(fields) {
	case 0:
		return "", ""
	case 1:
		return fields[0], ""
	}
	return fields[0], fields[1]
}

func newerThan(a, b string) bool {
	ia, err1 := os.Stat(a)
	ib, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && ia.ModTime().After(ib.ModTime())
}

// Engine binary and exported variables impeccable.cmd resolves, following its goto flow label by label
func (l *engineLauncher) resolveWindows(ctx context.Context) (string, []string, error) {
	sep := string(filepath.Separator)
	dp0 := l.ScriptsDir + sep
	probing := l.Getenv("IMPECCABLE_LAUNCHER_PROBE") != ""
	arch := "x64"
	if strings.EqualFold(l.Getenv("PROCESSOR_ARCHITECTURE"), "ARM64") {
		arch = "arm64"
	}
	if b := l.Getenv("IMPECCABLE_BIN"); b != "" && pathExists(b) {
		return b, nil, nil
	}
	version := cmdSetPLine(dp0 + "VERSION")
	bin := dp0 + "bin" + sep + "windows-" + arch + sep + "impeccable.exe"
	if pathExists(bin) {
		if probing || version == "" {
			return bin, nil, nil
		}
		probed, _ := l.probeWindows(ctx, bin, nil)
		if probed == version {
			return bin, nil, nil
		}
		if probed == "" {
			probed = "unknown"
		}
		l.warn(`impeccable: skipping "%s": it reports engine %s but this skill pins %s; delete it to stop this warning`, bin, probed, version)
	}
	homeBin := l.Getenv("USERPROFILE") + sep + ".impeccable" + sep + "bin" + sep + "impeccable.exe"
	if pathExists(homeBin) && !probing {
		if _, ok := l.probeWindows(ctx, homeBin, nil); ok {
			return homeBin, nil, nil
		}
	}
	var env []string
	cacheRoot := l.Getenv("IMPECCABLE_HOME")
	if cacheRoot == "" {
		cacheRoot = l.Getenv("USERPROFILE") + sep + ".impeccable"
		env = append(env, "IMPECCABLE_HOME="+cacheRoot)
	}
	cacheDir := cacheRoot + sep + "bin" + sep + version
	cached := cacheDir + sep + "impeccable.exe"
	asset := "impeccable-windows-" + arch + ".exe"
	manifest := dp0 + "engine-checksums.txt"
	readExpected := func() (string, string) {
		x64 := ""
		if arch == "arm64" {
			x64 = cmdManifestDigest(manifest, version, "impeccable-windows-x64.exe")
		}
		return cmdManifestDigest(manifest, version, asset), x64
	}
	vouches := func(digest, expected, x64 string) bool {
		return strings.EqualFold(digest, expected) || strings.EqualFold(digest, x64)
	}
	if version != "" && pathExists(cached) {
		expected, expectedX64 := readExpected()
		size := fileSize(cached)
		stamp, stampSize := cmdStamp(cached + ".verified")
		if stamp != "" && stampSize == size && vouches(stamp, expected, expectedX64) &&
			strings.EqualFold(newestCacheEntry(cacheDir, "impeccable.exe"), "impeccable.exe.verified") {
			return cached, env, nil
		}
		if actual := l.HashFile(cached); actual != "" && vouches(actual, expected, expectedX64) {
			_ = os.WriteFile(cached+".verified", []byte(actual+" "+size+"\r\n"), 0o666)
			return cached, env, nil
		}
		l.warn(`impeccable: cannot verify the cached engine "%s" against "%s"; not using it`, cached, manifest)
	}
	if !probing && l.LookPath != nil {
		if found, err := l.LookPath("impeccable"); err == nil || errors.Is(err, exec.ErrDot) {
			if _, ok := l.probeWindows(ctx, found, env); ok {
				return found, env, nil
			}
		}
	}
	if probing {
		return "", env, &EngineError{}
	}
	if version == "" {
		return "", env, &EngineError{Lines: []string{
			fmt.Sprintf("impeccable: no engine binary found (looked in %s, %s, PATH).", bin, cached),
			fmt.Sprintf("Download impeccable-windows-%s.exe from https://github.com/pbakaus/impeccable/releases (tag engine-v%s) and save it as %s, or set IMPECCABLE_BIN to a preinstalled engine binary. Docs: https://impeccable.style", arch, version, cached)}}
	}
	setupFailed := func(first string) error {
		return &EngineError{Lines: []string{first,
			fmt.Sprintf(`Engine %s setup needs network access and write permission to "%s".`, version, cacheDir),
			fmt.Sprintf(`Run this launcher ("%s") with engine-probe in a terminal that has those permissions, then retry the original command.`, l.launcherPath()),
			"Alternatively, set IMPECCABLE_HOME to a writable cache location, or IMPECCABLE_BIN to a preinstalled engine binary."}}
	}
	expected, expectedX64 := readExpected()
	if expected == "" && expectedX64 == "" {
		return "", env, &EngineError{Lines: []string{fmt.Sprintf(`impeccable: no checksum for engine %s (%s) in "%s"; refusing the unverified download`, version, asset, manifest)}}
	}
	curl := ""
	if l.LookPath != nil {
		if found, err := l.LookPath("curl.exe"); err == nil || errors.Is(err, exec.ErrDot) {
			curl = found
		}
	}
	if curl == "" {
		return "", env, setupFailed(fmt.Sprintf("impeccable: cannot download engine %s; curl.exe is unavailable.", version))
	}
	base := l.Getenv("IMPECCABLE_DOWNLOAD_BASE")
	if base == "" {
		base = defaultEngineDownloadBase
		env = append(env, "IMPECCABLE_DOWNLOAD_BASE="+base)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", env, setupFailed(fmt.Sprintf(`impeccable: engine %s is not installed; cannot create cache directory: "%s"`, version, cacheDir))
	}
	writeFailed := fmt.Sprintf(`impeccable: engine %s is not installed; cannot write to cache directory: "%s"`, version, cacheDir)
	staging := ""
	for attempt := 1; staging == ""; attempt++ {
		if attempt > 20 {
			return "", env, setupFailed(writeFailed)
		}
		if candidate := cacheDir + sep + ".impeccable.part." + cmdStagingSuffix(time.Now()); os.Mkdir(candidate, 0o755) == nil {
			staging = candidate
		}
	}
	discard := func() { _ = os.RemoveAll(staging) }
	part := staging + sep + "impeccable.exe"
	if err := os.WriteFile(part, nil, 0o666); err != nil {
		discard()
		return "", env, setupFailed(writeFailed)
	}
	url := base + "/engine-v" + version + "/" + asset
	fetched := expected != "" && l.curlDownload(ctx, curl, part, url, env) == nil
	if !fetched && expectedX64 != "" {
		asset, expected = "impeccable-windows-x64.exe", expectedX64
		url = base + "/engine-v" + version + "/" + asset
		fetched = l.curlDownload(ctx, curl, part, url, env) == nil
	}
	if !fetched {
		discard()
		return "", env, setupFailed(fmt.Sprintf("impeccable: could not download engine %s from %s; check network access and the release URL.", version, url))
	}
	check := func(p string) error {
		if !pathExists(p) {
			discard()
			return &EngineError{Lines: []string{fmt.Sprintf("impeccable: download completed but the file was removed before execution: %s; check your antivirus quarantine or logs. Refusing to continue; do not disable protection.", url)}}
		}
		if fileSize(p) == "0" {
			_ = os.Remove(p)
			discard()
			return &EngineError{Lines: []string{fmt.Sprintf("impeccable: downloaded file is empty: %s; refusing the unverified download", url)}}
		}
		return nil
	}
	if err := check(part); err != nil {
		return "", env, err
	}
	actual := l.HashFile(part)
	if err := check(part); err != nil {
		return "", env, err
	}
	if actual == "" {
		discard()
		return "", env, &EngineError{Lines: []string{fmt.Sprintf("impeccable: cannot verify %s (certutil could not hash it); refusing the unverified download", url)}}
	}
	if !strings.EqualFold(actual, expected) {
		discard()
		return "", env, &EngineError{Lines: []string{fmt.Sprintf("impeccable: checksum mismatch downloading %s", url)}}
	}
	if err := l.Rename(part, cached); err != nil {
		if err := check(part); err != nil {
			return "", env, err
		}
		discard()
		return "", env, &EngineError{Lines: []string{fmt.Sprintf("impeccable: could not cache the verified download: %s", url)}}
	}
	discard()
	if err := check(cached); err != nil {
		return "", env, err
	}
	_ = os.WriteFile(cached+".verified", []byte(actual+" "+fileSize(cached)+"\r\n"), 0o666)
	return cached, env, nil
}

// Engine version an engine-probe run reports to impeccable.cmd, false when it fails the handshake
func (l *engineLauncher) probeWindows(ctx context.Context, bin string, env []string) (string, bool) {
	cmd := exec.CommandContext(ctx, bin, "engine-probe")
	cmd.Env = append(append(append(os.Environ(), l.exports()...), env...), "IMPECCABLE_LAUNCHER_PROBE=1")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return parseCmdProbe(string(out))
}

// Handshake of findstr /b over the output and the second token for /f takes as the version
func parseCmdProbe(out string) (string, bool) {
	ok := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "impeccable-engine") {
			ok = true
		}
	}
	if !ok {
		return "", false
	}
	for _, fields := range cmdForLines(out) {
		if len(fields) >= 2 {
			return fields[1], true
		}
	}
	return "", true
}

// Runs curl.exe -fsSL into the staging file with its output discarded
func (l *engineLauncher) curlDownload(ctx context.Context, curl, part, url string, env []string) error {
	cmd := exec.CommandContext(ctx, curl, "-fsSL", "-o", part, url)
	cmd.Env = append(append(os.Environ(), l.exports()...), env...)
	return cmd.Run()
}

// Tokens of every line a default for /f visits, blank lines and semicolon lines skipped
func cmdForLines(text string) [][]string {
	var out [][]string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.FieldsFunc(strings.TrimSuffix(line, "\r"), func(r rune) bool { return r == ' ' || r == '\t' })
		if len(fields) == 0 || strings.HasPrefix(fields[0], ";") {
			continue
		}
		out = append(out, fields)
	}
	return out
}

// First line set /p reads from a file, empty when the file is missing or starts with a line break
func cmdSetPLine(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	line := string(data)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	return line
}

// Digest of the first manifest line whose version matches exactly and asset matches ignoring case
func cmdManifestDigest(manifest, version, asset string) string {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return ""
	}
	for _, fields := range cmdForLines(string(data)) {
		if len(fields) >= 3 && fields[0] == version && strings.EqualFold(fields[1], asset) {
			return fields[2]
		}
	}
	return ""
}

// Digest and size of the last stamp line, the way the for /f loop leaves them set
func cmdStamp(p string) (string, string) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", ""
	}
	stamp, size := "", ""
	for _, fields := range cmdForLines(string(data)) {
		stamp, size = fields[0], ""
		if len(fields) > 1 {
			size = fields[1]
		}
	}
	return stamp, size
}

// Newest file whose name starts with the prefix, the first entry of dir /b /a:-d /o:-d
func newestCacheEntry(dir, prefix string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	newest, newestTime := "", time.Time{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(strings.ToLower(e.Name()), strings.ToLower(prefix)) {
			continue
		}
		info, err := e.Info()
		if err == nil && (newest == "" || info.ModTime().After(newestTime)) {
			newest, newestTime = e.Name(), info.ModTime()
		}
	}
	return newest
}

// Staging suffix from the TIME digits and two RANDOM values the launcher concatenates
func cmdStagingSuffix(now time.Time) string {
	return fmt.Sprintf("%02d%02d%02d%02d-%d%d", now.Hour(), now.Minute(), now.Second(), now.Nanosecond()/10000000,
		rand.IntN(32768), rand.IntN(32768))
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
