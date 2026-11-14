package azure

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

const dpapiDescription = "python_data"

type protectedFile struct{ path string }

func openCacheStore(context.Context) (cacheStore, error) {
	base, ok := os.LookupEnv("LOCALAPPDATA")
	if !ok {
		return nil, errors.New("A persistent cache is not available in this environment.")
	}
	path := filepath.Join(base, ".IdentityService", sharedCacheName)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	return protectedFile{path: path}, nil
}

func (p protectedFile) location() string { return p.path }

func (p protectedFile) modified() (time.Time, bool, error) { return fileModified(p.path) }

func (p protectedFile) load(context.Context) ([]byte, bool, error) {
	sealed, found, err := readPlain(p.path)
	if err != nil || !found {
		return nil, found, err
	}
	plain, err := unprotectData(sealed)
	if err != nil {
		slog.Error("DPAPI error likely caused by file content not previously encrypted. App developer should migrate by calling save(plaintext) first.", "error", err)
		return nil, false, err
	}
	if !utf8.Valid(plain) {
		return nil, false, errors.New("the protected token cache is not UTF-8 text")
	}
	return plain, true, nil
}

func (p protectedFile) save(_ context.Context, data []byte) error {
	sealed, err := protectData(data)
	if err != nil {
		return err
	}
	return writeInPlace(p.path, sealed)
}

func dataBlob(data []byte) *windows.DataBlob {
	if len(data) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

func takeBlob(out windows.DataBlob) []byte {
	if out.Data == nil {
		return nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...)
}

func protectData(data []byte) ([]byte, error) {
	description, err := windows.UTF16PtrFromString(dpapiDescription)
	if err != nil {
		return nil, err
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(dataBlob(data), description, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(out), nil
}

func unprotectData(data []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(dataBlob(data), nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(out), nil
}
