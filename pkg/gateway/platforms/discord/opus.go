package discord

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

// Process wide libopus handle shared by every voice connection
var opus = &opusCodec{}

// Loaded libopus library, nil until a load succeeds
type opusCodec struct {
	mu  sync.Mutex
	lib *pyctypes.CDLL
}

// Loads libopus from a name or path, keeping the handle once the encoder symbol resolves
func (o *opusCodec) load(path string) error {
	if !pyctypes.Supported {
		return &pyctypes.OSError{Msg: "shared library loading is unavailable on this platform"}
	}
	lib, err := pyctypes.LoadLibrary(path, false)
	if err != nil {
		return err
	}
	if _, err := lib.Symbol("opus_encoder_create"); err != nil {
		return err
	}
	o.mu.Lock()
	o.lib = lib
	o.mu.Unlock()
	return nil
}

// Whether a libopus handle is loaded
func (o *opusCodec) isLoaded() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lib != nil
}

// Bundled Windows DLL, then the system library search, then the Homebrew paths on macOS
func opusCandidates() []string {
	var candidates []string
	if bundled := findDiscordWindowsBundledOpus(); bundled != "" {
		candidates = append(candidates, bundled)
	}
	if opusPath := pyctypes.FindLibrary("opus"); opusPath != "" {
		candidates = append(candidates, opusPath)
	} else if runtime.GOOS == "darwin" {
		for _, hp := range []string{"/opt/homebrew/lib/libopus.dylib", "/usr/local/lib/libopus.dylib"} {
			if st, err := os.Stat(hp); err == nil && st.Mode().IsRegular() {
				candidates = append(candidates, hp)
				break
			}
		}
	}
	return candidates
}

// Loads opus from the candidate paths, warning once when none loads
func loadOpusCodec() {
	for _, opusPath := range opusCandidates() {
		if err := opus.load(opusPath); err != nil {
			slog.Warn(fmt.Sprintf("Opus codec found at %s but failed to load", opusPath))
			continue
		}
		if opus.isLoaded() {
			break
		}
	}
	if !opus.isLoaded() {
		slog.Warn("Opus codec not found — voice channel playback disabled")
	}
}

// Opus DLL shipped beside the running executable on Windows, empty elsewhere or when absent
func findDiscordWindowsBundledOpus() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	target := "x86"
	if 32<<(^uintptr(0)>>63) > 32 {
		target = "x64"
	}
	bundled := filepath.Join(filepath.Dir(exe), "bin", "libopus-0."+target+".dll")
	if st, err := os.Stat(bundled); err == nil && st.Mode().IsRegular() {
		return bundled
	}
	return ""
}

// Loads the first libopus candidate that resolves the encoder, the last failure when none does
func LoadOpusLibrary() error {
	err := error(&pyctypes.OSError{Msg: "libopus not found"})
	for _, opusPath := range opusCandidates() {
		if err = (&opusCodec{}).load(opusPath); err == nil {
			return nil
		}
	}
	return err
}
