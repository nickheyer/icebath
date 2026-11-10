package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"gopkg.in/yaml.v3"
)

// EvictingSync is how an update sync was asked, the extras it adds, the shipped and frozen features and whether explicit
type EvictingSync struct {
	Extras, Shipped, Frozen []string
	Explicit                bool
}

// Version of the interpreter the eviction verdicts judge requires-python against
func pythonVersionOf(ctx context.Context, python string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, Native(python), "-I", "-c", "import platform; print(platform.python_version())")
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", &PyError{Class: "TimeoutExpired", Message: fmt.Sprintf("Command '%s' timed out after 60 seconds", pyrepr.Repr(cmd.Args))}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "", &PyError{Class: "CalledProcessError", Message: fmt.Sprintf("Command '%s' returned non-zero exit status %d.", pyrepr.Repr(cmd.Args), exitErr.ExitCode())}
	}
	if err != nil {
		return "", err
	}
	return pystr.Strip(string(out)), nil
}

// Python version lookup the eviction verdicts use, replaceable by tests the way Python patches it
var interpreterVersion func(ctx context.Context) (string, error)

func init() { interpreterVersion = pinnedInterpreterVersion }

// Python version the generation is built for, judged against each plugin's requires-python
func pinnedInterpreterVersion(ctx context.Context) (string, error) {
	_, python, found, err := uvToolchain(ctx, true, true)
	if err != nil {
		return "", err
	}
	if !found {
		return "", NewInstallError("venv", "PM's pinned toolchain is unavailable", "")
	}
	return pythonVersionOf(ctx, python)
}

// Reports an error Python raises as OSError, ValueError or TypeError
func isOSValueTypeError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &syscallErr) {
		return true
	}
	switch errorClass(err) {
	case "OSError", "FileNotFoundError", "FileExistsError", "PermissionError", "IsADirectoryError", "NotADirectoryError",
		"TimeoutError", "ValueError", "UnicodeDecodeError", "JSONDecodeError", "TOMLDecodeError", "TypeError":
		return true
	}
	return false
}

// Reports an error Python raises as a ValueError or one of its subclasses
func isPyValueError(err error) bool {
	switch errorClass(err) {
	case "ValueError", "UnicodeDecodeError", "JSONDecodeError", "TOMLDecodeError":
		return true
	}
	return false
}

// Disable and sit out reasons found without a resolver, keyed by resolved plugin dir
func staticVerdicts(ctx context.Context, entries []PluginEntry, pythonVersion string, extras []string) (map[string]string, map[string]string, error) {
	reasons, waiting := map[string]string{}, map[string]string{}
	for _, entry := range entries {
		key := resolvePath(entry.PluginDir)
		if _, done := reasons[key]; done {
			continue
		}
		if _, done := waiting[key]; done {
			continue
		}
		declaration, err := ReadPythonDeclaration(entry.PluginDir)
		if err != nil {
			if !isOSValueTypeError(err) {
				return nil, nil, err
			}
			reasons[key] = "its dependency declaration is invalid: " + pyExceptionText(err)
			continue
		}
		hermes, err := hermesFitError(ctx, declaration, extras)
		if err != nil {
			return nil, nil, err
		}
		if hermes != "" {
			waiting[key] = hermes
			continue
		}
		name := filepath.Base(entry.PluginDir)
		manifest, err := ManifestVersionError(declaration.Manifest, name)
		if err != nil {
			return nil, nil, err
		}
		reason := strings.TrimPrefix(manifest, "Plugin '"+name+"' ")
		if manifest == "" {
			if reason, err = declaration.PythonError(pythonVersion); err != nil {
				return nil, nil, err
			}
		}
		if reason != "" {
			reasons[key] = reason
		}
	}
	return reasons, waiting, nil
}

// Removes every item spelled one of the names from a sequence in place, keeping the order and comments of the rest
func discardNames(names *yaml.Node, spellings map[string]bool) error {
	var kept []*yaml.Node
	for _, item := range names.Content {
		spelled, err := rtSpelledIn(item, spellings)
		if err != nil {
			return err
		}
		if !spelled {
			kept = append(kept, item)
		}
	}
	names.Content = kept
	return nil
}

