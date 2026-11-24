package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Mode carrying atomic JSON write that also syncs the resolved target directory so the rename survives power loss
func AtomicJSONWriteModeSyncDir(ctx context.Context, path string, data any, mode os.FileMode, opts ...jsonutil.Option) error {
	text, err := atomicJSONText(data, opts)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicWriteMode(ctx, path, []byte(text), "."+stem+"_", mode, true, true)
}

// Writes data as two space indented JSON through a synced temp file keeping the existing mode and owner
func AtomicJSONWrite(ctx context.Context, path string, data any) error {
	text, err := jsonutil.Encode(data, jsonutil.Indent(2))
	if err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		if text, err = jsonutil.Encode(data, jsonutil.Indent(2), jsonutil.ASCII()); err != nil {
			return err
		}
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicPreservingWrite(ctx, path, []byte(text), "."+stem+"_")
}

// Atomic json.dump with the given dump options, ensure_ascii escapes only for a payload that is not valid UTF-8
func AtomicJSONWriteWith(ctx context.Context, path string, data any, opts ...jsonutil.Option) error {
	text, err := atomicJSONText(data, opts)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicPreservingWrite(ctx, path, []byte(text), "."+stem+"_")
}

// Atomic json.dump with the given dump options whose temp file and target carry the given mode
func AtomicJSONWriteMode(ctx context.Context, path string, data any, mode os.FileMode, opts ...jsonutil.Option) error {
	text, err := atomicJSONText(data, opts)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicWriteMode(ctx, path, []byte(text), "."+stem+"_", mode, true, false)
}

// JSON text of data under the dump options, raw UTF-8 unless the payload holds surrogate escapes
func atomicJSONText(data any, opts []jsonutil.Option) (string, error) {
	text, err := jsonutil.Encode(data, append(append([]jsonutil.Option{}, opts...))...)
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(text) {
		return jsonutil.Encode(data, append(append([]jsonutil.Option{}, opts...), jsonutil.ASCII())...)
	}
	return text, nil
}
