package wakewordutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Installed openWakeWord bundle directory: a lib directory with the tensorflowlite_c library and a models directory with the feature models
type Bundle struct {
	dir string
	mu  sync.Mutex
	rt  *Runtime
}

// Bundle of the directory dir
func NewBundle(dir string) *Bundle { return &Bundle{dir: dir} }

// The bundle's tensorflowlite_c runtime, loaded on first success from the bundle's lib directory, else lib/<platform> beside the bundle directory
func (b *Bundle) Runtime() (*Runtime, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rt != nil {
		return b.rt, nil
	}
	path, err := findLibrary(b.dir)
	if err != nil {
		return nil, err
	}
	rt, err := OpenRuntime(path)
	if err != nil {
		return nil, err
	}
	b.rt = rt
	return rt, nil
}

// Feature front end over the bundle's mel spectrogram and embedding models
func (b *Bundle) Features() (*Features, error) {
	rt, err := b.Runtime()
	if err != nil {
		return nil, err
	}
	models := filepath.Join(b.dir, "models")
	return rt.Features(filepath.Join(models, "melspectrogram.tflite"), filepath.Join(models, "embedding_model.tflite"))
}

// Classifier from a model file over the bundle's runtime
func (b *Bundle) Model(path string) (*Model, error) {
	rt, err := b.Runtime()
	if err != nil {
		return nil, err
	}
	return rt.Model(path)
}

// Path of the tensorflowlite_c library in dir/lib, else in lib/<platform> beside dir; the error wraps ErrLibraryNotFound, and ErrUnsupportedPlatform too where no platform directory exists
func findLibrary(dir string) (string, error) {
	libDir := filepath.Join(dir, "lib")
	if path := firstLibrary(libDir); path != "" {
		return path, nil
	}
	platform, ok := platformDir()
	if !ok {
		return "", fmt.Errorf("%w in %s: %w", ErrLibraryNotFound, libDir, ErrUnsupportedPlatform)
	}
	platformLib := filepath.Join(filepath.Dir(filepath.Clean(dir)), "lib", platform)
	if path := firstLibrary(platformLib); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("%w in %s or %s", ErrLibraryNotFound, libDir, platformLib)
}

// First file in dir named like *tensorflowlite_c.*, empty when there is none
func firstLibrary(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if ok, _ := filepath.Match("*tensorflowlite_c.*", entry.Name()); ok {
			return filepath.Join(dir, entry.Name())
		}
	}
	return ""
}

// Directory name of this platform's tensorflowlite_c build, false where bundles carry none
func platformDir() (string, bool) {
	goos := runtime.GOOS
	if goos == "android" {
		goos = "linux"
	}
	switch goos + "/" + runtime.GOARCH {
	case "linux/amd64", "linux/arm64", "windows/amd64", "darwin/arm64":
		return goos + "_" + runtime.GOARCH, true
	}
	return "", false
}
