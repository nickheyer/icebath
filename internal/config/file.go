package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Settings of one scope: config.yaml at the top of the config folder or in a profile's folder, or what Remote Config holds for a profile.
// Every name is kept as it is spelled, and a key answers to it as keyPath says
type layer interface {
	// Value under the key, a copy, nil when there is none
	get(key keyPath) any
	// Whether it holds a value under the key
	holds(key keyPath) bool
	// Everything it holds, a copy
	all() map[string]any
	// Names of the mapping under the key in the order it holds them, none when it holds no mapping there
	order(key keyPath) []string
	// Reads it again
	load() error
	// Saves the value under the key, nil removing the key and everything under it
	set(key keyPath, value any) error
	// Why a write can't be saved now, such as a file that does not read or parse, nil when it can
	writable() error
	// Where its writes go, as a person reads it
	where() string
	// Level of Remote Config that locks the key, empty when none does
	locked(key keyPath) string
}

// Stream the warnings about the settings go to besides the log, as the person at the terminal reads them: a config file that does not read
// or parse, an administrator's file Remote Config ignores
var NoticeOut io.Writer = os.Stderr

// Handler of the log before anything sets one: the standard logger's, which writes on the terminal
var stockLog = slog.Default().Handler()

// Gives a warning about the settings on the terminal, and to the log as a record too once the log is kept apart from the terminal, so the
// person at the terminal reads it once
func tell(notice, record string, args ...any) {
	fmt.Fprintln(NoticeOut, notice)
	if slog.Default().Handler() != stockLog || log.Writer() != os.Stderr {
		slog.Warn(record, args...)
	}
}

