package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Collaborators the agent browser env move reaches outside this package
type BrowserEnvSources struct {
	HomeDotenv         func(h string) map[string]string
	IsLaunchHome       func(h string) bool
	PMStoreRoots       func() ([]string, error)
	DeferStartupNotice func(message string)
}

// AGENT_BROWSER env names and the browser key each one moves to, in move order
var agentBrowserEnvKeys = [][2]string{
	{"AGENT_BROWSER_EXECUTABLE_PATH", "executable_path"},
	{"AGENT_BROWSER_PROFILE", "user_data_dir"},
	{"AGENT_BROWSER_ARGS", "chromium_args"},
	{"AGENT_BROWSER_HEADED", "headed"},
	{"AGENT_BROWSER_ENGINE", "engine"},
}

type browserEnvValue struct{ name, key, value string }

// AGENT_BROWSER settings of a home from its dotenv layers, plus the process env for the launch home only
func agentBrowserEnv(h string, src BrowserEnvSources) []browserEnvValue {
	dotenv := src.HomeDotenv(h)
	launch := src.IsLaunchHome(h)
	var found []browserEnvValue
	for _, pair := range agentBrowserEnvKeys {
		value := dotenv[pair[0]]
		if value == "" && launch {
			value = os.Getenv(pair[0])
		}
		if value = strings.TrimFunc(value, unicode.IsSpace); value != "" {
			found = append(found, browserEnvValue{pair[0], pair[1], value})
		}
	}
	return found
}

// Path with a leading tilde expanded the way os.path.expanduser does
func pyExpandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	name, rest, _ := strings.Cut(p[1:], string(filepath.Separator))
	var dir string
	if name == "" {
		if dir = os.Getenv("HOME"); dir == "" {
			u, err := user.Current()
			if err != nil {
				return p
			}
			dir = u.HomeDir
		}
	} else {
		u, err := user.Lookup(name)
		if err != nil {
			return p
		}
		dir = u.HomeDir
	}
	if rest == "" && !strings.Contains(p, string(filepath.Separator)) {
		return dir
	}
	return strings.TrimRight(dir, string(filepath.Separator)) + string(filepath.Separator) + rest
}

// Absolute path with every existing symlink resolved and the missing tail kept, as Path.resolve does
func pyResolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	tail := ""
	for cur := abs; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, tail), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// Reports whether a Chromium path sits inside the pm store, whose versioned paths must not be pinned
