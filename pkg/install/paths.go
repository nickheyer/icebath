package install

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Install tree of a home
func InstallRootOf(homeDir string) string { return filepath.Join(homeDir, InstallDirName) }

// Bootstrap complete marker of an install tree
func BootstrapMarkerPath(installRoot string) string {
	return filepath.Join(installRoot, BootstrapMarkerName)
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

// Whether p names an existing regular file
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func itoa(n int) string { return strconv.Itoa(n) }