// Version of a file as reads compare it, its time and size; empty when it is not there
func version(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

// config.yaml of one scope, read again whenever its version changes: one stat for each read of a setting, as reads cache by the version of
// the file. A read that fails keeps what the last good read gave, else the newest good copy kept beside the file, and says why once for
// each version of the file. A write changes the file where the change is and keeps the rest as the person wrote it
type fileLayer struct {
	core *core
	path string
	mu   sync.RWMutex
	// Settings of the file as last read, every name as the file spells it
	tree map[string]any
	// Names of each mapping of the file as last read in the order it writes them, by the names on the way to it
	names map[string][]string
	// Version of the file the last read saw
	seen string
	// Whether the file was read once at all, and whether it parsed once in this process, so a broken file goes on serving what it held then
	read, good bool
}

func newFile(c *core, path string) *fileLayer {
	return &fileLayer{core: c, path: path, tree: map[string]any{}}
}

// Document of YAML text and the settings it holds, every name as the text spells it; an error saying why it does not parse to a mapping
func parse(data []byte) (*yaml.Node, map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	node := content(&doc)
	if node == nil {
		return &doc, map[string]any{}, nil
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, nil, err
	}
	settings, ok := plain(value).(map[string]any)
	if !ok {
		return nil, nil, &shapeError{kind: kindOf(value)}
	}
	return &doc, settings, nil
}

// Why the text of a config file gives no settings, nil when a read of it does: the same reading every config.yaml is read with
func Problem(data []byte) error {
	_, _, err := parse(data)
	return err
}

// Document whose top is not a mapping of settings
type shapeError struct{ kind string }

func (e *shapeError) Error() string {
	return "the file must hold named settings, and it holds " + e.kind
}

// What a YAML value is, in the words the refusals of a file that holds one at its top say it in
func kindOf(doc any) string {
	switch doc.(type) {
	case []any:
		return "a list"
	case string:
		return "plain text"
	case bool:
		return "true or false"
	case int, int64, uint64, float64:
		return "a number"
	}
	return "something else"
}

// Settings of the file as it is now and the order of their names, read again first when its version changed
func (f *fileLayer) current() (map[string]any, map[string][]string) {
	f.mu.RLock()
	tree, names, fresh := f.tree, f.names, f.read && f.seen == version(f.path)
	f.mu.RUnlock()
	if fresh {
		return tree, names
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.read || f.seen != version(f.path) {
		f.reread()
	}
	return f.tree, f.names
}

func (f *fileLayer) get(key keyPath) any {
	tree, _ := f.current()
	value, _ := lookup(tree, key)
	return copied(value)
}

func (f *fileLayer) holds(key keyPath) bool {
	tree, _ := f.current()
	value, ok := lookup(tree, key)
	return ok && value != nil
}

func (f *fileLayer) all() map[string]any {
	tree, _ := f.current()
	all, _ := copied(tree).(map[string]any)
	return all
}

func (f *fileLayer) order(key keyPath) []string {
	tree, names := f.current()
	var node any = tree
	var held []string
	for i, part := range key.parts {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		name, ok := pickIn(m, part, key.match[i])
		if !ok {
			return nil
		}
		held, node = append(held, name), m[name]
	}
	m, _ := node.(map[string]any)
	return ordered(m, names[strings.Join(held, "\x00")])
}

// Names of the mapping in the order given, those the order lacks, such as names a << takes from another mapping, after them in order
func ordered(m map[string]any, order []string) []string {
	var out []string
	for _, name := range order {
		if _, ok := m[name]; ok && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func (f *fileLayer) where() string         { return f.path }
func (f *fileLayer) locked(keyPath) string { return "" }

func (f *fileLayer) load() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reread()
	return nil
}

// Reads the file under f.mu: a good read keeps a copy of it, a failed one falls back and is told
func (f *fileLayer) reread() {
	f.read, f.seen = true, version(f.path)
	data, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		f.tree, f.names, f.good = map[string]any{}, nil, true
		return
	}
	var doc *yaml.Node
	var next map[string]any
	if err == nil {
		doc, next, err = parse(data)
	}
	if err == nil {
		f.tree, f.names, f.good = next, orderOf(doc), true
		Backup(f.path, Good, BackupKeep)
		return
	}
	f.broken(err)
}

// Falls back from a read that failed and tells the person once for this version of the file, keeping a copy of a broken one
func (f *fileLayer) broken(err error) {
	fallback := "last-known-good"
	if !f.good {
		fallback, f.tree, f.names = "defaults", map[string]any{}, nil
		if doc, tree := newestGood(f.path); tree != nil {
			f.tree, f.names, fallback = tree, orderOf(doc), "last-known-good-backup"
		}
	}
	f.warn(err, fallback)
}

// Tells once for this version of the file why it does not read or parse and what icebath does meanwhile, keeping a copy of a broken one
func (f *fileLayer) warn(err error, fallback string) {
	if !f.core.once("parse", f.path, version(f.path)) {
		return
	}
	message := parseFailure(f.path, err, fallback)
	if !osError(err) {
		if copied := Backup(f.path, Corrupt, BackupKeep); copied != "" {
			message += " A copy of the broken file was saved to " + copied + "."
		}
	}
	tell(fmt.Sprintf("⚠ %s\n  The error: %s.", message, details(err)), message+" The error: "+details(err)+".", "path", f.path)
}

// The file as it is on disk now, under f.mu: its text, its document and the settings it holds; nothing for a file that is not there, the
// refusal of one that does not read or parse rather than replacing it
func (f *fileLayer) onDisk() ([]byte, *yaml.Node, map[string]any, error) {
	data, err := os.ReadFile(f.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, &yaml.Node{}, map[string]any{}, nil
	case err != nil:
		return nil, nil, nil, refuseOverwrite(f.path, "can't be read", err, fixPerms)
	}
	doc, settings, err := parse(data)
	if shape, ok := errors.AsType[*shapeError](err); ok {
		f.warn(err, "refuse-write")
		return nil, nil, nil, refuseOverwrite(f.path, "must hold named settings, and it holds "+shape.kind, err, f.fixYAML())
	}
	if err != nil {
		f.warn(err, "refuse-write")
		return nil, nil, nil, refuseOverwrite(f.path, "has a formatting error", err, f.fixYAML())
	}
	return data, doc, settings, nil
}

func (f *fileLayer) writable() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _, _, err := f.onDisk()
	return err
}

// Saves the value under the key in the file as it is on disk now, refusing a file that does not read or parse rather than replacing it
func (f *fileLayer) set(key keyPath, value any) error {
	return f.amend(keyPath{}, key, value, nil)
}

// Saves the value as set does while the file on disk keeps the table at within, writing nothing once that table is gone; always for no
// within. The file is changed where the key is and kept as the person wrote it elsewhere, with every name the schema declares spelled as
// declared
func (f *fileLayer) amend(within, key keyPath, value any, declares *declared) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	text, doc, settings, err := f.onDisk()
	if err != nil {
		return err
	}
	if held, _ := lookup(settings, within); len(within.parts) > 0 && !isTable(held) {
		f.reread()
		return nil
	}
	empty := content(doc) == nil
	mapping := top(doc)
	declares.respell(mapping)
	if value == nil {
		if !remove(mapping, key) {
			f.reread()
			return nil
		}
	} else {
		place := declares.walk(key.parts)
		var fresh yaml.Node
		if err := fresh.Encode(place.canon(value)); err != nil {
			return err
		}
		put(mapping, key, &fresh, place)
	}
	settle(doc)
	data, err := render(doc, text, empty)
	if err != nil {
		return err
	}
	if err := fsx.Write(f.path, data, 0o600); err != nil {
		return err
	}
	f.reread()
	return nil
}

