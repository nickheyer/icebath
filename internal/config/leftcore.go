package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Config keys recording which features that left the core a home used, has converted and has installed
const (
	leftCoreUsedKey      = "_left_core_used"
	leftCoreScopedKey    = "_left_core_scoped"
	leftCoreInstalledKey = "_left_core_installed"
)

// Feature that left the core for a plugin, as far as its conversion changes the config of a home
type LeftCoreRow struct {
	Plugin           string
	Toolsets         []string
	OffPlatforms     []string
	ReplacesToolsets []string
	LeftAt           int
}

// Rows whose conversion depends on the home having run the core feature before a schema version
func versionedLeftCoreRows() []LeftCoreRow {
	return []LeftCoreRow{{Plugin: "camofox", Toolsets: []string{"camofox"}, OffPlatforms: []string{"webhook"},
		ReplacesToolsets: []string{"browser"}, LeftAt: 52}}
}

// Reports a row named in the list a config keeps under key
func leftCoreMarked(config *yaml.Node, key string, row LeftCoreRow) bool {
	done := childOf(config, key)
	return isSequence(done) && slices.ContainsFunc(done.Content, func(it *yaml.Node) bool { return displayText(it) == row.Plugin })
}

// Adds a row to the sorted list a config keeps under key
func leftCoreMark(config *yaml.Node, key string, row LeftCoreRow) {
	names := []string{row.Plugin}
	if done := childOf(config, key); isSequence(done) {
		for _, it := range done.Content {
			names = append(names, displayText(it))
		}
	}
	putKey(config, key, stringSeq(sortedSet(names...)))
}

// Value of a name from the home env files, else the process value for the launch home when asked
func (s *Store) homeEnv(ctx context.Context, h, name string, processEnv bool) (string, error) {
	resolved, err := s.Env().Resolve(home.WithOverride(ctx, h))
	if err != nil {
		return "", err
	}
	value := trimEnvSpace(resolved[name])
	if value != "" || !processEnv {
		return value, nil
	}
	launch, err := home.RoutingHome()
	if err != nil {
		return "", err
	}
	if s.sameHome(h, launch) {
		value = trimEnvSpace(os.Getenv(name))
	}
	return value, nil
}

// Reports whether the core ran its Camofox browser backend for a home, read the way the core selected it
func (s *Store) camofoxInUse(ctx context.Context, h string, config *yaml.Node, processEnv bool) (bool, error) {
	browser := childOf(config, "browser")
	if !isMapping(browser) {
		browser = emptyMapping()
	}
	cdp, err := s.homeEnv(ctx, h, "BROWSER_CDP_URL", processEnv)
	if err != nil {
		return false, err
	}
	if trimEnvSpace(orText(childOf(browser, "cdp_url"))) != "" || cdp != "" {
		return false, nil
	}
	if gateway := childOf(browser, "use_gateway"); gateway != nil && !isNull(gateway) {
		truthy := isSet(gateway)
		if isStringNode(gateway) {
			truthy = slices.Contains([]string{"1", "true", "yes", "on"}, strings.ToLower(trimEnvSpace(gateway.Value)))
		}
		if truthy {
			return false, nil
		}
	}
	if provider := childOf(browser, "cloud_provider"); provider != nil && !isNull(provider) {
		if selected := strings.ToLower(trimEnvSpace(displayText(provider))); selected != "" {
			return selected == "camofox", nil
		}
	}
	url, err := s.homeEnv(ctx, h, "CAMOFOX_URL", processEnv)
	return url != "", err
}

// Raw config of the home of ctx as a mapping, a missing, unreadable or invalid file reading as empty
func (s *Store) visibleConfig(ctx context.Context) *yaml.Node {
	raw, err := s.RawDocumentReadonly(ctx)
	if err != nil || raw == nil || !isMapping(raw.root) {
		return emptyMapping()
	}
	return raw.root
}

// Marks every row that left the core at version and the home still uses, reporting a change
func (s *Store) recordCoreUse(ctx context.Context, h string, config *yaml.Node, version int) (bool, error) {
	changed := false
	raw := s.visibleConfig(home.WithOverride(ctx, h))
	for _, row := range versionedLeftCoreRows() {
		if row.LeftAt != version || leftCoreMarked(config, leftCoreUsedKey, row) {
			continue
		}
		used, err := s.camofoxInUse(ctx, h, raw, true)
		if err != nil {
			return false, err
		}
		if used {
			leftCoreMark(config, leftCoreUsedKey, row)
			changed = true
		}
	}
	return changed, nil
}