func insidePMStore(value string, src BrowserEnvSources) bool {
	resolved, err := pyResolve(pyExpandUser(value))
	if err != nil {
		return false
	}
	roots, err := src.PMStoreRoots()
	if err != nil {
		return false
	}
	for _, root := range roots {
		r, err := pyResolve(root)
		if err != nil {
			return false
		}
		if rel, err := filepath.Rel(r, resolved); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

var chromiumArgSplitRe = regexp.MustCompile(`[,\n]`)

// Browser key value an AGENT_BROWSER value meant, false when it carries nothing
func browserValueFromEnv(key, value string, src BrowserEnvSources) (any, bool) {
	lower := strings.ToLower(value)
	switch key {
	case "chromium_args":
		args := []any{}
		for _, arg := range chromiumArgSplitRe.Split(value, -1) {
			if arg = strings.TrimFunc(arg, unicode.IsSpace); arg != "" {
				args = append(args, arg)
			}
		}
		return args, len(args) > 0
	case "headed":
		return true, lower == "1" || lower == "true" || lower == "yes" || lower == "on"
	case "engine":
		return lower, lower == "auto" || lower == "lightpanda" || lower == "chrome"
	case "executable_path":
		if insidePMStore(value, src) {
			return nil, false
		}
	case "user_data_dir":
		expanded := pyExpandUser(value)
		if !filepath.IsAbs(expanded) {
			abs, err := filepath.Abs(expanded)
			if err != nil {
				return nil, false
			}
			return abs, true
		}
	}
	return value, true
}

// Reports whether a browser key already won over its AGENT_BROWSER variable
func browserConfigDecided(key string, current any) bool {
	switch key {
	case "engine":
		s := ""
		if PyTruthy(current) {
			s = PyStr(current)
		}
		s = strings.ToLower(strings.TrimFunc(s, unicode.IsSpace))
		return s != "" && s != "auto"
	case "headed":
		if current == nil {
			return false
		}
		s := strings.ToLower(strings.TrimFunc(PyStr(current), unicode.IsSpace))
		return s == "true" || s == "1" || s == "yes"
	}
	if current == nil || pyEq(current, "") {
		return false
	}
	if list, ok := current.([]any); ok && len(list) == 0 {
		return false
	}
	return true
}

var browserBackendReasons = map[string]string{
	"browser-use":   "browser_exec (Browser Use mode) is the only browser now",
	"agent-browser": "agent-browser left icebath; browser_exec is the browser now",
	"camofox":       "Camofox is now the camofox plugin (`icebath plugins install camofox`)",
	"local":         "browser_exec launches icebath's packaged Chromium itself (browser.cloud_provider: local)",
	"browserbase":   "a cloud browser is chosen with browser.cloud_provider (`icebath tools` \u2192 Browser Automation)",
	"firecrawl":     "a cloud browser is chosen with browser.cloud_provider (`icebath tools` \u2192 Browser Automation)",
	"nous":          "a cloud browser is chosen with browser.cloud_provider (`icebath tools` \u2192 Browser Automation)",
}

// Config added note naming a removed browser.backend value and why it went
func backendRemovedNote(backend any) string {
	value := ""
	if _, isBool := backend.(bool); backend != nil && !isBool {
		value = strings.TrimFunc(PyStr(backend), unicode.IsSpace)
	}
	if value == "" {
		return "removed browser.backend (unset)"
	}
	reason, ok := browserBackendReasons[strings.ToLower(value)]
	if !ok {
		reason = "browser_exec is the only browser now"
	}
	return fmt.Sprintf("removed browser.backend: %s \u2014 %s", value, reason)
}

// Note with its first character upper cased and a closing period
func capitalizedNote(note string) string {
	r, size := utf8.DecodeRuneInString(note)
	return string(unicode.ToUpper(r)) + note[size:] + "."
}

// Moves a home's AGENT_BROWSER settings into its browser section, returning the moves and pinned profile notes
func MoveAgentBrowserEnv(h string, browser map[string]any, src BrowserEnvSources) ([]string, []string) {
	var moved, pinned []string
	for _, env := range agentBrowserEnv(h, src) {
		if browserConfigDecided(env.key, browser[env.key]) {
			continue
		}
		value, ok := browserValueFromEnv(env.key, env.value, src)
		if !ok {
			continue
		}
		browser[env.key] = value
		moved = append(moved, env.name+" \u2192 browser."+env.key)
		if env.key == "user_data_dir" && value != env.value {
			pinned = append(pinned, fmt.Sprintf("pinned browser.user_data_dir to %s: %s=%s was relative to the directory icebath ran in; "+
				"check that the browser logins are there", value, env.name, env.value))
		}
	}
	return moved, pinned
}

// Reports whether the home config would still take AGENT_BROWSER settings from its env
func AgentBrowserEnvPending(h string, src BrowserEnvSources) bool {
	browser := map[string]any{}
	if raw, err := ReadUser(h); err == nil {
		if m, ok := raw["browser"].(map[string]any); ok {
			for k, v := range m {
				browser[k] = v
			}
		}
	}
	moved, _ := MoveAgentBrowserEnv(h, browser, src)
	return len(moved) > 0
}

// Moves AGENT_BROWSER settings into a config receiving its first stamp outside a step, deferring pinned notices
func AdoptAgentBrowserEnv(h string, cfg map[string]any, src BrowserEnvSources) []string {
	browser, ok := cfg["browser"].(map[string]any)
	if !ok {
		browser = map[string]any{}
	}
	moved, pinned := MoveAgentBrowserEnv(h, browser, src)
	if len(moved) > 0 {
		cfg["browser"] = browser
	}
	for _, note := range pinned {
		src.DeferStartupNotice(capitalizedNote(note))
	}
	return moved
}

// Drops browser.backend and extension_control and moves AGENT_BROWSER env settings into browser keys
func migrateTo52(r *migrationRun) error {
	cfg := r.read()
	browser, ok := cfg["browser"].(map[string]any)
	if !ok {
		browser = map[string]any{}
	}
	var added, messages, notices []string
	if backend, has := browser["backend"]; has {
		delete(browser, "backend")
		s, isStr := backend.(string)
		if backend == false || (isStr && strings.ToLower(strings.TrimFunc(s, unicode.IsSpace)) == "off") {
			note := "removed browser.backend: off \u2014 the built-in browser tools are gone and browser_exec is the browser now; to turn " +
				"browsing off, disable the `browser` toolset in `icebath tools`"
			added = append(added, note)
			messages = append(messages, "  \u2713 "+capitalizedNote(note))
			notices = append(notices, capitalizedNote(note))
		} else {
			added = append(added, backendRemovedNote(backend))
		}
	}
	if _, has := browser["extension_control"]; has {
		delete(browser, "extension_control")
		added = append(added, "removed browser.extension_control")
	}
	moved, pinned := MoveAgentBrowserEnv(r.h, browser, r.src.Browser)
	for _, note := range pinned {
		added = append(added, note)
		messages = append(messages, "  \u2713 "+capitalizedNote(note))
		notices = append(notices, capitalizedNote(note))
	}
	if len(moved) > 0 {
		added = append(added, "browser: "+strings.Join(moved, ", "))
		messages = append(messages, "  \u2713 Moved agent-browser settings from .env to config.yaml: "+strings.Join(moved, ", "))
	}
	used := r.src.RecordCoreUse(r.h, cfg, 52)
	if len(added) == 0 && !used {
		return nil
	}
	if len(added) > 0 {
		cfg["browser"] = browser
	}
	if err := r.src.Persist(r.h, cfg); err != nil {
		return err
	}
	r.res.ConfigAdded = append(r.res.ConfigAdded, added...)
	r.res.Notices = append(r.res.Notices, notices...)
	for _, message := range messages {
		r.say(message)
	}
	return nil
}
