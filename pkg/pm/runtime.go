//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Environment of an isolated runtime child, ambient interpreter selection stripped and the home and store pinned
func RuntimeEnvironment(ctx context.Context) (map[string]string, error) {
	env, err := BaseEnvironment(nil)
	if err != nil {
		return nil, err
	}
	store, err := StoreRoot(ctx)
	if err != nil {
		return nil, err
	}
	env[home.EnvHome] = home.Dir(ctx)
	env[EnvRuntimeDir] = Native(store)
	return env, nil
}

// Absolute spelling of a path without collapsing dot dot or resolving links, like Path absolute
func absoluteUnresolved(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	wd, err := os.Getwd()
	if err != nil {
		return path
	}
	return wd + string(filepath.Separator) + path
}

// Creates a file or bumps its mtime
func touchFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}
