package tools

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Shortest idle time a server is stopped after, in seconds
const minIdle = 30.0

// One entry of the servers setting, by id
type serverEntry struct {
	id  string
	raw map[string]any
}

// The language server settings of a profile as a service runs by them
type settings struct {
	enabled  bool
	waitMode string
	// Seconds
	wait, warmup, retry, idle float64
	// A missing server is installed the first time a file needs it
	auto bool
	// Exclude globs, and whether the setting is no list, which keeps every workspace out
	excludes       []string
	excludeInvalid bool
	bundle         string
	entries        []serverEntry
	commands       map[string][]string
	envs           map[string]map[string]string
	inits          map[string]map[string]any
	disabled       map[string]bool
}

// Seconds of a number setting, none below zero
func seconds(cfg *config.Config, key string) float64 {
	return max(cast.ToFloat64(cfg.Get(key)), 0)
}

func duration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Settings of the profile, the servers table read as its file writes it
func (m *Manager) read(cfg *config.Config) settings {
	s := settings{enabled: cfg.GetBool(lspconfig.Enabled), waitMode: cfg.GetString(lspconfig.WaitMode), wait: seconds(cfg, lspconfig.WaitTimeout),
		warmup: seconds(cfg, lspconfig.WarmupTimeout), retry: seconds(cfg, lspconfig.BrokenRetry), idle: seconds(cfg, lspconfig.IdleTimeout),
		auto: cfg.GetBool(lspconfig.InstallWhenNeeded), bundle: cfg.GetString(lspconfig.PowerShellBundle), commands: map[string][]string{},
		envs: map[string]map[string]string{}, inits: map[string]map[string]any{}, disabled: map[string]bool{}}
	if s.waitMode != lspconfig.WaitDocument && s.waitMode != lspconfig.WaitFull {
		s.waitMode = lspconfig.WaitDocument
	}
	if s.idle > 0 && s.idle < minIdle {
		s.idle = minIdle
	}
	excludes, ok := tools.Folders(cfg, lspconfig.ExcludeRoots)
	for i, pattern := range excludes {
		excludes[i] = fsx.ExpandHome(pattern)
	}
	s.excludes, s.excludeInvalid = excludes, !ok
	s.entries = fromSettings(cfg)
	for _, e := range s.entries {
		command := commandOf(e.raw[lspconfig.Command])
		// The command of a server icebath knows is the path of its program, which may hold what a shell would split or take away, such
		// as a space or a backslash: a path that is there is the program whole
		if text, ok := e.raw[lspconfig.Command].(string); ok && builtin(e.id) {
			if whole := fsx.ExpandHome(strings.TrimSpace(text)); whole != "" && fsx.Exists(whole) {
				command = []string{whole}
			}
		}
		if len(command) > 0 {
			s.commands[e.id] = command
		}
		if env, ok := e.raw[lspconfig.Env].(map[string]any); ok {
			vars := map[string]string{}
			for k, v := range env {
				vars[k] = fmt.Sprint(v)
			}
			s.envs[e.id] = vars
		}
		if init, ok := e.raw[lspconfig.Init].(map[string]any); ok {
			s.inits[e.id] = init
		}
		if truthy(e.raw[lspconfig.Disabled]) {
			s.disabled[e.id] = true
		}
	}
	return s
}

// Words of a server's command: a command line split as a shell splits it, or a list of words kept as written; none for any other value or
// a line that does not split
func commandOf(v any) []string {
	switch command := v.(type) {
	case string:
		words, err := strs.Shell(command)
		if err != nil {
			return nil
		}
		return words
	case []any:
		var words []string
		for _, part := range command {
			if word := fmt.Sprint(part); part != nil && word != "" {
				words = append(words, word)
			}
		}
		return words
	}
	return nil
}

// Whether a value written as on, yes, true or a number other than zero says so
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return config.Yes(t)
	case int, int64, float64:
		return cast.ToFloat64(t) != 0
	}
	return false
}

// Whether two readings of the settings start the same service
func (s settings) same(other settings) bool { return reflect.DeepEqual(s, other) }

// Entries of the servers setting in the order the settings write them, their names and options as written
func fromSettings(cfg *config.Config) []serverEntry {
	table, _ := cfg.Get(lspconfig.Servers).(map[string]any)
	var entries []serverEntry
	for _, id := range cfg.Order(lspconfig.Servers) {
		if raw, ok := table[id].(map[string]any); ok {
			entries = append(entries, serverEntry{id: id, raw: raw})
		}
	}
	return entries
}
