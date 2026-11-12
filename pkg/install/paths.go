package install

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Directory name of the install tree under the home
const InstallDirName = "icebath-agent"

// Bootstrap complete marker file name inside the install tree
const BootstrapMarkerName = ".icebath-bootstrap-complete"

// Home the installer works in, the ICEBATH_HOME override first then the platform default
func Home() string {
	if v := os.Getenv(home.EnvHome); pystr.Strip(v) != "" {
		return v
	}
	return home.PlatformDefault()
}

// Profile tree root that holds the host wide update marker
func Root() string { return home.Root() }

// Install tree of a home
func InstallRootOf(homeDir string) string { return filepath.Join(homeDir, InstallDirName) }

func LogDir() string { return filepath.Join(Home(), "logs") }

func LogPath() string { return filepath.Join(LogDir(), "bootstrap-installer.log") }

// Stable copy of the installer the desktop relaunches with --update
func InstallerDest() string {
	name := "hermes-setup"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(Home(), name)
}

// Host wide update marker under the profile tree root
func UpdateMarkerPath() string { return filepath.Join(Root(), updaterecord.MarkerName) }

// Bootstrap complete marker of an install tree
func BootstrapMarkerPath(installRoot string) string {
	return filepath.Join(installRoot, BootstrapMarkerName)
}

// Copies the running installer, the AppImage file itself when run from one, to the stable home location unless already there
func CopySelfToHome() error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if image := os.Getenv("APPIMAGE"); image != "" && isFile(image) {
		src = image
	}
	dest := InstallerDest()
	if samePath(src, dest) {
		slog.Info("installer already at destination; skipping self-copy", "dest", dest)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	if err := copyFile(src, dest, 0o755); err != nil {
		return err
	}
	repairMacOSInstallerHelper(dest)
	slog.Info("copied installer to ICEBATH_HOME", "src", src, "dest", dest)
	return nil
}

// Clears quarantine and ad hoc signs the staged macOS helper when its signature does not verify
func repairMacOSInstallerHelper(path string) {
	if runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("/usr/bin/xattr", "-cr", path).Run()
	if exec.Command("/usr/bin/codesign", "--verify", path).Run() != nil {
		_ = exec.Command("/usr/bin/codesign", "--force", "--sign", "-", path).Run()
	}
}

// Reports two paths naming one file, resolved when both resolve, compared verbatim otherwise
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			return strings.EqualFold(ra, rb)
		}
		return ra == rb
	}
	return a == b
}

// Copies src to dest through a sibling temp file renamed into place
func copyFile(src, dest string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".tmp-" + itoa(os.Getpid())
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return replaceFile(tmp, dest)
}

// Renames src over dest, moving a dest Windows still holds aside first
func replaceFile(src, dest string) error {
	err := os.Rename(src, dest)
	if err == nil || runtime.GOOS != "windows" {
		if err != nil {
			os.Remove(src)
		}
		return err
	}
	aside := dest + ".old-" + itoa(os.Getpid())
	if moveErr := os.Rename(dest, aside); moveErr != nil {
		os.Remove(src)
		return err
	}
	if err := os.Rename(src, dest); err != nil {
		_ = os.Rename(aside, dest)
		os.Remove(src)
		return err
	}
	_ = os.Remove(aside)
	return nil
}

// Opens the bootstrap installer log under the home logs dir and routes slog there at the ICEBATH_BOOTSTRAP_LOG level
func InitLogging() (io.Closer, error) {
	dir := LogDir()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	level := slog.LevelInfo
	switch strings.ToLower(pystr.Strip(os.Getenv("ICEBATH_BOOTSTRAP_LOG"))) {
	case "trace", "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level})))
	return f, nil
}