// Python membership of a name in a disabled value, substrings of a string and keys of a mapping counting
func rtContains(container *yaml.Node, name string) (bool, error) {
	container = rtDeref(container)
	switch {
	case container.Kind == yaml.SequenceNode:
		return seqHasString(container, name), nil
	case container.Kind == yaml.MappingNode:
		return rtGet(container, name) != nil, nil
	case container.Kind == yaml.ScalarNode && rtTag(container) == "str":
		return strings.Contains(container.Value, name), nil
	}
	return false, &PyError{Class: "TypeError", Message: fmt.Sprintf("argument of type '%s' is not iterable", rtTag(container))}
}

// Removes spellings from a list value in place the way Python indexes and deletes, a string or mapping raising its error
func discardIn(names *yaml.Node, spellings map[string]bool) error {
	names = rtDeref(names)
	switch {
	case names.Kind == yaml.SequenceNode:
		return discardNames(names, spellings)
	case names.Kind == yaml.ScalarNode && rtTag(names) == "str":
		runes := []rune(names.Value)
		for i := len(runes) - 1; i >= 0; i-- {
			if spellings[string(runes[i])] {
				return &PyError{Class: "TypeError", Message: "'str' object doesn't support item deletion"}
			}
		}
		return nil
	case names.Kind == yaml.MappingNode:
		for index := len(names.Content)/2 - 1; index >= 0; index-- {
			id, position := fmt.Sprintf("i:%d", index), -1
			for i := 0; i+1 < len(names.Content); i += 2 {
				if rtKey(names.Content[i]) == id {
					position = i
				}
			}
			if position < 0 {
				return &PyError{Class: "KeyError", Message: fmt.Sprintf("%d", index)}
			}
			spelled, err := rtSpelledIn(names.Content[position+1], spellings)
			if err != nil {
				return err
			}
			if spelled {
				rtDeleteKey(names, id)
			}
		}
		return nil
	}
	return &PyError{Class: "TypeError", Message: fmt.Sprintf("object of type '%s' has no len()", rtTag(names))}
}

// Reports whether a node is a string among the spellings, a list or mapping raising the unhashable TypeError
func rtSpelledIn(n *yaml.Node, spellings map[string]bool) (bool, error) {
	n = rtDeref(n)
	if n.Kind == yaml.SequenceNode || n.Kind == yaml.MappingNode {
		return false, &PyError{Class: "TypeError", Message: fmt.Sprintf("unhashable type: '%s'", rtTag(n))}
	}
	return n.Kind == yaml.ScalarNode && rtTag(n) == "str" && spellings[n.Value], nil
}

// Reports whether a sequence holds the string
func seqHasString(seq *yaml.Node, text string) bool {
	for _, item := range seq.Content {
		if rtIsString(item, text) {
			return true
		}
	}
	return false
}

// Value under a plugins key, an absent or null one replaced by a fresh empty list
func listNode(plugins *yaml.Node, key string) *yaml.Node {
	list := rtGet(plugins, key)
	if rtIsNone(list) {
		list = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		rtSet(plugins, key, list)
	}
	return rtDeref(list)
}

// Appends a name to a list value, the AttributeError Python raises for any other value
func appendName(list *yaml.Node, name string) error {
	if list.Kind != yaml.SequenceNode {
		return &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'append'", rtTag(list))}
	}
	list.Content = append(list.Content, rtString(name))
	return nil
}

