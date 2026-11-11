package installidentity

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const fileName = "install_id"

var installIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Process cached identity and the root it was read under
type Cache struct {
	Root  string
	Value string
}

var (
	cacheLock       sync.Mutex
	publicationLock sync.Mutex
	processCache    = &Cache{}
)

// UnicodeDecodeError reading an identity file that is not UTF-8
type DecodeError struct{ Path string }

func (e *DecodeError) Error() string {
	return "'utf-8' codec can't decode the identity file " + e.Path
}

// Python class name of the error
func (e *DecodeError) PyTypeName() string { return "UnicodeDecodeError" }

// Holds the cross process identity lock under root while fn runs
func installIDFileLock(root string, fn func() (string, error)) (string, error) {
	f, err := os.OpenFile(filepath.Join(root, ".install_id.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if runtime.GOOS == "windows" {
		st, err := f.Stat()
		if err != nil {
			return "", err
		}
		if st.Size() == 0 {
			if _, err := f.Write([]byte{0}); err != nil {
				return "", err
			}
			if err := f.Sync(); err != nil {
				return "", err
			}
		}
		if _, err := f.Seek(0, 0); err != nil {
			return "", err
		}
	}
	if err := pyfs.LockFile(f); err != nil {
		return "", err
	}
	value, fnErr := fn()
	if runtime.GOOS == "windows" {
		if _, err := f.Seek(0, 0); err != nil && fnErr == nil {
			return "", err
		}
	}
	if err := pyfs.UnlockFile(f); err != nil && fnErr == nil {
		return "", err
	}
	return value, fnErr
}

// Stripped lowercase text of a utf-8-sig file, a DecodeError for bytes that are not UTF-8
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	if !utf8.Valid(data) {
		return "", &DecodeError{path}
	}
	return pystr.Lower(pystr.Strip(string(data))), nil
}

// Valid identity or empty plus whether to mint, minting for a missing or malformed file only
func readExisting(path string) (string, bool) {
	existing, err := readText(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	if installIDRE.MatchString(existing) {
		return existing, false
	}
	return "", true
}

// Random 32 hex digit identity as uuid4 hex spells it
func mintUUIDHex() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// Reads or atomically mints the opaque install identity under root, empty when neither readable nor persistable
func ReadOrCreateInstallID(ctx context.Context, root string) (string, error) {
	return readOrCreate(ctx, root, mintUUIDHex)
}

// Identity under root with mint producing a fresh value under the cross process lock
func readOrCreate(ctx context.Context, root string, mint func() string) (string, error) {
	if root == "" {
		root = home.Root()
	}
	path := filepath.Join(root, fileName)
	publicationLock.Lock()
	defer publicationLock.Unlock()
	if existing, _ := readExisting(path); existing != "" {
		return existing, nil
	}
	if _, err := home.Mkdir(ctx, root); err != nil {
		return osFailure(err)
	}
	value, err := installIDFileLock(root, func() (string, error) {
		existing, minting := readExisting(path)
		if !minting {
			return existing, nil
		}
		content := mint() + "\n"
		if runtime.GOOS == "windows" {
			content = strings.ReplaceAll(content, "\n", "\r\n")
		}
		mode := os.FileMode(0o600)
		if err := pyfs.AtomicWriteBytes(ctx, path, []byte(content), ".install_id-", &mode, true); err != nil {
			return "", err
		}
		committed, err := readText(path)
		if err != nil {
			return "", err
		}
		if installIDRE.MatchString(committed) {
			return committed, nil
		}
		return "", nil
	})
	if err != nil {
		return osFailure(err)
	}
	return value, nil
}

// Empty identity for an OSError, the error itself for anything else
func osFailure(err error) (string, error) {
	if pypath.IsOSError(err) {
		return "", nil
	}
	return "", err
}

// Process cached stable identity for the active root, cache nil using the process cache
func GetInstallID(ctx context.Context, cache *Cache) (string, error) {
	root := home.Root()
	if cache == nil {
		cache = processCache
	}
	cached := func() string {
		if cache.Value != "" && (cache.Root == "" || cache.Root == root) {
			return cache.Value
		}
		return ""
	}
	cacheLock.Lock()
	defer cacheLock.Unlock()
	if value := cached(); value != "" {
		return value, nil
	}
	value, err := ReadOrCreateInstallID(ctx, root)
	if err != nil {
		return "", err
	}
	if value != "" {
		cache.Root = root
		cache.Value = value
	}
	return value, nil
}
