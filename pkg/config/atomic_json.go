package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Mode carrying atomic JSON write that also syncs the resolved target directory so the rename survives power loss
func AtomicJSONWriteModeSyncDir(ctx context.Context, path string, data any, mode os.FileMode, opts ...pyjson.Option) error {
	text, err := atomicJSONText(data, opts)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicWriteMode(ctx, path, []byte(text), "."+stem+"_", mode, true, true)
}

// Writes data as two space indented JSON through a synced temp file keeping the existing mode and owner
func AtomicJSONWrite(ctx context.Context, path string, data any) error {
	text, err := pyjson.Dumps(data, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		if text, err = pyjson.Dumps(data, pyjson.Indent(2), pyjson.EnsureASCII(true)); err != nil {
			return err
		}
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicPreservingWrite(ctx, path, []byte(text), "."+stem+"_")
}

// Atomic json.dump with the given dump options, ensure_ascii escapes only for a payload that is not valid UTF-8
func AtomicJSONWriteWith(ctx context.Context, path string, data any, opts ...pyjson.Option) error {
	text, err := atomicJSONText(data, opts)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicPreservingWrite(ctx, path, []byte(text), "."+stem+"_")
}

// Atomic json.dump with the given dump options whose temp file and target carry the given mode
func AtomicJSONWriteMode(ctx context.Context, path string, data any, mode os.FileMode, opts ...pyjson.Option) error {
	text, err := atomicJSONText(data, opts)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicWriteMode(ctx, path, []byte(text), "."+stem+"_", mode, true, false)
}

// JSON text of data under the dump options, raw UTF-8 unless the payload holds surrogate escapes
func atomicJSONText(data any, opts []pyjson.Option) (string, error) {
	text, err := pyjson.Dumps(data, append(append([]pyjson.Option{}, opts...), pyjson.EnsureASCII(false))...)
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(text) {
		return pyjson.Dumps(data, append(append([]pyjson.Option{}, opts...), pyjson.EnsureASCII(true))...)
	}
	return text, nil
}

// JSON object at path as a map, empty when the file is missing, unreadable, malformed or not an object
func ReadJSONOrEmpty(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return map[string]any{}
	}
	switch v := parsed.(type) {
	case *pyjson.Object:
		out := make(map[string]any, len(v.Keys))
		for _, k := range v.Keys {
			out[k] = v.Vals[k]
		}
		return out
	case map[string]any:
		return v
	}
	return map[string]any{}
}
