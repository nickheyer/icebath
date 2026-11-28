package speech

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

// Programs of yours of one kind: where their entries are kept, the placeholders a command must name and the services whose names they
// can't take
type Programs struct {
	// Section they may also sit directly under, and the table that holds them
	Section, Table string
	// Placeholders a command must name, each a set of which one will do, with what goes there
	Needs []Placeholder
	// Setting that chooses the service in use
	Choice string
	// Names of the services of the kind registered now, whether each is a program of yours; the first registered of a name wins
	names func(cfg *config.Config) map[string]bool
}

// A placeholder a command must name: one of the names will do
type Placeholder struct {
	Names []string
	// What icebath puts there, after "where"
	Holds string
}

// Programs of yours that read text aloud
func (s *Speech) SpeechPrograms() Programs {
	return Programs{Section: speechconfig.TTS, Table: speechconfig.TTSCommands, Choice: speechconfig.TTSProvider,
		Needs: []Placeholder{{Names: []string{"input_path", "text_path"}, Holds: "the file of the text to speak goes"},
			{Names: []string{"output_path"}, Holds: "the audio file it writes goes"}},
		names: func(cfg *config.Config) map[string]bool {
			out := map[string]bool{}
			for _, v := range s.Voices(cfg) {
				if _, seen := out[v.Name]; !seen {
					out[v.Name] = v.Program
				}
			}
			return out
		}}
}

// Programs of yours that turn a recording into text
func (s *Speech) RecognitionPrograms() Programs {
	return Programs{Section: speechconfig.STT, Table: speechconfig.STTCommands, Choice: speechconfig.STTProvider,
		Needs: []Placeholder{{Names: []string{"input_path"}, Holds: "the recording goes"}},
		names: func(cfg *config.Config) map[string]bool {
			out := map[string]bool{None: false}
			for _, r := range s.Recognizers(cfg) {
				if _, seen := out[r.Name]; !seen {
					out[r.Name] = r.Program
				}
			}
			return out
		}}
}

