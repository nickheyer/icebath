package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Opcode is one difflib opcode turning the a range I1 to I2 into the b range J1 to J2
type Opcode struct {
	Tag            string
	I1, I2, J1, J2 int
}

// Runtime carries the host services the LSP layer reaches through its caller
type Runtime struct {
	Home                         string
	Log                          *slog.Logger
	Config                       func() map[string]any
	EnvFor                       func(names []string, base map[string]string) map[string]string
	EnsureEnv                    func(name string, base map[string]string) (map[string]string, error)
	InstalledBinary              func(name string) (string, bool)
	PythonTool                   func(name, bin string) (string, bool, error)
	EnsurePythonTool             func(name string, requirements []string, bin string, timeout time.Duration) (string, error)
	SubprocessEnv                func() map[string]string
	DelegatedChildEnv            func(child map[string]string) map[string]string
	OwnProcessGroups             func(pgids []int)
	RegisterChild                func(ctx context.Context, pid int, purpose string) bool
	ReleaseProcessGroupOnceEmpty func(pgid int, poll func(), timeout time.Duration) bool
	Opcodes                      func(a, b []string) []Opcode
	DecodeJSON                   func(text string) (any, error)
}

// Logger of the runtime or the process default
func (rt *Runtime) logger() *slog.Logger {
	if rt != nil && rt.Log != nil {
		return rt.Log
	}
	return slog.Default()
}

// Logs one printf style message under a Python logger name
func (rt *Runtime) logf(level slog.Level, name, format string, args ...any) {
	l := rt.logger()
	if !l.Enabled(context.Background(), level) {
		return
	}
	l.Log(context.Background(), level, fmt.Sprintf(format, args...), "logger", name)
}

// Current lsp config section, empty when none is attached
func (rt *Runtime) lspConfig() map[string]any {
	if rt == nil || rt.Config == nil {
		return map[string]any{}
	}
	if cfg := rt.Config(); cfg != nil {
		return cfg
	}
	return map[string]any{}
}

// Profile home, the process home when the runtime names none
func (rt *Runtime) home() string {
	if rt != nil && rt.Home != "" {
		return rt.Home
	}
	return home.Process()
}

// Composed env of installed packages layered over a base
func (rt *Runtime) envFor(names []string, base map[string]string) (map[string]string, error) {
	if rt == nil || rt.EnvFor == nil {
		return nil, missingHook("EnvFor")
	}
	return guard(func() (map[string]string, error) { return rt.EnvFor(names, base), nil })
}

// Binary of an installed managed package
func (rt *Runtime) installedBinary(name string) (string, bool, error) {
	if rt == nil || rt.InstalledBinary == nil {
		return "", false, missingHook("InstalledBinary")
	}
	type found struct {
		bin string
		ok  bool
	}
	f, err := guard(func() (found, error) {
		bin, ok := rt.InstalledBinary(name)
		return found{bin, ok}, nil
	})
	return f.bin, f.ok, err
}

// Scrubbed environment for third party children
func (rt *Runtime) subprocessEnv() (map[string]string, error) {
	if rt == nil || rt.SubprocessEnv == nil {
		return nil, missingHook("SubprocessEnv")
	}
	return guard(func() (map[string]string, error) { return maps.Clone(rt.SubprocessEnv()), nil })
}

// Error naming a host service the runtime was built without
func missingHook(name string) error {
	return fmt.Errorf("lsp runtime has no %s service", name)
}

// Runs a host service, a panic it raises returned as the error a Python exception would be
func guard[T any](fn func() (T, error)) (v T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return fn()
}

// json.loads of the host, a missing service failing every decode
func (rt *Runtime) jsonDecoder() func(text string) (any, error) {
	if rt == nil || rt.DecodeJSON == nil {
		return func(string) (any, error) { return nil, missingHook("DecodeJSON") }
	}
	return func(text string) (any, error) { return guard(func() (any, error) { return rt.DecodeJSON(text) }) }
}
