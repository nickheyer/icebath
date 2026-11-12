//go:build !windows && !darwin

package files

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

// Shows the item selected in the file manager over FileManager1, else opens its folder with xdg-open
func ShowItemInFolder(fullPath string) {
	if conn, err := dbus.SessionBus(); err == nil {
		obj := conn.Object("org.freedesktop.FileManager1", "/org/freedesktop/FileManager1")
		call := obj.Call("org.freedesktop.FileManager1.ShowItems", 0, []string{pathToFileURL(fullPath)}, "")
		if call.Err == nil {
			return
		}
	}
	cmd := exec.Command("xdg-open", filepath.Dir(fullPath))
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

// Opens the path in its default application with xdg-open, the error text on failure and empty on success
func OpenPath(fullPath string) string {
	cmd := exec.Command("xdg-open", fullPath)
	if err := cmd.Run(); err != nil {
		return "Failed to open path"
	}
	return ""
}

// Moves the item to the freedesktop trash of its volume with its trashinfo record
func TrashItem(fullPath string) error {
	abs := resolvePath(fullPath)
	st, err := os.Lstat(abs)
	if err != nil {
		return nodeFsError(err, "lstat", abs)
	}
	trashDir, topdir, err := trashDirFor(abs, st)
	if err != nil {
		return err
	}
	filesDir, infoDir := filepath.Join(trashDir, "files"), filepath.Join(trashDir, "info")
	for _, d := range []string{filesDir, infoDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("Failed to move %s to trash: %w", abs, err)
		}
	}
	recorded := abs
	if topdir != "" {
		rel, err := filepath.Rel(topdir, abs)
		if err == nil {
			recorded = rel
		}
	}
	info := "[Trash Info]\nPath=" + trashInfoEscape(recorded) + "\nDeletionDate=" + time.Now().Format("2006-01-02T15:04:05") + "\n"
	base := filepath.Base(abs)
	for i := 1; ; i++ {
		name := uniqueTrashName(base, i)
		infoFile := filepath.Join(infoDir, name+".trashinfo")
		f, err := os.OpenFile(infoFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return fmt.Errorf("Failed to move %s to trash: %w", abs, err)
		}
		_, werr := f.WriteString(info)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			os.Remove(infoFile)
			return fmt.Errorf("Failed to move %s to trash: %w", abs, errors.Join(werr, cerr))
		}
		if err := os.Rename(abs, filepath.Join(filesDir, name)); err != nil {
			os.Remove(infoFile)
			return fmt.Errorf("Failed to move %s to trash: %w", abs, err)
		}
		return nil
	}
}

// Name of the i-th try for a trashed basename, a dot number before the first extension as GIO numbers them
func uniqueTrashName(base string, i int) string {
	if i == 1 {
		return base
	}
	if dot := strings.IndexByte(base, '.'); dot > 0 {
		return base[:dot] + "." + strconv.Itoa(i) + base[dot:]
	}
	return base + "." + strconv.Itoa(i)
}

// Percent escaping of a trashinfo Path, slashes kept
func trashInfoEscape(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func deviceOf(st os.FileInfo) (uint64, bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(sys.Dev), true
}

// Home trash when the item shares the data home's volume, else the volume's own trash
func trashDirFor(abs string, st os.FileInfo) (string, string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(userHome(), ".local", "share")
	}
	homeTrash := filepath.Join(dataHome, "Trash")
	itemDev, ok := deviceOf(st)
	if !ok {
		return homeTrash, "", nil
	}
	os.MkdirAll(dataHome, 0o700)
	if hst, err := os.Stat(dataHome); err == nil {
		if hdev, ok := deviceOf(hst); ok && hdev == itemDev {
			return homeTrash, "", nil
		}
	}
	topdir := filepath.Dir(abs)
	for {
		parent := filepath.Dir(topdir)
		if parent == topdir {
			break
		}
		pst, err := os.Stat(parent)
		if err != nil {
			break
		}
		if pdev, ok := deviceOf(pst); !ok || pdev != itemDev {
			break
		}
		topdir = parent
	}
	uid := strconv.Itoa(os.Getuid())
	shared := filepath.Join(topdir, ".Trash")
	if sst, err := os.Lstat(shared); err == nil && sst.IsDir() && sst.Mode()&os.ModeSticky != 0 && sst.Mode()&os.ModeSymlink == 0 {
		return filepath.Join(shared, uid), topdir, nil
	}
	return filepath.Join(topdir, ".Trash-"+uid), topdir, nil
}
