package config

import (
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Writes data as two space indented JSON through a synced temp file keeping the existing mode and owner
func AtomicJSONWrite(path string, data any) error {
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
	return atomicPreservingWrite(path, []byte(text), "."+stem+"_")
}

// Atomic json.dump with the given dump options, ensure_ascii escapes only for a payload that is not valid UTF-8
func AtomicJSONWriteWith(path string, data any, opts ...pyjson.Option) error {
	text, err := pyjson.Dumps(data, append(append([]pyjson.Option{}, opts...), pyjson.EnsureASCII(false))...)
	if err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		if text, err = pyjson.Dumps(data, append(append([]pyjson.Option{}, opts...), pyjson.EnsureASCII(true))...); err != nil {
			return err
		}
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicPreservingWrite(path, []byte(text), "."+stem+"_")
}
