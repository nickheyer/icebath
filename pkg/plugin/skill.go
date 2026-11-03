package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Characters of description and when to use shown for one skill in the listing
const DescriptionCap = 1536

// Where a component was found, deciding its precedence and its name
const (
	OriginManaged = "managed"
	OriginUser    = "user"
	OriginProject = "project"
	OriginNested  = "nested"
	OriginAdded   = "added"
	OriginPlugin  = "plugin"
)

// Instructions a person or the model loads by name, with the files beside them
type Skill struct {
	// Full name the skill is invoked by, plugin:name for a plugin skill
	Name string
	// Name alone without any plugin or directory prefix
	Short string
	// Names the skill also answers to, the short one when no other skill claims it
	Aliases      []string
	Description  string
	WhenToUse    string
	ArgumentHint string
	// Named positional arguments, each substituted for $name
	Arguments []string
	// Never loaded by the model, and left out of its listing
	DisableModelInvocation bool
	// Offered in the slash menu and runnable by name
	UserInvocable bool
	// Tools allowed without asking for the turn that invokes the skill
	AllowedTools []string
	// Tools withheld while the skill is active
	DisallowedTools []string
	Model           string
	Effort          string
	// Runs in a subagent when fork
	Context string
	// Subagent type a forked skill runs as, general-purpose when empty
	Agent string
	// A forked skill waits for its answer in the invoking turn
	Background *bool
	// Hooks registered when the skill is invoked and kept for the session
	Hooks agent.HookConfig
	// Globs of files whose use loads the skill on its own
	Paths []string
	// Shell of the inline commands, bash or powershell
	Shell         string
	Metadata      map[string]string
	License       string
	Compatibility string
	Version       string
	// Operating systems the skill runs on, every one when empty
	Platforms []string
	// Markdown after the frontmatter, before any substitution
	Body string
	// Directory the SKILL.md sits in, the directory of the file for a command
	Dir  string
	File string
	// Plugin the skill came with, nil for a loose one
	Plugin *Plugin
	Origin string
	// Directory the skill was discovered under, the added directory for an added one
	Root string
	// Read from commands, the format before skills
	Legacy bool
	// Errors found reading the skill, kept so a listing can show them
	Problems []string
}

// Reads a SKILL.md, the directory naming the skill unless the frontmatter does
func ReadSkill(path string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := ParseSkill(string(data), path)
	return s, nil
}

// Parses skill text from a file path, the directory name standing in for a missing name
func ParseSkill(text, path string) *Skill {
	f := ParseFront(text)
	dir := filepath.Dir(path)
	s := &Skill{Dir: dir, File: path, Body: f.Body, UserInvocable: true}
	if f.Err != nil {
		s.Problems = append(s.Problems, "frontmatter does not parse: "+f.Err.Error())
	}
	s.Short = strings.TrimSpace(f.String("name"))
	if s.Short == "" {
		s.Short = filepath.Base(dir)
	}
	s.Description = strings.TrimSpace(f.String("description"))
	if s.Description == "" {
		s.Description = firstLine(f.Body)
	}
	s.WhenToUse = strings.TrimSpace(f.String("when_to_use"))
	s.ArgumentHint = strings.TrimSpace(f.String("argument-hint"))
	s.Arguments = f.List("arguments")
	s.DisableModelInvocation = f.Bool("disable-model-invocation", false)
	s.UserInvocable = f.Bool("user-invocable", true)
	s.AllowedTools = f.Rules("allowed-tools")
	s.DisallowedTools = f.Rules("disallowed-tools")
	s.Model = strings.TrimSpace(f.String("model"))
	s.Effort = strings.TrimSpace(f.String("effort"))
	s.Context = strings.TrimSpace(f.String("context"))
	s.Agent = strings.TrimSpace(f.String("agent"))
	if f.Has("background") {
		b := f.Bool("background", true)
		s.Background = &b
	}
	s.Paths = f.List("paths")
	s.Shell = strings.TrimSpace(f.String("shell"))
	s.License = strings.TrimSpace(f.String("license"))
	s.Compatibility = strings.TrimSpace(f.String("compatibility"))
	s.Version = strings.TrimSpace(f.String("version"))
	s.Platforms = f.List("platforms")
	if m := f.Map("metadata"); m != nil {
		s.Metadata = map[string]string{}
		for k, v := range m {
			s.Metadata[k] = scalarOrJSON(v)
		}
	}
	if raw := f.Map("hooks"); raw != nil {
		cfg, err := hooksFromMap(raw)
		if err != nil {
			s.Problems = append(s.Problems, "hooks: "+err.Error())
		} else {
			s.Hooks = cfg.Labeled(agent.SourceSkill, s.Short, nil)
		}
	}
	for _, key := range []string{"context", "shell", "effort"} {
		if err := checkChoice(key, f.String(key), choices[key]); err != nil {
			s.Problems = append(s.Problems, err.Error())
		}
	}
	if len(s.Compatibility) > 500 {
		s.Problems = append(s.Problems, "compatibility is over 500 characters")
	}
	if len(s.Short) > 64 {
		s.Problems = append(s.Problems, "name is over 64 characters")
	}
	return s
}