// Parks a plugin in plugins disabled, drops it from plugins enabled and records what a later update undoes
func disablePlugin(plugins *yaml.Node, name string, spellings map[string]bool, reason, tried string) error {
	disabled := listNode(plugins, "disabled")
	enabled := rtGet(plugins, "enabled")
	record := rtGet(plugins, DisabledByUpdateRecord)
	if record == nil || record.Kind != yaml.MappingNode {
		record = rtMapping()
		rtSet(plugins, DisabledByUpdateRecord, record)
	}
	earlier := rtGet(record, name)
	wasEnabled := false
	parked, err := rtContains(disabled, name)
	if err != nil {
		return err
	}
	if parked && earlier != nil && earlier.Kind == yaml.MappingNode {
		wasEnabled = rtTruthy(rtGet(earlier, "enabled"))
	} else if enabled != nil && enabled.Kind == yaml.SequenceNode {
		for _, item := range enabled.Content {
			spelled, err := rtSpelledIn(item, spellings)
			if err != nil {
				return err
			}
			wasEnabled = wasEnabled || spelled
		}
	}
	if !parked {
		if err := appendName(disabled, name); err != nil {
			return err
		}
	}
	if enabled != nil && enabled.Kind == yaml.SequenceNode {
		if err := discardNames(enabled, spellings); err != nil {
			return err
		}
	}
	words := []rune(strings.Join(strings.FieldsFunc(reason, pystr.IsSpace), " "))
	if len(words) > 300 {
		words = words[:300]
	}
	entry := rtMapping()
	rtSet(entry, "enabled", rtBool(wasEnabled))
	rtSet(entry, "reason", rtString(string(words)))
	if tried != "" {
		rtSet(entry, "tried", rtString(tried))
	}
	rtSet(record, name, entry)
	return nil
}

// Undoes an earlier update's disable now the plugin fits, unless the user settled it since
func rejoinPlugin(plugins *yaml.Node, name string, spell func() (map[string]bool, error)) error {
	record := rtGet(plugins, DisabledByUpdateRecord)
	if record == nil || record.Kind != yaml.MappingNode {
		return nil
	}
	earlier := rtGet(record, name)
	if earlier == nil || earlier.Kind != yaml.MappingNode {
		return nil
	}
	disabled := listNode(plugins, "disabled")
	spellings, err := spell()
	if err != nil {
		return err
	}
	if err := discardIn(disabled, spellings); err != nil {
		return err
	}
	if !rtTruthy(rtGet(earlier, "enabled")) {
		return nil
	}
	enabled := listNode(plugins, "enabled")
	present, err := rtContains(enabled, name)
	if err != nil || present {
		return err
	}
	return appendName(enabled, name)
}

// Identity of a decoded JSON value under Python equality, an error for an unhashable one
func jsonKey(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "n", nil
	case string:
		return "s:" + t, nil
	case bool:
		if t {
			return "i:1", nil
		}
		return "i:0", nil
	case float64:
		value := big.NewFloat(t)
		if value.IsInt() {
			integer, _ := value.Int(nil)
			return "i:" + integer.String(), nil
		}
		return "f:" + value.Text('g', -1), nil
	case int:
		return fmt.Sprintf("i:%d", t), nil
	case int64:
		return fmt.Sprintf("i:%d", t), nil
	case *big.Int:
		return "i:" + t.String(), nil
	case fmt.Stringer:
		return "i:" + t.String(), nil
	}
	return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(v))}
}

// Forgets the records of plugins the user just enabled, disabled or removed, then prunes the rest
func settleRecord(plugins *yaml.Node, settled []any) error {
	if record := rtGet(plugins, DisabledByUpdateRecord); record != nil && record.Kind == yaml.MappingNode {
		for _, name := range settled {
			id, err := jsonKey(name)
			if err != nil {
				return err
			}
			rtDeleteKey(record, id)
		}
	}
	return pruneRecord(plugins)
}

// Forgets records whose plugin left plugins disabled and drops an empty or malformed record
func pruneRecord(plugins *yaml.Node) error {
	record := rtGet(plugins, DisabledByUpdateRecord)
	if record == nil || record.Kind != yaml.MappingNode {
		rtDeleteKey(plugins, "s:"+DisabledByUpdateRecord)
		return nil
	}
	disabled := map[string]bool{}
	if list := rtGet(plugins, "disabled"); rtTruthy(list) {
		if list.Kind == yaml.SequenceNode {
			for _, item := range list.Content {
				if inner := rtDeref(item); inner.Kind == yaml.SequenceNode || inner.Kind == yaml.MappingNode {
					return &PyError{Class: "TypeError", Message: fmt.Sprintf("unhashable type: '%s'", rtTag(inner))}
				}
				disabled[rtKey(item)] = true
			}
		} else if list.Kind == yaml.MappingNode {
			for i := 0; i < len(list.Content); i += 2 {
				disabled[rtKey(list.Content[i])] = true
			}
		} else if rtTag(list) == "str" {
			for _, r := range list.Value {
				disabled["s:"+string(r)] = true
			}
		} else {
			return &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", rtTag(list))}
		}
	}
	kept := record.Content[:0]
	for i := 0; i+1 < len(record.Content); i += 2 {
		if disabled[rtKey(record.Content[i])] {
			kept = append(kept, record.Content[i], record.Content[i+1])
		}
	}
	record.Content = kept
	if len(record.Content) == 0 {
		rtDeleteKey(plugins, "s:"+DisabledByUpdateRecord)
	}
	return nil
}

