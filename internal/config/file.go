package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

// Settings of one scope: config.yaml at the top of the config folder or in a profile's folder, or what Remote Config holds for a profile
type layer interface {
	// Value under the dotted key, nil when there is none
	get(key string) any
	// Whether it holds a value under the key
	holds(key string) bool
	// Everything it holds, by key
	all() map[string]any
	// Reads it again
	load() error
	// Saves the value under the key, nil removing the key and everything under it
	set(key string, value any) error
	// Why a write can't be saved now, such as a file that does not read or parse, nil when it can
	writable() error
	// Where its writes go, as a person reads it
	where() string
	// Level of Remote Config that locks the key, empty when none does
	locked(key string) string
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
// each version of the file
type fileLayer struct {
	core *core
	path string
	mu   sync.RWMutex
	v    *viper.Viper
	// Version of the file the last read saw
	seen string
	// Whether the file was read once at all, and whether it parsed once in this process, so a broken file goes on serving what it held then
	read, good bool
}

func newFile(c *core, path string) *fileLayer {
	return &fileLayer{core: c, path: path, v: viper.New()}
}

// Settings of YAML text, an error saying why it does not parse to a mapping
func parse(data []byte) (*viper.Viper, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if _, ok := doc.(map[string]any); !ok && doc != nil {
		return nil, &shapeError{kind: kindOf(doc)}
	}
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return v, nil
}

// Document whose top is not a mapping of settings
type shapeError struct{ kind string }

func (e *shapeError) Error() string { return "top-level YAML value must be a mapping, got " + e.kind }

// Name of the kind of a YAML value, as the refusals of a file that holds one at its top name it
func kindOf(doc any) string {
	switch doc.(type) {
	case []any:
		return "list"
	case string:
		return "str"
	case bool:
		return "bool"
	case int, int64, uint64:
		return "int"
	case float64:
		return "float"
	}
	return fmt.Sprintf("%T", doc)
}

// Viper of the file as it is now, read again first when its version changed
func (f *fileLayer) current() *viper.Viper {
	f.mu.RLock()
	v, fresh := f.v, f.read && f.seen == version(f.path)
	f.mu.RUnlock()
	if fresh {
		return v
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.read || f.seen != version(f.path) {
		f.reread()
	}
	return f.v
}

func (f *fileLayer) get(key string) any {
	return f.current().Get(key)
}

func (f *fileLayer) holds(key string) bool {
	return f.current().InConfig(key)
}

func (f *fileLayer) all() map[string]any {
	return f.current().AllSettings()
}

func (f *fileLayer) where() string        { return f.path }
func (f *fileLayer) locked(string) string { return "" }

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
		f.v, f.good = viper.New(), true
		return
	}
	var next *viper.Viper
	if err == nil {
		next, err = parse(data)
	}
	if err == nil {
		f.v, f.good = next, true
		Backup(f.path, Good, BackupKeep)
		return
	}
	f.broken(err)
}

// Falls back from a read that failed and tells the person once for this version of the file, keeping a copy of a broken one
func (f *fileLayer) broken(err error) {
	fallback := "last-known-good"
	if !f.good {
		fallback, f.v = "defaults", viper.New()
		if v := newestGood(f.path); v != nil {
			f.v, fallback = v, "last-known-good-backup"
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
	tell(fmt.Sprintf("⚠️  icebath config: %s\n    Details: %s", message, details(err)), message+" Details: "+details(err), "path", f.path)
}

// Settings of the file as it is on disk now, under f.mu: none for a file that is not there, the refusal of one that does not read or parse
// rather than replacing it
func (f *fileLayer) onDisk() (map[string]any, error) {
	data, err := os.ReadFile(f.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return map[string]any{}, nil
	case err != nil:
		return nil, refuseOverwrite(f.path, "cannot be read", err, fixPerms)
	}
	on, err := parse(data)
	if shape, ok := errors.AsType[*shapeError](err); ok {
		f.warn(err, "refuse-write")
		return nil, refuseOverwrite(f.path, "must start with settings names, but its top level is "+shape.kind, err, f.fixYAML())
	}
	if err != nil {
		f.warn(err, "refuse-write")
		return nil, refuseOverwrite(f.path, "has a formatting error", err, f.fixYAML())
	}
	return on.AllSettings(), nil
}

func (f *fileLayer) writable() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.onDisk()
	return err
}

// Saves the value under the key in the file as it is on disk now, refusing a file that does not read or parse rather than replacing it
func (f *fileLayer) set(key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	settings, err := f.onDisk()
	if err != nil {
		return err
	}
	parts := strings.Split(key, ".")
	node, last := settings, len(parts)-1
	for _, part := range parts[:last] {
		child, ok := node[part].(map[string]any)
		if !ok {
			if value == nil {
				f.reread()
				return nil
			}
			child = map[string]any{}
			node[part] = child
		}
		node = child
	}
	if value == nil {
		delete(node, parts[last])
	} else {
		node[parts[last]] = value
	}
	if err := Write(f.path, settings); err != nil {
		return err
	}
	f.reread()
	return nil
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

const fixPerms = "Fix the file permissions or move it aside first."

func readFix(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return fixPerms
	}
	return "Try again; run `icebath config check` if it keeps failing."
}

// What a person reads about a config file that does not read or parse: what icebath runs on meanwhile and how to fix it
func parseFailure(path string, err error, fallback string) string {
	meanwhile := map[string]string{
		"last-known-good":        "Icebath is running on the settings it loaded before the edit until it is fixed, so recent changes are not applied.",
		"last-known-good-backup": "Icebath is running on your last good settings until it is fixed, so recent changes are not applied.",
		"refuse-write":           "Nothing was written, so the existing file is preserved.",
	}[fallback]
	if meanwhile == "" {
		meanwhile = "Icebath is running on default settings until it is fixed, so none of your saved settings are applied."
	}
	if osError(err) {
		return fmt.Sprintf("Your settings file (%s) could not be read. %s %s", path, meanwhile, readFix(err))
	}
	at, fix := "", "the problem"
	if where := location(err); where != "" {
		at, fix = " at "+where, where
	}
	return fmt.Sprintf("Your settings file (%s) has a formatting error%s. %s Open it with `icebath config edit`, fix %s, then run `icebath config check`.",
		path, at, meanwhile, fix)
}

// Write that would replace a config file icebath can't read or parse, refused with how to fix the file
type RefusedError struct{ Message string }

func (e *RefusedError) Error() string { return e.Message }

func refuseOverwrite(path, reason string, err error, fix string) error {
	at := ""
	if where := location(err); where != "" {
		at = " (" + where + ")"
	}
	return &RefusedError{fmt.Sprintf("Your settings file (%s) %s%s, so this change was not saved. %s Details: %s", path, reason, at, fix, details(err))}
}