// Whether a value is a table of named values
func isTable(value any) bool {
	_, ok := value.(map[string]any)
	return ok
}

// How to fix a config file that does not parse, naming the folder of its good copies
func (f *fileLayer) fixYAML() string {
	return fmt.Sprintf("Fix it with `icebath config edit` and check with `icebath config check`, or copy the newest good file from %s over config.yaml.",
		display(BackupsDir(f.path))+"/")
}

// Line a YAML error points at, and the same leading its text
var (
	yamlLine   = regexp.MustCompile(`line (\d+)`)
	yamlPrefix = regexp.MustCompile(`^(?:While parsing config: )?(?:yaml: )?(?:line \d+: )?`)
)

// Line the error points at, such as line 12, empty when it names none
func location(err error) string {
	if m := yamlLine.FindStringSubmatch(err.Error()); m != nil {
		return "line " + m[1]
	}
	return ""
}

// One line of what the error says, its YAML problem without the line it points at
func details(err error) string {
	return strings.Join(strings.Fields(yamlPrefix.ReplaceAllString(err.Error(), "")), " ")
}

// Whether the error is the system's rather than a parse failure
func osError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) || errors.As(err, &errno)
}

const fixPerms = "Fix the permissions of the file, or move the file aside."

func readFix(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return fixPerms
	}
	return "Try again, and run `icebath config check` if it keeps failing."
}

// What a person reads about a config file that does not read or parse: what icebath runs on meanwhile and how to fix it
func parseFailure(path string, err error, fallback string) string {
	meanwhile := map[string]string{
		"last-known-good":        "Until it is fixed, icebath uses the settings it had before the edit, so recent changes have no effect.",
		"last-known-good-backup": "Until it is fixed, icebath uses the last settings that worked, so recent changes have no effect.",
		"refuse-write":           "Nothing was saved, so the file is as it was.",
	}[fallback]
	if meanwhile == "" {
		meanwhile = "Until it is fixed, icebath uses the default settings, so none of your saved settings have an effect."
	}
	if osError(err) {
		return fmt.Sprintf("Your settings file %s could not be read. %s %s", path, meanwhile, readFix(err))
	}
	at, fix := "", "the problem"
	if where := location(err); where != "" {
		at, fix = " at "+where, where
	}
	return fmt.Sprintf("Your settings file %s has a formatting error%s. %s Open it with `icebath config edit`, fix %s, then run `icebath config check`.",
		path, at, meanwhile, fix)
}

// Write that would replace a config file icebath can't read or parse, refused with how to fix the file
type RefusedError struct{ Message string }

func (e *RefusedError) Error() string { return e.Message }

func refuseOverwrite(path, reason string, err error, fix string) error {
	at := ""
	if where := location(err); where != "" {
		at = " at " + where
	}
	return &RefusedError{fmt.Sprintf("This change was not saved, because your settings file %s %s%s. %s The error: %s.", path, reason, at, fix, details(err))}
}