// One plugin an earlier update disabled that its home still has disabled, with the record it left
type recordedEviction struct {
	Entry  PluginEntry
	Record map[string]any
}

// Plugins an earlier update disabled and their homes still keep disabled, a provider record only while it is the provider
func recordedEvictions(ctx context.Context) ([]recordedEviction, error) {
	homes, err := dependencyHomes(ctx)
	if err != nil {
		return nil, err
	}
	var entries []recordedEviction
	for _, home := range homes {
		selection, err := ReadHomeSelection(home)
		if err != nil {
			if isPyValueError(err) {
				continue
			}
			return nil, err
		}
		plugins, _ := orEmpty(selection["plugins"]).(map[string]any)
		memory, _ := orEmpty(selection["memory"]).(map[string]any)
		provider := ""
		if raw := memory["provider"]; pyrepr.Truthy(raw) {
			provider = pystr.Strip(pyrepr.Str(raw))
		}
		record, ok := plugins[DisabledByUpdateRecord].(map[string]any)
		if !ok {
			continue
		}
		disabled, _ := orEmpty(plugins["disabled"]).([]any)
		names, err := recordOrder(home)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			raw, present := record[name]
			if !present || !anyIsString(disabled, name) {
				continue
			}
			pluginDir, err := SelectedPluginDir(filepath.Join(home, "plugins"), name, "")
			if err != nil {
				if isInstallError(err) {
					continue
				}
				return nil, err
			}
			earlier, isMap := raw.(map[string]any)
			if !isMap {
				earlier = map[string]any{}
			}
			if pluginDir == "" {
				continue
			}
			keep := pyrepr.Truthy(earlier["enabled"])
			if !keep {
				spellings, err := pluginSpellings(pluginDir, name)
				if err != nil {
					return nil, err
				}
				keep = spellings[provider]
			}
			if keep {
				entries = append(entries, recordedEviction{Entry: PluginEntry{PluginsDir: filepath.Join(home, "plugins"), Name: name, PluginDir: pluginDir}, Record: earlier})
			}
		}
	}
	return entries, nil
}

// Empty mapping for a falsy decoded value, the value otherwise
func orEmpty(v any) any {
	if !pyrepr.Truthy(v) {
		return map[string]any{}
	}
	return v
}

// Reports whether a decoded list holds the string
func anyIsString(list []any, text string) bool {
	for _, item := range list {
		if s, ok := item.(string); ok && s == text {
			return true
		}
	}
	return false
}

// String keys of a home's eviction record in config file order
func recordOrder(home string) ([]string, error) {
	data, present, err := ReadBytesOrNone(filepath.Join(home, "config.yaml"))
	if err != nil || !present || len(data) == 0 {
		return nil, err
	}
	doc, err := loadRoundtrip(data)
	if err != nil {
		return nil, err
	}
	config := rtRoot(doc)
	if config == nil || config.Kind != yaml.MappingNode {
		return nil, nil
	}
	plugins := rtGet(config, "plugins")
	if plugins == nil || plugins.Kind != yaml.MappingNode {
		return nil, nil
	}
	record := rtGet(plugins, DisabledByUpdateRecord)
	if record == nil || record.Kind != yaml.MappingNode {
		return nil, nil
	}
	var names []string
	for i := 0; i+1 < len(record.Content); i += 2 {
		if key := rtDeref(record.Content[i]); key.Kind == yaml.ScalarNode && rtTag(key) == "str" {
			names = append(names, key.Value)
		}
	}
	return names, nil
}

// One home config rewrite an eviction publishes
type configEdit struct {
	path     string
	previous configBytes
	proposed []byte
}

// PluginEviction is one update's config edits, the plugins it disables and the ones it turns back on
type PluginEviction struct {
	configs configSnapshot
	edits   []configEdit
}