// Env file names the retired browser launcher read, each with the browser key that takes its value
func agentBrowserEnvKeys() [][2]string {
	return [][2]string{
		{"AGENT_BROWSER_EXECUTABLE_PATH", "executable_path"}, {"AGENT_BROWSER_PROFILE", "user_data_dir"},
		{"AGENT_BROWSER_ARGS", "chromium_args"}, {"AGENT_BROWSER_HEADED", "headed"}, {"AGENT_BROWSER_ENGINE", "engine"},
	}
}

// Separators of a browser argument list
var browserArgSeparator = regexp.MustCompile(`[,\n]`)

// Browser key value a retired launcher setting meant, nil when it carries nothing
func (s *Store) browserValueFromEnv(key, value string) (*yaml.Node, error) {
	switch key {
	case "chromium_args":
		var args []string
		for _, arg := range browserArgSeparator.Split(value, -1) {
			if arg = trimEnvSpace(arg); arg != "" {
				args = append(args, arg)
			}
		}
		if len(args) == 0 {
			return nil, nil
		}
		return stringSeq(args), nil
	case "headed":
		if slices.Contains([]string{"1", "true", "yes", "on"}, strings.ToLower(value)) {
			return boolNode(true), nil
		}
		return nil, nil
	case "engine":
		if lower := strings.ToLower(value); slices.Contains([]string{"auto", "lightpanda", "chrome"}, lower) {
			return stringNode(lower), nil
		}
		return nil, nil
	case "executable_path":
		if s.know.InToolchainStore == nil {
			return nil, &SeamError{Seam: "toolchain store"}
		}
		if s.know.InToolchainStore(value) {
			return nil, nil
		}
	case "user_data_dir":
		if expanded := home.ExpandUser(value); !filepath.IsAbs(expanded) {
			abs, err := filepath.Abs(expanded)
			if err != nil {
				return nil, err
			}
			return stringNode(abs), nil
		}
	}
	return stringNode(value), nil
}

// Reports a browser key whose config value already won over its retired launcher variable
func browserConfigDecided(key string, current *yaml.Node) bool {
	switch key {
	case "engine":
		word := strings.ToLower(trimEnvSpace(orText(current)))
		return word != "" && word != "auto"
	case "headed":
		if current == nil || isNull(current) {
			return false
		}
		return slices.Contains([]string{"true", "1", "yes"}, strings.ToLower(trimEnvSpace(displayText(current))))
	}
	if current == nil || isNull(current) {
		return false
	}
	return !(isStringNode(current) && current.Value == "") && !(isSequence(current) && len(current.Content) == 0)
}

// Moves the retired launcher settings of a home into its browser section, never over a value that already won
func (s *Store) moveAgentBrowserEnv(ctx context.Context, h string, browser *yaml.Node) (moved, pinned []string, err error) {
	resolved, err := s.Env().Resolve(home.WithOverride(ctx, h))
	if err != nil {
		return nil, nil, err
	}
	launchHome, err := home.RoutingHome()
	if err != nil {
		return nil, nil, err
	}
	launch := s.sameHome(h, launchHome)
	for _, pair := range agentBrowserEnvKeys() {
		name, key := pair[0], pair[1]
		value := resolved[name]
		if value == "" && launch {
			value = os.Getenv(name)
		}
		if value = trimEnvSpace(value); value == "" || browserConfigDecided(key, childOf(browser, key)) {
			continue
		}
		next, err := s.browserValueFromEnv(key, value)
		if err != nil {
			return nil, nil, err
		}
		if next == nil {
			continue
		}
		putKey(browser, key, next)
		moved = append(moved, fmt.Sprintf("%s → browser.%s", name, key))
		if key == "user_data_dir" && next.Value != value {
			pinned = append(pinned, fmt.Sprintf("pinned browser.user_data_dir to %s: %s=%s was relative to the directory "+
				"icebath ran in; check that the browser logins are there", next.Value, name, value))
		}
	}
	return moved, pinned, nil
}

// Reports whether the config of ctx would still take retired launcher settings from its env file
func (s *Store) AgentBrowserEnvPending(ctx context.Context) (bool, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return false, err
	}
	raw, err := s.RawDocument(ctx)
	if err != nil {
		return false, err
	}
	moved, _, err := s.moveAgentBrowserEnv(ctx, h, plainCopy(dictAt(raw.root, "browser")))
	return len(moved) > 0, err
}