// Placeholder of a command: a name in braces, single or doubled, no dollar before it
func placeholder(name string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^$])\{\{?` + regexp.QuoteMeta(name) + `\}\}?`)
}

// Why a command can't run as a program of this kind, nil when it can: empty, or naming none of the names of a placeholder it needs
func (p Programs) Command(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return errors.New("A program needs a command.")
	}
	for _, need := range p.Needs {
		if !slices.ContainsFunc(need.Names, func(name string) bool { return placeholder(name).MatchString(command) }) {
			return fmt.Errorf("The command must name {%s}, where %s.", need.Names[0], need.Holds)
		}
	}
	return nil
}

// Why a name can't be a new program's, nil when it can: not a name a key holds, or the name of a service there is already
func (p Programs) Name(cfg *config.Config, name, old string) error {
	switch {
	case name == "":
		return errors.New("A program needs a name.")
	case !config.Named(name) || name == "providers":
		return errors.New("A program's name holds only lower case letters, digits, - and _, such as my-whisper.")
	}
	if program, taken := p.names(cfg)[name]; taken && name != old {
		if program {
			return fmt.Errorf("You have a program called %s already.", name)
		}
		return fmt.Errorf("%s is the name of a service of icebath. Give your program another name.", name)
	}
	return nil
}

// Why an entry can't be a program of this kind, nil when it can: its command, and each field it holds as the entry's fields take it
func (p Programs) entry(cfg *config.Config, name string, value any) error {
	entry, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("The program %s must be a set of named values.", name)
	}
	if kind := strings.ToLower(strings.TrimSpace(cast.ToString(entry["type"]))); kind != "" && kind != "command" {
		return fmt.Errorf("The program %s can only be of the type command.", name)
	}
	if err := p.Command(cast.ToString(entry["command"])); err != nil {
		return fmt.Errorf("%s %w", "The program "+name+" can't run.", err)
	}
	_, table, _ := cfg.Setting(p.Table)
	if table.Entry == nil {
		return nil
	}
	for _, field := range table.Entry.Fields {
		if held, ok := entry[field.Key]; ok && held != nil {
			if _, err := field.Check(held); err != nil {
				return err
			}
		}
	}
	return nil
}

// Programs of yours as the settings keep them after a write a person asked for of the key: the whole table, one program or one field of one
func (p Programs) after(cfg *config.Config, key string, value any) (map[string]any, []string) {
	table := map[string]any{}
	for name, entry := range cast.ToStringMap(cfg.Stored(p.Table)) {
		table[name] = clone(entry)
	}
	rest, _ := strings.CutPrefix(key, p.Table)
	parts := strings.Split(strings.TrimPrefix(rest, "."), ".")
	switch {
	case rest == "":
		table = map[string]any{}
		for name, entry := range cast.ToStringMap(value) {
			table[name] = entry
		}
		return table, slices.Collect(maps.Keys(table))
	case len(parts) == 1:
		if value == nil {
			delete(table, parts[0])
			return table, nil
		}
		table[parts[0]] = value
	default:
		entry, _ := table[parts[0]].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		if value == nil {
			delete(entry, parts[1])
		} else {
			entry[parts[1]] = value
		}
		table[parts[0]] = entry
	}
	return table, []string{parts[0]}
}

// Deep copy of a value read from the settings
func clone(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, inner := range v {
			out[key] = clone(inner)
		}
		return out
	case []any:
		return slices.Clone(v)
	}
	return value
}

// Guard of the table of programs: a write that would keep a program that can't run is refused, whichever way it comes
func (p Programs) Guard() config.Guard {
	return config.Guard{Check: func(cfg *config.Config, key string, value any) error {
		table, changed := p.after(cfg, key, value)
		for _, name := range changed {
			if !config.Named(name) || name == "providers" {
				return errors.New("A program's name holds only lower case letters, digits, - and _, such as my-whisper.")
			}
			if program, taken := p.names(cfg)[name]; taken && !program {
				return fmt.Errorf("%s is the name of a service of icebath. Give your program another name.", name)
			}
			if err := p.entry(cfg, name, table[name]); err != nil {
				return err
			}
		}
		return nil
	}}
}

// Key of a program of yours as the settings keep it: under the table, or directly under the section when it sits there; empty for a name
// no program has
func (p Programs) Key(cfg *config.Config, name string) string {
	if _, ok := cast.ToStringMap(cfg.Stored(p.Table))[name]; ok {
		return p.Table + "." + name
	}
	if _, ok := cast.ToStringMap(cfg.Stored(p.Section))[name].(map[string]any); ok && strings.TrimSpace(cfg.GetString(p.Section+"."+name+".command")) != "" {
		return p.Section + "." + name
	}
	return ""
}

// Adds a program of yours with its command, after the checks of its name and command
func (p Programs) Add(cfg *config.Config, name, command string) error {
	name = Normal(name)
	if err := p.Name(cfg, name, ""); err != nil {
		return err
	}
	if err := p.Command(command); err != nil {
		return err
	}
	return cfg.Change(p.Table+"."+name, map[string]any{"command": strings.TrimSpace(command)})
}

// Changes one field of a program of yours, null putting its default back; refused when the program could then not run
func (p Programs) Change(cfg *config.Config, name, field string, value any) error {
	key := p.Key(cfg, name)
	if key == "" {
		return fmt.Errorf("You have no program called %s.", name)
	}
	if !strings.HasPrefix(key, p.Table+".") {
		entry, _ := clone(cfg.Stored(key)).(map[string]any)
		if value == nil {
			delete(entry, field)
		} else {
			entry[field] = value
		}
		if err := p.entry(cfg, name, entry); err != nil {
			return err
		}
		return cfg.Set(key, entry)
	}
	return cfg.Change(key+"."+field, value)
}

// Gives a program of yours another name, the choice of it following; whether the settings chose it
func (p Programs) Rename(cfg *config.Config, name, to string) (bool, error) {
	to = Normal(to)
	key := p.Key(cfg, name)
	if key == "" {
		return false, fmt.Errorf("You have no program called %s.", name)
	}
	if to == name {
		return false, nil
	}
	if err := p.Name(cfg, to, name); err != nil {
		return false, err
	}
	entry := clone(cfg.Stored(key))
	if strings.HasPrefix(key, p.Table+".") {
		table, _ := clone(cfg.Stored(p.Table)).(map[string]any)
		delete(table, name)
		table[to] = entry
		if err := cfg.View(p.Table).Replace(table); err != nil {
			return false, err
		}
	} else {
		if err := cfg.Change(p.Table+"."+to, entry); err != nil {
			return false, err
		}
		if err := cfg.Set(key, nil); err != nil {
			return false, err
		}
	}
	if Chosen(cfg, p.Choice) != name {
		return false, nil
	}
	return true, cfg.Change(p.Choice, to)
}

// Removes a program of yours, the choice of it going back to its default; whether the settings chose it
func (p Programs) Remove(cfg *config.Config, name string) (bool, error) {
	key := p.Key(cfg, name)
	if key == "" {
		return false, fmt.Errorf("You have no program called %s.", name)
	}
	if err := cfg.Set(key, nil); err != nil {
		return false, err
	}
	if Chosen(cfg, p.Choice) != name {
		return false, nil
	}
	return true, cfg.Change(p.Choice, nil)
}
