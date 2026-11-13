//go:build !linux && !freebsd && !netbsd && !openbsd && !dragonfly && !darwin && !windows

package safestorage

import "errors"

// Platforms without an Electron key store never yield a key
type platformKeys struct{}

func newPlatformKeys(Options) *platformKeys { return &platformKeys{} }

func (k *platformKeys) available(bool) bool { return false }

func (k *platformKeys) encrypt([]byte) ([]byte, error) { return nil, errors.New("no key store") }

func (k *platformKeys) decrypt([]byte) ([]byte, error) { return nil, errors.New("no key store") }
