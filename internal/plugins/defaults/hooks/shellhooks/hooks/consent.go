package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// File of the profile's folder that holds the hooks the user allowed
const AllowlistFile = "shell-hooks-allowlist.json"

// Script suffixes a command's script is found by
var scripts = []string{".sh", ".bash", ".zsh", ".fish", ".py", ".pyw", ".rb", ".pl", ".lua", ".js", ".mjs", ".cjs", ".ts"}

// Event and command of a hook, which consent is given to
type pair struct{ event, command string }

// Allowlist of the profile
func Allowlist(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), AllowlistFile)
}

// The allowlist read, and whether it can be trusted: a missing file can and a broken one cannot; either way its approvals are a list
func read(path string) (map[string]any, bool) {
	empty := map[string]any{"approvals": []any{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty, errors.Is(err, fs.ErrNotExist)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return empty, false
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil || doc == nil {
		return empty, false
	}
	if _, ok := doc["approvals"].([]any); !ok {
		doc["approvals"] = []any{}
		return doc, false
	}
	return doc, true
}

// Pairs the well formed approvals of the allowlist hold
func approved(doc map[string]any) map[pair]bool {
	out := map[pair]bool{}
	list, _ := doc["approvals"].([]any)
	for _, item := range list {
		entry, _ := item.(map[string]any)
		event, ok := entry["event"].(string)
		command, ok2 := entry["command"].(string)
		if ok && ok2 {
			out[pair{event, command}] = true
		}
	}
	return out
}

// Whether an approval is of the command, and of the event when one is given
func matches(item any, event *string, command string) bool {
	entry, ok := item.(map[string]any)
	if !ok {
		return false
	}
	if event != nil {
		if e, ok := entry["event"].(string); !ok || e != *event {
			return false
		}
	}
	c, ok := entry["command"].(string)
	return ok && c == command
}

// Approval of the hook the allowlist holds, nil when none
func Entry(cfg *config.Config, event, command string) map[string]any {
	doc, _ := read(Allowlist(cfg))
	list, _ := doc["approvals"].([]any)
	for _, item := range list {
		if matches(item, &event, command) {
			entry, _ := item.(map[string]any)
			return entry
		}
	}
	return nil
}

// Writes the allowlist whole, readable by the user alone; a write the system refuses is warned about and the approval holds for this
// process only
func save(path string, doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	err = fsx.Write(path, data, 0o600)
	var failure *fs.PathError
	var link *os.LinkError
	if errors.As(err, &failure) || errors.As(err, &link) {
		slog.Warn(fmt.Sprintf("Failed to persist shell hook allowlist to %s: %v. The approval holds for this process only (a server keeps a launch's "+
			"--accept-hooks until it restarts); other processes skip the hook, and the next run asks again.", path, err))
		return nil
	}
	return err
}

// The profile's allowlist as last read, known by its path, size and time of change
type listed struct {
	path     string
	size     int64
	changed  time.Time
	present  bool
	pairs    map[pair]bool
	readable bool
}

// Whether the allowlist file as it stands now is the one read
func (l *listed) current(path string, info os.FileInfo, err error) bool {
	switch {
	case l == nil || l.path != path:
		return false
	case err == nil:
		return l.present && l.size == info.Size() && l.changed.Equal(info.ModTime())
	default:
		return !l.present && errors.Is(err, fs.ErrNotExist)
	}
}

// Pairs of the profile's allowlist and whether it could be read, the file read again only when its size or time of change moved, so
// asking on every stream delta costs one stat
func (r *Registrant) allowlist(cfg *config.Config) (map[pair]bool, bool) {
	path := Allowlist(cfg)
	info, err := os.Stat(path)
	r.mu.Lock()
	cached := r.state(cfg.Profile()).list
	r.mu.Unlock()
	if cached.current(path, info, err) {
		return cached.pairs, cached.readable
	}
	doc, readable := read(path)
	fresh := &listed{path: path, pairs: approved(doc), readable: readable}
	switch {
	case err == nil:
		fresh.size, fresh.changed, fresh.present = info.Size(), info.ModTime(), true
	case !errors.Is(err, fs.ErrNotExist):
		return fresh.pairs, readable
	}
	r.mu.Lock()
	r.state(cfg.Profile()).list = fresh
	r.mu.Unlock()
	return fresh.pairs, readable
}

// Changes the allowlist under its lock file, so writes of several processes never lose one another's approvals; the profile's cached
// allowlist is let go, so a write never hides behind a time of change the file system did not move
func (r *Registrant) update(cfg *config.Config, change func(doc map[string]any)) error {
	path := Allowlist(cfg)
	defer func() {
		r.mu.Lock()
		r.state(cfg.Profile()).list = nil
		r.mu.Unlock()
	}()
	release, err := fsx.Lock(context.Background(), path+".lock", -1)
	if err != nil {
		return err
	}
	defer release()
	doc, _ := read(path)
	change(doc)
	return save(path, doc)
}

// Puts the hook on the allowlist, in place of any approval of it, with the time of its script now
func (r *Registrant) record(cfg *config.Config, event, command string) error {
	entry := map[string]any{"event": event, "command": command, "approved_at": stamp(time.Now()), "script_mtime_at_approval": nil}
	if mtime, ok := Mtime(command); ok {
		entry["script_mtime_at_approval"] = mtime
	}
	return r.update(cfg, func(doc map[string]any) {
		kept := []any{}
		list, _ := doc["approvals"].([]any)
		for _, item := range list {
			if !matches(item, &event, command) {
				kept = append(kept, item)
			}
		}
		doc["approvals"] = append(kept, entry)
	})
}

// Takes every approval of the command off the allowlist, how many went
func (r *Registrant) Revoke(cfg *config.Config, command string) (int, error) {
	removed := 0
	err := r.update(cfg, func(doc map[string]any) {
		list, _ := doc["approvals"].([]any)
		kept := []any{}
		for _, item := range list {
			if !matches(item, nil, command) {
				kept = append(kept, item)
			}
		}
		removed = len(list) - len(kept)
		doc["approvals"] = kept
	})
	return removed, err
}

// Instant in UTC as the allowlist writes it, its microseconds only when it has some
func stamp(t time.Time) string {
	t = t.UTC()
	out := t.Format("2006-01-02T15:04:05")
	if micro := t.Nanosecond() / 1000; micro != 0 {
		out += fmt.Sprintf(".%06d", micro)
	}
	return out + "Z"
}

// Script a command runs: its first word with a script's suffix, else its first word that looks like a path, else its first word
func script(command string) string {
	words, err := terminal.Split(command)
	if err != nil {
		return command
	}
	if len(words) == 0 {
		words = []string{command}
	}
	for _, word := range words {
		lower := strings.ToLower(word)
		for _, suffix := range scripts {
			if strings.HasSuffix(lower, suffix) {
				return word
			}
		}
	}
	for _, word := range words {
		if strings.Contains(word, "/") || strings.HasPrefix(word, "~") {
			return word
		}
	}
	return words[0]
}

// Time the command's script was last changed as the allowlist writes it, false when the script is missing
func Mtime(command string) (string, bool) {
	path := script(command)
	if path == "" {
		return "", false
	}
	info, err := os.Stat(fsx.ExpandHome(path))
	if err != nil {
		return "", false
	}
	return stamp(info.ModTime()), true
}

// Whether the command can run as it is set: a script it runs directly must be executable, one an interpreter runs readable
func Runnable(command string) bool {
	path := script(command)
	expanded := fsx.ExpandHome(path)
	if path == "" || !fsx.IsFile(expanded) {
		return false
	}
	words, err := terminal.Split(command)
	if err != nil {
		return false
	}
	return usable(expanded, len(words) > 0 && words[0] == path)
}