// Note with its first letter in upper case and a closing full stop
func sentence(note string) string {
	if note == "" {
		return "."
	}
	return upperText(note[:1]) + note[1:] + "."
}

// Reasons shown for each value the removed browser.backend key held
func browserBackendReasons() map[string]string {
	cloud := "a cloud browser is chosen with browser.cloud_provider (`icebath tools` → Browser Automation)"
	return map[string]string{
		"browser-use":   "browser_exec (Browser Use mode) is the only browser now",
		"agent-browser": "agent-browser left icebath; browser_exec is the browser now",
		"camofox":       "Camofox is now the camofox plugin (`icebath plugins install camofox`)",
		"local":         "browser_exec launches icebath's packaged Chromium itself (browser.cloud_provider: local)",
		"browserbase":   cloud, "firecrawl": cloud, "portal": cloud,
	}
}

// Drops the browser backend and extension control keys and moves the retired launcher settings into the browser section
func migrateTo52(m *migration) error {
	config, err := m.raw()
	if err != nil {
		return err
	}
	browser := dictAt(config, "browser")
	var added, messages, notices []string
	if at := mappingIndex(browser, "backend"); at >= 0 {
		backend := browser.Content[at]
		dropKey(browser, "backend")
		flag, isBool := boolScalar(backend)
		switch {
		case (isBool && !flag) || lowerIs("off")(backend):
			note := "removed browser.backend: off — the built-in browser tools are gone and browser_exec is the " +
				"browser now; to turn browsing off, disable the `browser` toolset in `icebath tools`"
			added = append(added, note)
			messages = append(messages, "  ✓ "+sentence(note))
			notices = append(notices, sentence(note))
		case isNull(backend) || isBool || trimEnvSpace(displayText(backend)) == "":
			added = append(added, "removed browser.backend (unset)")
		default:
			value := trimEnvSpace(displayText(backend))
			reason, ok := browserBackendReasons()[strings.ToLower(value)]
			if !ok {
				reason = "browser_exec is the only browser now"
			}
			added = append(added, fmt.Sprintf("removed browser.backend: %s — %s", value, reason))
		}
	}
	if mappingIndex(browser, "extension_control") >= 0 {
		dropKey(browser, "extension_control")
		added = append(added, "removed browser.extension_control")
	}
	moved, pinned, err := m.s.moveAgentBrowserEnv(m.ctx, m.home, browser)
	if err != nil {
		return err
	}
	for _, note := range pinned {
		added = append(added, note)
		messages = append(messages, "  ✓ "+sentence(note))
		notices = append(notices, sentence(note))
	}
	if len(moved) > 0 {
		added = append(added, "browser: "+strings.Join(moved, ", "))
		messages = append(messages, "  ✓ Moved agent-browser settings from .env to config.yaml: "+strings.Join(moved, ", "))
	}
	used, err := m.s.recordCoreUse(m.ctx, m.home, config, 52)
	if err != nil {
		return err
	}
	if len(added) == 0 && !used {
		return nil
	}
	if len(added) > 0 {
		putKey(config, "browser", browser)
	}
	if err := m.persist(config); err != nil {
		return err
	}
	m.report.ConfigAdded = append(m.report.ConfigAdded, added...)
	m.report.Notices = append(m.report.Notices, notices...)
	for _, message := range messages {
		m.say(message)
	}
	return nil
}

// Applies the earlier home file steps to a config getting its first version stamp, returning the browser moves
func (s *Store) noteFirstStamp(ctx context.Context, h string, onDisk, toWrite *yaml.Node) ([]string, error) {
	if mappingIndex(onDisk, "_config_version") >= 0 || mappingIndex(toWrite, "_config_version") < 0 || s.worker != nil {
		return nil, nil
	}
	var versions []int
	for _, row := range versionedLeftCoreRows() {
		if row.LeftAt != 0 && !slices.Contains(versions, row.LeftAt) {
			versions = append(versions, row.LeftAt)
		}
	}
	slices.Sort(versions)
	for _, version := range versions {
		if _, err := s.recordCoreUse(ctx, h, toWrite, version); err != nil {
			return nil, err
		}
	}
	browser := dictAt(toWrite, "browser")
	moved, pinned, err := s.moveAgentBrowserEnv(ctx, h, browser)
	if err != nil {
		return nil, err
	}
	if len(moved) > 0 {
		putKey(toWrite, "browser", browser)
	}
	for _, note := range pinned {
		s.DeferStartupNotice(home.WithOverride(ctx, h), sentence(note))
	}
	return moved, nil
}