// One plugin change inside a home, a rejoin when reason is empty
type evictionChange struct {
	name, pluginDir, reason string
	rejoin                  bool
}

// Renders each affected home's config with the plugins in reasons disabled and the rejoined ones restored
func newPluginEviction(ctx context.Context, entries []PluginEntry, reasons map[string]string, rejoined []PluginEntry, tried map[string]string, configs configSnapshot) (*PluginEviction, error) {
	e := &PluginEviction{configs: configs}
	if configs == nil {
		snapshot, err := selectionSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		e.configs = snapshot
	}
	var homes []string
	byHome := map[string][]evictionChange{}
	add := func(home string, change evictionChange) {
		if _, seen := byHome[home]; !seen {
			homes = append(homes, home)
		}
		byHome[home] = append(byHome[home], change)
	}
	for _, entry := range entries {
		if reason, ok := reasons[resolvePath(entry.PluginDir)]; ok {
			add(filepath.Dir(entry.PluginsDir), evictionChange{name: entry.Name, pluginDir: entry.PluginDir, reason: reason})
		}
	}
	for _, entry := range rejoined {
		add(filepath.Dir(entry.PluginsDir), evictionChange{name: entry.Name, pluginDir: entry.PluginDir, rejoin: true})
	}
	for _, home := range homes {
		path := filepath.Join(home, "config.yaml")
		data, present, err := ReadBytesOrNone(path)
		if err != nil {
			return nil, err
		}
		var doc *yaml.Node
		if present && len(data) > 0 {
			if doc, err = loadRoundtrip(data); err != nil {
				return nil, err
			}
		}
		config := rtRoot(doc)
		if !rtTruthy(config) {
			doc = rtNewDocument()
			config = doc.Content[0]
		} else if config.Kind != yaml.MappingNode {
			return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", rtTag(config))}
		}
		plugins := rtGet(config, "plugins")
		if rtIsNone(plugins) {
			plugins = rtMapping()
			rtSet(config, "plugins", plugins)
		} else if plugins.Kind != yaml.MappingNode {
			return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", rtTag(plugins))}
		}
		for _, change := range byHome[home] {
			spell := func() (map[string]bool, error) { return pluginSpellings(change.pluginDir, change.name) }
			var err error
			if change.rejoin {
				err = rejoinPlugin(plugins, change.name, spell)
			} else if spellings, serr := spell(); serr != nil {
				err = serr
			} else {
				err = disablePlugin(plugins, change.name, spellings, change.reason, tried[resolvePath(change.pluginDir)])
			}
			if err != nil {
				return nil, err
			}
		}
		if err := pruneRecord(plugins); err != nil {
			return nil, err
		}
		proposed, err := dumpRoundtrip(doc)
		if err != nil {
			return nil, err
		}
		e.edits = append(e.edits, configEdit{path: path, previous: configBytes{data: data, present: present}, proposed: proposed})
	}
	return e, nil
}

// Journals every config edit and writes them, refusing any config edit since the update's snapshot
func (e *PluginEviction) Publish(ctx context.Context, project string) error {
	now, err := selectionSnapshot(ctx)
	if err != nil {
		return err
	}
	if !now.equal(e.configs) {
		return &PyError{Class: "ValueError", Message: "plugin configuration changed while preparing publication; retry"}
	}
	configs := []any{}
	for _, edit := range e.edits {
		sum := sha256.Sum256(edit.proposed)
		item := NewOrderedObject()
		item.Set("config", edit.path)
		item.Set("previous", base64Value(edit.previous))
		item.Set("config_after", hex.EncodeToString(sum[:]))
		configs = append(configs, item)
	}
	factsBefore, err := digestValue(EnvironmentRuntimeFactsPath(ctx, project))
	if err != nil {
		return err
	}
	row := NewOrderedObject()
	row.Set("configs", configs)
	row.Set("facts_before", factsBefore)
	if err := writePublicationJournal(ctx, project, row); err != nil {
		return err
	}
	for _, edit := range e.edits {
		if err := DurableWriteBytes(edit.path, edit.proposed); err != nil {
			return err
		}
	}
	return nil
}

