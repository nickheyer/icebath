package portaudioutil

import (
	"os"
	"path/filepath"
	"sync"

	dlutil "github.com/nickheyer/icebath/pkg/utils/dl"
)

var (
	loadMu      sync.Mutex
	library     *dlutil.Library
	initialized bool
)

// Loads and binds the PortAudio library without initializing its host APIs, the check for whether PortAudio is installed
func Load() error {
	loadMu.Lock()
	defer loadMu.Unlock()
	return load()
}

// PortAudio loaded and initialized once per process, what it and the host libraries print while probing devices routed into the debug log
func Open() (Audio, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if initialized {
		return audio{}, nil
	}
	if err := load(); err != nil {
		return nil, err
	}
	var code int32
	quietly(func() { code = paInitialize() })
	if err := check(code, "initialize portaudio"); err != nil {
		return nil, err
	}
	if streamCB == 0 {
		streamCB, streamCBErr = dlutil.NewCallback(dispatch)
		if streamCB == 0 && streamCBErr == nil {
			streamCBErr = dlutil.ErrNoCallback
		}
	}
	initialized = true
	return audio{}, nil
}

// Binds the first PortAudio library that opens and has every function, trying the platform's usual names and paths and then the loader search, and hooks its diagnostics into the log; a LibraryError carrying the first failure when none does
func load() error {
	if library != nil {
		return nil
	}
	if !dlutil.Supported {
		return ErrUnsupported
	}
	var first error
	tried := map[string]bool{}
	try := func(candidate string) bool {
		if candidate == "" || tried[candidate] {
			return false
		}
		tried[candidate] = true
		if filepath.Base(candidate) != candidate && !isFile(candidate) {
			return false
		}
		lib, err := dlutil.Open(candidate, false)
		if err == nil {
			err = bind(lib)
		}
		if err != nil {
			if first == nil {
				first = err
			}
			return false
		}
		library = lib
		installLogHooks(lib)
		return true
	}
	for _, candidate := range libraryCandidates() {
		if try(candidate) {
			return nil
		}
	}
	if try(dlutil.FindLibrary("portaudio")) {
		return nil
	}
	if first == nil {
		first = errNoLibrary
	}
	return &LibraryError{Err: first}
}

// Whether a path names a regular file or a link to one
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