// Creates a missing home config when the env file still carries retired launcher settings, reporting a write
func (s *Store) StampMissingConfig(ctx context.Context) (bool, error) {
	h, path, err := s.configPath(ctx)
	if err != nil {
		return false, err
	}
	b, bh, err := s.route(ctx, path)
	if err != nil {
		return false, err
	}
	if exists, err := b.Exists(ctx, bh); err != nil || exists || home.ManagedSystem(h) != "" {
		return false, err
	}
	config := emptyMapping()
	putKey(config, "_config_version", intNode(max(Version, 1)))
	moved, err := s.noteFirstStamp(ctx, h, emptyMapping(), config)
	if err != nil || len(moved) == 0 {
		return false, err
	}
	if err := s.homes.Mkdir(ctx, h); err != nil {
		return false, err
	}
	if err := s.WriteDocument(ctx, &YAMLDocument{root: config}); err != nil {
		return false, err
	}
	home.ApplySecureFilePolicy(path, h)
	return true, nil
}

// Reports whether the saved toolset selection of a platform had the toolsets of a row on under the core
func leftCoreCarried(row LeftCoreRow, selection []string) bool {
	return slices.ContainsFunc(selection, func(name string) bool {
		if slices.Contains(row.Toolsets, name) || slices.Contains(row.ReplacesToolsets, name) || name == "icebath" {
			return true
		}
		platform, composite := strings.CutPrefix(name, "icebath-")
		return composite && !slices.Contains(row.OffPlatforms, strings.ReplaceAll(platform, "-", "_"))
	})
}

// Turns the core toolsets a row replaces off in config, saved platform lists naming the plugin toolsets instead
func turnOffReplaced(config *yaml.Node, row LeftCoreRow) {
	if saved := childOf(config, "platform_toolsets"); isMapping(saved) {
		for i := 1; i < len(saved.Content); i += 2 {
			selection, ok := platformToolsets(saved.Content[i])
			if !ok || !slices.ContainsFunc(selection, func(name string) bool { return slices.Contains(row.ReplacesToolsets, name) }) {
				continue
			}
			kept := slices.DeleteFunc(slices.Clone(selection), func(name string) bool { return slices.Contains(row.ReplacesToolsets, name) })
			for _, ts := range row.Toolsets {
				if !slices.Contains(kept, ts) {
					kept = append(kept, ts)
				}
			}
			saved.Content[i] = stringSeq(kept)
		}
	}
	agent := dictAt(config, "agent")
	var disabled []string
	if current := childOf(agent, "disabled_toolsets"); isSet(current) {
		disabled = configStringList(current)
	}
	turnedOff := slices.Clone(row.ReplacesToolsets)
	if slices.ContainsFunc(row.ReplacesToolsets, func(name string) bool { return slices.Contains(disabled, name) }) {
		turnedOff = append(turnedOff, row.Toolsets...)
	}
	for _, name := range sortedSet(turnedOff...) {
		if !slices.Contains(disabled, name) {
			disabled = append(disabled, name)
		}
	}
	putKey(agent, "disabled_toolsets", stringSeq(disabled))
	putKey(config, "agent", agent)
}