// Last runes of a cause the way a Python negative slice keeps them
func causeTail(cause string, n int) string {
	runes := []rune(cause)
	if len(runes) > n {
		runes = runes[len(runes)-n:]
	}
	return string(runes)
}

// Why the last plugin dir cannot join the build and whether that is a verdict on it, nil when it builds
func evictionTrial(ctx context.Context, p VenvState, enabled []string, explicit bool, pluginDirs []MemberDir) (*CarriedFailure, error) {
	cause := ""
	for attempt := 0; attempt < 2; attempt++ {
		_, err := p.Prepare(ctx, enabled, VenvApplyOptions{PluginDirs: pluginDirs, Explicit: explicit, SkipInvalidSecondary: true})
		if err == nil {
			return nil, nil
		}
		var install *InstallError
		if !errors.As(err, &install) {
			return nil, err
		}
		if isBuildVerdict(err) {
			return &CarriedFailure{Reason: "the dependency environment no longer builds with it: " + causeTail(install.Cause, 400), Verdict: true}, nil
		}
		cause = install.Cause
	}
	return &CarriedFailure{Reason: "its dependencies could not be prepared, twice: " + causeTail(cause, 400)}, nil
}

// Builds the discovered selection, disabling whatever plugin keeps it from building and retrying recorded ones
func syncEvicting(ctx context.Context, p VenvState, facts *Facts, fact map[string]any, opts EvictingSync) error {
	configs, err := selectionSnapshot(ctx)
	if err != nil {
		return err
	}
	var notices []string
	homes, err := dependencyHomes(ctx)
	if err != nil {
		return err
	}
	for i, home := range homes {
		if i == 0 {
			continue
		}
		if _, err := ReadHomeSelection(home); err != nil {
			if !isPyValueError(err) {
				return err
			}
			notices = append(notices, fmt.Sprintf("Skipped the plugins of profile %s: %s; they rejoin once its config.yaml is fixed", home, pyExceptionText(err)))
		}
	}
	entries, err := enabledPluginEntries(ctx, EnabledPluginsOptions{SkipInvalidSecondary: true})
	if err != nil {
		return err
	}
	recorded, err := recordedEvictions(ctx)
	if err != nil {
		return err
	}
	base, _, _, err := selectTarget(ctx, p, fact, opts.Extras, VenvApplyOptions{PluginDirs: []MemberDir{}}, false, opts.Shipped, opts.Frozen)
	if err != nil {
		return err
	}
	all := append([]PluginEntry{}, entries...)
	for _, r := range recorded {
		all = append(all, r.Entry)
	}
	version, err := interpreterVersion(ctx)
	if err != nil {
		return err
	}
	reasons, waiting, err := staticVerdicts(ctx, all, version, base)
	if err != nil {
		return err
	}
	update, err := newEvictingUpdate(ctx, p, facts, fact, opts, base, configs, entries, recorded, reasons, waiting)
	if err != nil {
		return err
	}
	if err := update.run(ctx); err != nil {
		return err
	}
	notices = append(notices, outcomeNotices(entries, recorded, reasons, waiting, update.joined)...)
	for _, message := range notices {
		fmt.Fprintf(cliStderr(ctx), "\u26a0 %s\n", message)
		RecordWarning(ctx, message)
	}
	return nil
}

// One update's build, the enabled plugins first and then the recorded ones that are due
type evictingUpdate struct {
	pkg      VenvState
	facts    *Facts
	fact     map[string]any
	opts     EvictingSync
	base     []string
	configs  configSnapshot
	entries  []PluginEntry
	retrying []PluginEntry
	reasons  map[string]string
	tried    map[string]string
	verdicts []string
	kept     []string
	joined   []PluginEntry
	due      []PluginEntry
}

