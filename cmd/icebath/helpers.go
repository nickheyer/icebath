package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/commands"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// The value an optional argument flag takes when given without a value
const flagPresent = commands.FlagPresent

// Python truthiness of a value
func truthy(v any) bool { return anyutil.IsTrue(v) }

// Value of a key on a row, nil when the row or key is absent
func rowValue(row map[string]any, key string) any {
	if row == nil {
		return nil
	}
	return row[key]
}

// Resolved absolute path with symlinks followed, the input when resolution fails
func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		if abs, err := filepath.Abs(r); err == nil {
			return abs
		}
		return r
	}
	return p
}

func sortStrings(s []string) { sort.Strings(s) }

func errorsAs(err error, target any) bool { return errors.As(err, target) }

func slogWarn(msg string) { pkgLog().Warn(msg) }

func slogError(msg string, err error) { pkgLog().Error(fmt.Sprintf("%s: %v", msg, err)) }

// Prints a line to stderr
func printErr(msg string) { fmt.Fprintln(os.Stderr, msg) }

// Truthiness of an argparse dest, False when absent or None
func nsBool(ns map[string]any, key string) bool { return truthy(ns[key]) }

// String dest of an argparse namespace, empty when absent or None
func nsString(ns map[string]any, key string) string { return anyutil.String(ns[key]) }

// Integer dest of an argparse namespace, zero when absent or not an int
func nsInt(ns map[string]any, key string) int {
	v, _ := ns[key].(int)
	return v
}
