package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/lsp"
)

// The value an optional argument flag takes when given without a value
const flagPresent = commands.FlagPresent

// Python str of a value, nil reading as None
func pyStr(v any) string { return config.PyStr(v) }

// Python str of a value, empty for None
func pyStrOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return config.PyStr(v)
}

// Python truthiness of a value
func truthy(v any) bool { return config.PyTruthy(v) }

// Integer of a JSON number, zero for anything else
func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case *big.Int:
		return int(n.Int64())
	}
	return 0
}

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

func slogDebug(msg string) { slog.Debug(msg) }

func slogWarn(msg string) { slog.Warn(msg) }

func slogError(msg string, err error) { slog.Error(fmt.Sprintf("%s: %v", msg, err)) }

// Tears down the LSP services the way agent.lsp registers at exit
func lspAtExit() { lsp.AtExitShutdown() }

// Prints a line to stderr
func printErr(msg string) { fmt.Fprintln(os.Stderr, msg) }

// Truthiness of an argparse dest, False when absent or None
func nsBool(ns map[string]any, key string) bool { return truthy(ns[key]) }

// String dest of an argparse namespace, empty when absent or None
func nsString(ns map[string]any, key string) string { return pyStrOrEmpty(ns[key]) }

// Integer dest of an argparse namespace, zero when absent or not an int
func nsInt(ns map[string]any, key string) int {
	v, _ := ns[key].(int)
	return v
}