// Splits the plugins into kept members, recorded ones that rejoin statically and recorded ones due a retry
func newEvictingUpdate(ctx context.Context, p VenvState, facts *Facts, fact map[string]any, opts EvictingSync, base []string, configs configSnapshot, entries []PluginEntry, recorded []recordedEviction, reasons, waiting map[string]string) (*evictingUpdate, error) {
	u := &evictingUpdate{pkg: p, facts: facts, fact: fact, opts: opts, base: base, configs: configs, entries: entries, reasons: reasons, tried: map[string]string{}}
	fits := func(dir string) bool {
		_, disabled := reasons[resolvePath(dir)]
		_, sitting := waiting[resolvePath(dir)]
		return !disabled && !sitting
	}
	for _, r := range recorded {
		u.retrying = append(u.retrying, r.Entry)
	}
	for _, entry := range entries {
		if !fits(entry.PluginDir) {
			continue
		}
		candidate, err := declaresPackages(entry.PluginDir)
		if err != nil {
			return nil, err
		}
		if candidate && !containsString(u.kept, entry.PluginDir) {
			u.kept = append(u.kept, entry.PluginDir)
		}
	}
	type dueEntry struct {
		entry PluginEntry
		tried bool
	}
	var due []dueEntry
	for _, r := range recorded {
		if !fits(r.Entry.PluginDir) {
			continue
		}
		candidate, err := declaresPackages(r.Entry.PluginDir)
		if err != nil {
			return nil, err
		}
		if !candidate {
			u.joined = append(u.joined, r.Entry)
			continue
		}
		fingerprint, err := u.fingerprint(ctx, r.Entry.PluginDir, u.kept)
		if err != nil {
			return nil, err
		}
		if !jsonEquals(r.Record["tried"], fingerprint) {
			_, hasTried := r.Record["tried"]
			due = append(due, dueEntry{entry: r.Entry, tried: hasTried})
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return !due[i].tried && due[j].tried })
	for _, d := range due {
		u.due = append(u.due, d.entry)
	}
	return u, nil
}

// Stamp a retry of the plugin dir would build, core with the plugin and the enabled plugins beside it
func (u *evictingUpdate) fingerprint(ctx context.Context, pluginDir string, beside []string) (string, error) {
	dirs := append(append([]string{}, beside...), pluginDir)
	return u.pkg.Stamp(ctx, u.base, sameMemberDirs(dirs))
}

// Builds and records the selection with these members and publishes the config edits the update decided
func (u *evictingUpdate) commit(ctx context.Context, members []string) error {
	members = uniqueStrings(members)
	u.tried = map[string]string{}
	for _, member := range u.verdicts {
		fingerprint, err := u.fingerprint(ctx, member, members)
		if err != nil {
			return err
		}
		u.tried[resolvePath(member)] = fingerprint
	}
	enabled, stamp, inputs, err := selectTarget(ctx, u.pkg, u.fact, u.opts.Extras, VenvApplyOptions{PluginDirs: sameMemberDirs(members)}, false, u.opts.Shipped, u.opts.Frozen)
	if err != nil {
		return err
	}
	RecordFeatureList(ctx, enabled)
	var edit pluginChange
	if len(u.reasons) > 0 || len(u.joined) > 0 {
		eviction, err := newPluginEviction(ctx, append(append([]PluginEntry{}, u.entries...), u.retrying...), u.reasons, u.joined, u.tried, u.configs)
		if err != nil {
			return err
		}
		edit = eviction
	}
	current, err := runtimeStateMatches(ctx, u.fact, stamp, "")
	if err != nil {
		return err
	}
	return commitSelection(ctx, u.pkg, u.facts, edit, SelectionCommit{
		Enabled: enabled, Stamp: stamp, Inputs: inputs, Current: current,
		Explicit: u.opts.Explicit, SkipInvalidSecondary: true,
	})
}

// Plugin dirs of entries
func entryDirs(entries []PluginEntry) []string {
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		dirs = append(dirs, entry.PluginDir)
	}
	return dirs
}

