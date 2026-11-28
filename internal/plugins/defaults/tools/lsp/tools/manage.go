package tools

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Fields an entry under Servers may hold: those of every server, and those only a server a person added has
var (
	shared = []string{lspconfig.Command, lspconfig.Env, lspconfig.Init, lspconfig.Disabled}
	own    = []string{lspconfig.Extensions, lspconfig.RootMarkers, lspconfig.LanguageID, lspconfig.Description}
)

// One settings section for each language server the profile knows, holding the fields of its entry under Servers: those a person added,
// then the built in ones in the order icebath knows them. Made from what is kept in the file alone, since reading any setting looks
// through these sections
type sections struct{}

func (sections) Sections(values *config.Config) []config.Section {
	stored, _ := values.Stored(lspconfig.Servers).(map[string]any)
	var out []config.Section
	add := func(id, program, about string, added bool) {
		key := lspconfig.Servers + "." + id
		out = append(out, config.Section{Key: key, Parent: lspconfig.Key, Title: id, About: about, Order: (len(out) + 1) * 10,
			Settings: lspconfig.Fields(key, program, added)})
	}
	for _, id := range slices.Sorted(maps.Keys(stored)) {
		// Its fields read by their keys, which answer to them in any case while these sections are made
		server := values.View(lspconfig.Servers).View(id)
		if _, ok := stored[id].(map[string]any); ok && added(id, server.Stored(lspconfig.Extensions)) {
			about, _ := server.Stored(lspconfig.Description).(string)
			add(id, id, strs.Filled(about, "A language server you added"), true)
		}
	}
	for _, srv := range builtins {
		add(srv.id, binOf(recipeKey(srv.id)), srv.about, false)
	}
	return out
}

// Whether an entry under Servers is a server a person added: a name no built in server has, with file types of its own
func added(id string, extensions any) bool {
	return !builtin(id) && config.Named(id) && extensions != nil
}

// Takes every write a person asks for of the servers, so the page, the command line and the settings service end in the same functions
func guard(cfg *config.Config, m *Manager) {
	cfg.Guard(lspconfig.Servers, config.Guard{Check: m.check, Write: m.write})
}

// Server and the path of a field inside its entry of a key under Servers, both empty for the whole table. The key comes as the settings keep
// it: Servers spelled as declared, the server's name as written
func placed(key string) (id, path string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(key, lspconfig.Servers), ".")
	id, path, _ = strings.Cut(rest, ".")
	return id, path
}

// Entry of a server as the profile keeps it, a copy that can be changed; empty for one with nothing kept
func entryOf(cfg *config.Config, id string) map[string]any {
	stored, _ := cfg.Stored(lspconfig.Servers + "." + id).(map[string]any)
	return cloneDeep(stored)
}

// Copy of a table and every table inside it
func cloneDeep(table map[string]any) map[string]any {
	out := make(map[string]any, len(table))
	for k, v := range table {
		if inner, ok := v.(map[string]any); ok {
			v = cloneDeep(inner)
		}
		out[k] = v
	}
	return out
}

// Entry with the value put under the dotted path inside it, nil taking it out
func put(entry map[string]any, path string, value any) map[string]any {
	parts := strings.Split(path, ".")
	node := entry
	for _, part := range parts[:len(parts)-1] {
		child, ok := node[part].(map[string]any)
		if !ok {
			if value == nil {
				return entry
			}
			child = map[string]any{}
			node[part] = child
		}
		node = child
	}
	if value == nil {
		delete(node, parts[len(parts)-1])
	} else {
		node[parts[len(parts)-1]] = value
	}
	return entry
}

// Why a write of the key would be refused, nil when it would be kept
func (m *Manager) check(cfg *config.Config, key string, value any) error {
	id, path := placed(key)
	switch {
	case id == "":
		table, _ := value.(map[string]any)
		for name, v := range table {
			entry, _ := v.(map[string]any)
			if err := checkEntry(name, entry, nil); err != nil {
				return err
			}
		}
		return nil
	case path == "":
		if value == nil {
			return nil
		}
		entry, _ := value.(map[string]any)
		return checkEntry(id, entry, nil)
	}
	return checkEntry(id, put(entryOf(cfg, id), path, value), entryOf(cfg, id))
}

// Makes a write a person asked for of a key under Servers through the function of what it changes
func (m *Manager) write(cfg *config.Config, key string, value any) (bool, error) {
	id, path := placed(key)
	switch {
	case id == "":
		return true, m.replace(cfg, value)
	case path == "" && value == nil:
		return true, m.Remove(cfg, id)
	case path == "":
		entry, _ := value.(map[string]any)
		return true, m.keep(cfg, id, entry, nil)
	}
	return true, m.Change(cfg, id, path, value)
}

