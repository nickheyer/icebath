package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// Home skeleton that cannot be used, distinct from an invalid YAML document
type HomeInitializationError struct {
	Message string
	Err     error
}

func (e *HomeInitializationError) Error() string { return e.Message }

func (e *HomeInitializationError) Unwrap() error { return e.Err }

// Subdirectories every home carries
var HomeSubdirs = []string{"cron", "sessions", "logs", "logs/curator", "memories", "pairing", "hooks", "image_cache", "audio_cache", "skills"}

var (
	homeEnsuredMu sync.Mutex
	homeEnsured   = map[string]bool{}
)

// Symlinked components of a path from the filesystem root down to the path itself
func directoryLinks(path string) []string {
	path = filepath.Clean(path)
	var chain []string
	for p := path; ; p = filepath.Dir(p) {
		chain = append([]string{p}, chain...)
		if filepath.Dir(p) == p {
			break
		}
	}
	var links []string
	for _, p := range chain {
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			links = append(links, p)
		}
	}
	return links
}

// Links at or below the home, the ones that hand its permissions to the operator
func operatorOwnedLinks(links []string, h string) []string {
	h = filepath.Clean(h)
	var out []string
	for _, link := range links {
		if link == h || strings.HasPrefix(link, h+string(filepath.Separator)) {
			out = append(out, link)
		}
	}
	return out
}

// Python OSError text of an errno failure on a path
func pyOSErrorText(err error) string {
	var pe *fs.PathError
	var errno syscall.Errno
	if errors.As(err, &pe) && errors.As(pe.Err, &errno) {
		return fmt.Sprintf("[Errno %d] %s: '%s'", int(errno), pyStrerror(errno), pe.Path)
	}
	return err.Error()
}

// C library strerror text of an errno with the first letter capitalised as Python prints it
func pyStrerror(errno syscall.Errno) string {
	text := errno.Error()
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// Ensures a home directory exists and is private without materialising a missing link target
func ensureDirectory(path string, create, secure bool, h string) error {
	links := directoryLinks(path)
	details := make([]string, 0, len(links))
	fail := func(err error) error {
		msg := fmt.Sprintf("Cannot initialize Icebath directory %s: %s. ", path, pyOSErrorText(err))
		if len(details) > 0 {
			msg += "Directory links: " + strings.Join(details, "; ") + ". "
		}
		msg += "Check the directory/link target, mount availability and access permissions; " +
			"restore the mount or repair the link before retrying. " +
			"Icebath has not replaced the link or created its missing target."
		return &HomeInitializationError{Message: msg, Err: err}
	}
	for _, link := range links {
		target, err := os.Readlink(link)
		if err != nil {
			return fail(err)
		}
		details = append(details, link+" -> "+target)
	}
	for _, link := range links {
		if st, err := os.Stat(link); err != nil || !st.IsDir() {
			return fail(&fs.PathError{Op: "stat", Path: "Directory link is unavailable: " + link, Err: syscall.ENOENT})
		}
	}
	if create {
		if err := os.MkdirAll(path, 0o777); err != nil {
			return fail(err)
		}
	} else if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return fail(&fs.PathError{Op: "stat", Path: "Required directory does not exist: " + path, Err: syscall.ENOENT})
	}
	if secure && len(operatorOwnedLinks(links, h)) == 0 {
		SecureDir(h, path)
	}
	return nil
}

// Creates the home skeleton, private unless managed, and seeds the default soul
func InitializeHome(h string, subdirs []string, ensured map[string]bool) error {
	managed := ManagedSystem(h) != ""
	restore := setManagedUmask(managed)
	err := func() error {
		defer restore()
		if err := ensureDirectory(h, !managed, !managed, h); err != nil {
			return err
		}
		required := subdirs
		if managed {
			required = []string{"cron", "sessions", "logs", "memories"}
		}
		for _, sub := range required {
			if err := ensureDirectory(filepath.Join(h, filepath.FromSlash(sub)), !managed, !managed, h); err != nil {
				return err
			}
		}
		if managed {
			if err := ensureDirectory(filepath.Join(h, "logs", "curator"), true, false, h); err != nil {
				return err
			}
		}
		if err := seedSoul(h); err != nil {
			return &HomeInitializationError{Message: fmt.Sprintf("Cannot initialize Icebath home %s: %s. "+
				"Check storage availability and access permissions.", h, pyOSErrorText(err)), Err: err}
		}
		return nil
	}()
	if err != nil {
		return err
	}
	ensured[h] = true
	if resolved, rerr := filepath.EvalSymlinks(h); rerr == nil {
		ensured[resolved] = true
	}
	return nil
}

// Ensures the home skeleton once per home path, refusing a deleted named profile
func EnsureIcebathHome(h string) error {
	if err := home.AssertProfileLive(h); err != nil {
		return err
	}
	homeEnsuredMu.Lock()
	defer homeEnsuredMu.Unlock()
	if homeEnsured[h] {
		if st, err := os.Stat(h); err == nil && st.IsDir() {
			return nil
		}
	}
	return InitializeHome(h, HomeSubdirs, homeEnsured)
}

// Config issue for a load failure, storage problems kept apart from a broken config file
func ConfigLoadIssue(err error) ConfigIssue {
	var hie *HomeInitializationError
	if errors.As(err, &hie) || IsOSError(err) {
		return ConfigIssue{Severity: "error", Message: "Icebath storage is unavailable: " + err.Error(),
			Hint: "Check the reported path, link target, mount and permissions; keep config.yaml unchanged."}
	}
	return ConfigIssue{Severity: "error", Message: "Could not load config.yaml", Hint: "Run 'icebath setup' to create a valid config"}
}

// Reports whether chmod is skipped for a container or an explicit opt out
func containerOrChmodSkipped() bool {
	if os.Getenv("ICEBATH_CONTAINER") != "" || os.Getenv("ICEBATH_SKIP_CHMOD") != "" {
		return true
	}
	return hostinfo.IsContainer()
}

// Applies ICEBATH_UID and ICEBATH_GID ownership when set, failures ignored
func chownToIcebathUID(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	envInt := func(name string) int {
		n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
		if err != nil {
			return -1
		}
		return n
	}
	uid, gid := envInt("ICEBATH_UID"), envInt("ICEBATH_GID")
	if uid < 0 && gid < 0 {
		return
	}
	_ = os.Chown(path, uid, gid)
}

// Owner only directory unless managed, container defaults or ICEBATH_HOME_MODE say otherwise
func SecureDir(h, path string) {
	if ManagedSystem(h) != "" {
		return
	}
	explicit := strings.TrimSpace(os.Getenv("ICEBATH_HOME_MODE"))
	if containerOrChmodSkipped() && explicit == "" {
		chownToIcebathUID(path)
		return
	}
	text := explicit
	if text == "" {
		text = "700"
	}
	mode, err := strconv.ParseUint(text, 8, 32)
	if err != nil {
		mode = 0o700
	}
	fm := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		fm |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		fm |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		fm |= os.ModeSticky
	}
	_ = os.Chmod(path, fm)
	chownToIcebathUID(path)
}

// Owner only config file unless managed or in a container
func SecureFile(h, path string) {
	if ManagedSystem(h) != "" || containerOrChmodSkipped() {
		return
	}
	if _, err := os.Stat(path); err == nil {
		_ = os.Chmod(path, 0o600)
	}
}