// Builds with the due plugins on top, else the enabled ones alone and each due one that fits, evicting on failure
func (u *evictingUpdate) run(ctx context.Context) error {
	due := entryDirs(u.due)
	if len(due) > 0 {
		u.joined = append(u.joined, u.due...)
		err := u.commit(ctx, append(append([]string{}, u.kept...), due...))
		if err == nil {
			return nil
		}
		if !isInstallError(err) {
			return err
		}
		u.joined = u.joined[:len(u.joined)-len(u.due)]
	}
	var err error
	if len(due) == 0 {
		if err = u.commit(ctx, u.kept); err == nil {
			return nil
		}
	} else {
		_, err = u.pkg.Prepare(ctx, u.base, VenvApplyOptions{PluginDirs: sameMemberDirs(u.kept), Explicit: u.opts.Explicit, SkipInvalidSecondary: true})
	}
	if err != nil {
		if !isInstallError(err) {
			return err
		}
		return u.evictEnabled(ctx, err)
	}
	fitting := append([]string{}, u.kept...)
	for _, entry := range u.due {
		joins, err := u.tryMember(ctx, &fitting, entry.PluginDir)
		if err != nil {
			return err
		}
		if joins {
			u.joined = append(u.joined, entry)
		}
	}
	return u.commit(ctx, fitting)
}

// Adds the member to fitting when it builds on top, else records why, fingerprinting only a verdict on the plugin
func (u *evictingUpdate) tryMember(ctx context.Context, fitting *[]string, member string) (bool, error) {
	verdict, err := evictionTrial(ctx, u.pkg, u.base, u.opts.Explicit, sameMemberDirs(append(append([]string{}, *fitting...), member)))
	if err != nil {
		return false, err
	}
	if verdict == nil {
		*fitting = append(*fitting, member)
		return true, nil
	}
	u.reasons[resolvePath(member)] = verdict.Reason
	if verdict.Verdict {
		u.verdicts = append(u.verdicts, member)
	}
	return false, nil
}

// Proves core builds alone, then disables each enabled member that breaks the build and lets nothing rejoin
func (u *evictingUpdate) evictEnabled(ctx context.Context, failure error) error {
	if len(u.kept) == 0 {
		return failure
	}
	if _, err := u.pkg.Prepare(ctx, u.base, VenvApplyOptions{PluginDirs: []MemberDir{}, Explicit: u.opts.Explicit, SkipInvalidSecondary: true}); err != nil {
		if isInstallError(err) {
			return failure
		}
		return err
	}
	var joined []PluginEntry
	for _, entry := range u.joined {
		if !containsEntry(u.due, entry) {
			joined = append(joined, entry)
		}
	}
	u.joined = joined
	fitting := []string{}
	for _, member := range u.kept {
		if _, err := u.tryMember(ctx, &fitting, member); err != nil {
			return err
		}
	}
	return u.commit(ctx, fitting)
}

// Reports whether the entry is among entries
func containsEntry(entries []PluginEntry, entry PluginEntry) bool {
	for _, e := range entries {
		if e == entry {
			return true
		}
	}
	return false
}

const evictionRetry = "a later update retries it"

// Operator notices for every plugin this update disabled, left out, turned back on or kept disabled
func outcomeNotices(entries []PluginEntry, recorded []recordedEviction, reasons, waiting map[string]string, joined []PluginEntry) []string {
	var notices []string
	for _, entry := range entries {
		key := resolvePath(entry.PluginDir)
		home := filepath.Dir(entry.PluginsDir)
		if reason, ok := reasons[key]; ok {
			notices = append(notices, fmt.Sprintf("Disabled plugin '%s' in %s: %s; %s and turns it back on once it fits, or run `icebath plugins enable %s` sooner", entry.Name, home, reason, evictionRetry, entry.Name))
		} else if reason, ok := waiting[key]; ok {
			notices = append(notices, fmt.Sprintf("Left plugin '%s' in %s out of this update: %s; it stays enabled and rejoins once icebath reports a version it accepts", entry.Name, home, reason))
		}
	}
	for _, r := range recorded {
		home := filepath.Dir(r.Entry.PluginsDir)
		if containsEntry(joined, r.Entry) {
			notices = append(notices, fmt.Sprintf("Turned plugin '%s' in %s back on: it fits this icebath again", r.Entry.Name, home))
			continue
		}
		key := resolvePath(r.Entry.PluginDir)
		why := reasons[key]
		if why == "" {
			why = waiting[key]
		}
		if why == "" && pyrepr.Truthy(r.Record["reason"]) {
			why = pyrepr.Str(r.Record["reason"])
		}
		if why == "" {
			why = "unknown"
		}
		notices = append(notices, fmt.Sprintf("Plugin '%s' in %s is still disabled: %s; %s", r.Entry.Name, home, why, evictionRetry))
	}
	return notices
}