// Keeps the core toolset scope of a row and marks it installed once its plugin folder exists, in one write
func (s *Store) RecordLeftCoreMigration(ctx context.Context, row LeftCoreRow) error {
	h, path, err := s.configPath(ctx)
	if err != nil {
		return err
	}
	b, bh, err := s.route(ctx, path)
	if err != nil {
		return err
	}
	exists, err := b.Exists(ctx, bh)
	if err != nil {
		return err
	}
	locked, release, err := s.lockWrite(ctx, path)
	if err != nil {
		return err
	}
	config, err := s.requireReadable(locked, path)
	if err != nil {
		return errors.Join(err, release())
	}
	scopeKept := len(row.Toolsets) == 0 || leftCoreMarked(config, leftCoreScopedKey, row)
	present := false
	if fi, err := os.Stat(filepath.Join(home.Plugins.In(h), row.Plugin)); err == nil {
		present = fi.IsDir()
	}
	installed := present && !leftCoreMarked(config, leftCoreInstalledKey, row)
	if scopeKept && !installed {
		return release()
	}
	if installed {
		leftCoreMark(config, leftCoreInstalledKey, row)
	}
	if !scopeKept {
		type selection struct {
			platform string
			items    []string
			valid    bool
		}
		var selections []selection
		if saved := childOf(config, "platform_toolsets"); isMapping(saved) {
			for i := 0; i+1 < len(saved.Content); i += 2 {
				items, ok := platformToolsets(saved.Content[i+1])
				selections = append(selections, selection{displayText(saved.Content[i]), items, ok})
			}
		}
		for _, platform := range row.OffPlatforms {
			if !slices.ContainsFunc(selections, func(sel selection) bool { return sel.platform == platform }) {
				selections = append(selections, selection{platform: platform})
			}
		}
		known := dictAt(config, "known_plugin_toolsets")
		for _, sel := range selections {
			if (!sel.valid && !slices.Contains(row.OffPlatforms, sel.platform)) || (sel.valid && leftCoreCarried(row, sel.items)) {
				continue
			}
			var current []string
			if list := childOf(known, sel.platform); isSequence(list) {
				for _, it := range list.Content {
					current = append(current, displayText(it))
				}
			}
			if slices.ContainsFunc(row.Toolsets, func(ts string) bool { return !slices.Contains(current, ts) }) {
				putKey(known, sel.platform, stringSeq(sortedSet(append(current, row.Toolsets...)...)))
			}
		}
		if len(known.Content) > 0 {
			putKey(config, "known_plugin_toolsets", known)
		}
		if len(row.ReplacesToolsets) > 0 {
			turnOffReplaced(config, row)
		}
		leftCoreMark(config, leftCoreScopedKey, row)
	}
	if !exists {
		putKey(config, "_config_version", intNode(Version))
		if _, err := s.noteFirstStamp(locked, h, emptyMapping(), config); err != nil {
			return errors.Join(err, release())
		}
	}
	return errors.Join(s.writeState(locked, path, config, false, ""), release())
}

// Reports whether a home ran the core feature of a row, from its version stamp and recorded rows
func (s *Store) PredatesLeftCoreSplit(ctx context.Context, row LeftCoreRow) bool {
	if row.LeftAt == 0 {
		return true
	}
	raw := s.visibleConfig(ctx)
	at := mappingIndex(raw, "_config_version")
	if leftCoreMarked(raw, leftCoreUsedKey, row) || at < 0 {
		return true
	}
	version, ok := wholeOf(raw.Content[at])
	return ok && version < row.LeftAt
}

// Longest wait for the lock of the kept startup notices of a home
const noticeLockWait = 5 * time.Second

// Runs fn holding the cross process lock of the kept startup notices of the home of ctx
func (s *Store) noticesLocked(ctx context.Context, fn func(path string) error) error {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return err
	}
	path := home.LeftCoreNotices.In(h)
	if err := s.homes.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, noticeLockWait)
	lock, err := fileutil.OpenLock(wait, home.LeftCoreNoticesLock.In(h),
		fileutil.TimeoutMessage(fmt.Sprintf("Timed out waiting for the left-core notice lock (%s)", path)))
	cancel()
	if err != nil {
		return err
	}
	return errors.Join(fn(path), lock.Release())
}

// Logs a message and keeps it once for the next turn someone sees in the home of ctx
func (s *Store) DeferStartupNotice(ctx context.Context, message string) {
	slog.Warn(message)
	var b strings.Builder
	writeJSONString(&b, message, true)
	line := b.String()
	err := s.noticesLocked(ctx, func(path string) error {
		if text, err := fileutil.ReadText(path); err == nil && slices.Contains(strings.Split(text, "\n"), line) {
			return nil
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.WriteString(line + "\n")
		return errors.Join(werr, f.Close())
	})
	if err != nil {
		slog.Debug(fmt.Sprintf("left-core notice not kept: %v", err))
	}
}

// Takes every notice kept for the home of ctx exactly once across processes, in the order they were kept
func (s *Store) ClaimStartupNotices(ctx context.Context) ([]string, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(home.LeftCoreNotices.In(h)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var lines []string
	err = s.noticesLocked(ctx, func(path string) error {
		text, err := fileutil.ReadText(path)
		if err != nil {
			return err
		}
		lines = strings.Split(text, "\n")
		return os.Remove(path)
	})
	if err != nil {
		return nil, nil
	}
	var out, seen []string
	for _, line := range lines {
		if line == "" || slices.Contains(seen, line) {
			continue
		}
		seen = append(seen, line)
		var value any
		if json.Unmarshal([]byte(line), &value) != nil {
			continue
		}
		if text, ok := value.(string); ok {
			out = append(out, text)
		} else {
			out = append(out, fmt.Sprint(value))
		}
	}
	return out, nil
}