// Values each choice field may take
var choices = map[string][]string{
	"context": {"", "fork"},
	"shell":   {"", "bash", "powershell"},
	"effort":  {"", "low", "medium", "high", "xhigh", "max"},
}

// Checks that a value is one of the choices, the first choice being empty for unset
func checkChoice(key, value string, allowed []string) error {
	for _, a := range allowed {
		if a == value {
			return nil
		}
	}
	return fmt.Errorf("%s %q is not one of %s", key, value, strings.Join(allowed[1:], ", "))
}

// Reads a command file, the file name giving the name and any subdirectory a namespace
func ParseCommand(text, path, base string) *Skill {
	s := ParseSkill(text, path)
	s.Legacy = true
	s.Dir = filepath.Dir(path)
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	rel, err := filepath.Rel(base, filepath.Dir(path))
	if err == nil && rel != "." {
		name = strings.ReplaceAll(filepath.ToSlash(rel), "/", ":") + ":" + name
	}
	s.Short = name
	s.Paths = nil
	return s
}

// First non empty line of a text with markdown heading marks removed
func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			return line
		}
	}
	return ""
}

// Reports whether a skill name holds no refused character, blank runs, control, private or bidi characters
func RefusedName(name string) bool {
	if name == "" || strings.HasPrefix(name, " ") || strings.HasSuffix(name, " ") || strings.Contains(name, "  ") {
		return true
	}
	for _, r := range name {
		switch {
		case r == '<' || r == '>':
			return true
		case r == '​' || r == ' ':
			return true
		case unicode.IsControl(r), unicode.Is(unicode.Co, r), !unicode.IsPrint(r) && r != ' ':
			return true
		case r == '‪' || r == '‫' || r == '‬' || r == '‭' || r == '‮' || r == '⁦' || r == '⁧' || r == '⁨' || r == '⁩':
			return true
		case unicode.IsSpace(r) && r != ' ':
			return true
		}
	}
	return false
}

// Name with every refused character turned into a hyphen, as plugin skill directories are read
func SafeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '<' || r == '>', r == '​', r == ' ', unicode.IsControl(r), unicode.Is(unicode.Co, r), unicode.IsSpace(r) && r != ' ':
			b.WriteByte('-')
		case r == '‪' || r == '‫' || r == '‬' || r == '‭' || r == '‮' || r == '⁦' || r == '⁧' || r == '⁨' || r == '⁩':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), " ")
}

// Text of the skill in its listing, the description and when to use together cut to the cap
func (s *Skill) Listing() string {
	text := s.Description
	if s.WhenToUse != "" {
		text = strings.TrimSpace(text + " " + s.WhenToUse)
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "<", "&lt;"), ">", "&gt;")
	if len(text) > DescriptionCap {
		text = text[:DescriptionCap]
	}
	return text
}

// Reports whether the skill runs on this operating system
func (s *Skill) Supported(goos string) bool {
	if len(s.Platforms) == 0 {
		return true
	}
	for _, p := range s.Platforms {
		switch strings.ToLower(p) {
		case goos, "all", "any":
			return true
		case "macos", "mac", "osx":
			if goos == "darwin" {
				return true
			}
		}
	}
	return false
}

// Reports whether the model may load the skill on its own
func (s *Skill) ModelInvocable() bool { return !s.DisableModelInvocation }

// Reports whether the skill runs in a subagent
func (s *Skill) Forked() bool { return s.Context == "fork" }

// Reports whether a forked skill waits for its answer
func (s *Skill) Waits() bool { return s.Background != nil && !*s.Background }

// Name of a plugin skill, plugin:name unless the name already starts with the plugin prefix
func pluginName(plugin, name string) string {
	if strings.HasPrefix(name, plugin+":") {
		return name
	}
	return plugin + ":" + name
}

// Reads every skill under a skills directory, each immediate child holding a SKILL.md
func readSkillDir(dir string) ([]*Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Skill
	for _, e := range entries {
		if strings.EqualFold(e.Name(), "synced") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		st, err := os.Stat(path)
		if err != nil || !st.IsDir() {
			continue
		}
		file := filepath.Join(path, "SKILL.md")
		if _, err := os.Stat(file); err != nil {
			continue
		}
		s, err := ReadSkill(file)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// Reads every command file under a commands directory, subdirectories adding namespace segments
func readCommandDir(dir string) ([]*Skill, error) {
	var out []*Skill
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == dir {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, ParseCommand(string(data), path, dir))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// Text of a scalar, or the JSON of anything structured
func scalarOrJSON(v any) string {
	if s := scalar(v); s != "" {
		return s
	}
	return jsonText(v)
}

func sortStrings(s []string) { sort.Strings(s) }
