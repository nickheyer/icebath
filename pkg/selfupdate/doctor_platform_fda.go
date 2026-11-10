package selfupdate

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Seams over the host platform, the home directory and the TCC probe, the names the FDA tests patch
var (
	doctorSysPlatform = runtime.GOOS
	doctorUserHomeDir = pypath.PathHome
	doctorListDir     = func(path string) error { _, err := os.ReadDir(path); return err }
)

// CheckMacosFullDiskAccess reports the Full Disk Access grant that silences every macOS folder prompt, silent elsewhere
func CheckMacosFullDiskAccess(w io.Writer) error {
	if doctorSysPlatform != "darwin" {
		return nil
	}
	userHome, err := doctorUserHomeDir()
	if err != nil {
		return err
	}
	err = doctorListDir(filepath.Join(userHome, "Library", "Application Support", "com.apple.TCC"))
	switch {
	case err == nil:
		doctorCheckOK(w, "macOS Full Disk Access granted", "(no per-folder permission prompts will occur)")
	case errors.Is(err, fs.ErrPermission):
		doctorCheckInfo(w, "One switch silences all macOS folder prompts: grant your terminal app Full Disk Access and Icebath "+
			"will never trip per-folder dialogs (Desktop/Downloads/Documents/...) again. Open: System Settings → "+
			"Privacy & Security → Full Disk Access — or run:\n"+
			"      open \"x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles\"\n"+
			"    then enable your terminal (and Icebath.app if you use Desktop), and restart them once. "+
			"With Icebath's stable signing identities the grant survives every update.")
	}
	return nil
}