// Why an entry under Servers can't be kept: a name a key can't hold, a field no server has, and for a server a person added a command and
// file types it can't run without. Before is the entry as it was when one field of it is written, nil for an entry written whole: what a
// server a person added lacked already, as only a file written by hand leaves it, is not held against the write of another field, so
// each lack can be made good in turn
func checkEntry(id string, entry, before map[string]any) error {
	_, was := before[lspconfig.Extensions]
	lacked := func(field string) bool { return was && reflect.DeepEqual(entry[field], before[field]) }
	if !config.Named(id) {
		return fmt.Errorf("“%s” can't name a language server. Use lower case letters, digits, - and _.", id)
	}
	for field := range entry {
		if !slices.Contains(shared, field) && (builtin(id) || !slices.Contains(own, field)) {
			if slices.Contains(own, field) {
				return fmt.Errorf("%s checks the files icebath knows for it, so its %s can't be changed.", id, words[field])
			}
			return fmt.Errorf("A language server has no setting called %s.", field)
		}
	}
	if builtin(id) {
		return nil
	}
	if command, ok := entry[lspconfig.Command].(string); ok && !lacked(lspconfig.Command) {
		if _, err := strs.Shell(command); err != nil {
			return fmt.Errorf("The command of %s can't be read. %s", id, err.Error())
		}
	}
	if len(commandOf(entry[lspconfig.Command])) == 0 && !lacked(lspconfig.Command) {
		return fmt.Errorf("%s needs the command that starts it, such as gopls serve.", id)
	}
	if types, ok := texts(entry[lspconfig.Extensions]); (!ok || len(types) == 0) && !lacked(lspconfig.Extensions) {
		return fmt.Errorf("%s needs the file endings it checks, such as .go.", id)
	}
	return nil
}

// Fields as a person names them
var words = map[string]string{lspconfig.Extensions: "file types", lspconfig.RootMarkers: "project files", lspconfig.LanguageID: "language name",
	lspconfig.Description: "description"}

// Keeps a server's entry once it checks out, an empty one taken out; before is the entry as it was for a write of one field, nil for an
// entry written whole
func (m *Manager) keep(cfg *config.Config, id string, entry, before map[string]any) error {
	if err := checkEntry(id, entry, before); err != nil {
		return err
	}
	var value any = entry
	if len(entry) == 0 {
		value = nil
	}
	return cfg.Set(lspconfig.Servers+"."+id, value)
}

// Keeps a whole table of servers once every entry checks out
func (m *Manager) replace(cfg *config.Config, value any) error {
	if err := m.check(cfg, lspconfig.Servers, value); err != nil {
		return err
	}
	return cfg.Set(lspconfig.Servers, value)
}

// File types a person typed: endings such as .go, or whole names of files such as Dockerfile, separated by commas or spaces, each once
func fileTypes(typed string) []string {
	var out []string
	for _, item := range strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if strings.HasPrefix(item, ".") {
			item = strings.ToLower(item)
		}
		if !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out
}

// Adds a server of your own: its name, the command that starts it, written as a shell line, and the file types it checks, all three needed
func (m *Manager) Add(cfg *config.Config, name, command, types string) error {
	name, command = strings.TrimSpace(name), strings.TrimSpace(command)
	switch {
	case name == "":
		return errors.New("Give the server a name, such as gopls.")
	case !config.Named(name):
		return errors.New("A name holds only lower case letters, digits, - and _, such as my-server.")
	case builtin(name):
		return fmt.Errorf("icebath knows a server called %s already. Give yours another name.", name)
	case added(name, entryOf(cfg, name)[lspconfig.Extensions]):
		return fmt.Errorf("You added a server called %s already. Give this one another name.", name)
	case command == "":
		return errors.New("Give the command that starts the server, such as gopls serve.")
	}
	if words, err := strs.Shell(command); err != nil {
		return fmt.Errorf("The command can't be read. %s", err.Error())
	} else if len(words) == 0 {
		return errors.New("Give the command that starts the server, such as gopls serve.")
	}
	found := fileTypes(types)
	if len(found) == 0 {
		return errors.New("Give the file endings it checks, such as .go, .mod.")
	}
	return m.keep(cfg, name, map[string]any{lspconfig.Command: command, lspconfig.Extensions: anyList(found)}, nil)
}

// Changes one field of a server, the path naming it inside the server's entry, nil putting its default back
func (m *Manager) Change(cfg *config.Config, id, path string, value any) error {
	if m.service(cfg).byID(id) == nil {
		return fmt.Errorf("No language server is called “%s”.", id)
	}
	if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
		value = nil
	}
	return m.keep(cfg, id, put(entryOf(cfg, id), path, value), entryOf(cfg, id))
}

// Turns a server off or on
func (m *Manager) Turn(cfg *config.Config, id string, on bool) error {
	var off any
	if !on {
		off = true
	}
	return m.Change(cfg, id, lspconfig.Disabled, off)
}

// Removes a server a person added, or the changes a person made to one icebath knows
func (m *Manager) Remove(cfg *config.Config, id string) error {
	if m.service(cfg).byID(id) == nil {
		return fmt.Errorf("No language server is called “%s”.", id)
	}
	return cfg.Set(lspconfig.Servers+"."+id, nil)
}

// Stops the running servers of the profile; the next edit of a file starts them again. How many ran
func (m *Manager) StopRunning(cfg *config.Config) int {
	name := cfg.Profile()
	m.mu.Lock()
	s := m.services[name]
	delete(m.services, name)
	m.mu.Unlock()
	if s == nil {
		return 0
	}
	clients, _, _ := s.snapshot()
	s.shutdown()
	return len(clients)
}

// Trusts the folders the profile's servers were kept out of, so they run the project's own programs there too; the folders trusted
func (m *Manager) Trust(cfg *config.Config) ([]string, error) {
	_, _, untrusted := m.service(cfg).snapshot()
	var roots []string
	for _, pair := range untrusted {
		if !slices.Contains(roots, pair[1]) {
			roots = append(roots, pair[1])
		}
	}
	if len(roots) == 0 {
		return nil, errors.New("No folder was skipped for not being trusted.")
	}
	return roots, tools.Trust(cfg, roots...)
}

func anyList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
