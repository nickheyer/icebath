package builtin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
)

const egmRoot = "elephant-goldfish-model"

// Persona files the opencode adapter registers as read only reviewers
var egmReviewers = []string{"codebase_details", "comprehension", "design_critic", "implementation_readiness", "structural", "style", "zoom_out"}

// One EGM reviewer or orchestrator persona keyed by its frontmatter name
type Persona struct {
	Name        string
	Description string
	File        string
	Reviewer    bool
	Frontmatter map[string]any
	Body        string
}

// Compiled-in Elephant-Goldfish Model plugin
type egmPlugin struct{}

func init() {
	plugins.RegisterBundled(egmPlugin{})
	plugins.RegisterBundled(hallmarkPlugin{})
	plugins.RegisterBundled(impeccablePlugin{})
}

// Manifest parsed from an embedded plugin.json with the author object flattened to its name
func manifestFromJSON(rel string) (plugins.Manifest, error) {
	data, err := payloadFile(rel)
	if err != nil {
		return plugins.Manifest{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return plugins.Manifest{}, fmt.Errorf("builtin: %s: %w", rel, err)
	}
	str := func(k string) string { s, _ := raw[k].(string); return s }
	m := plugins.Manifest{Name: str("name"), Version: str("version"), Description: str("description"),
		License: str("license"), Homepage: str("homepage"), Kind: "standalone", Source: "bundled", Raw: raw}
	if author, ok := raw["author"].(map[string]any); ok {
		m.Author, _ = author["name"].(string)
	}
	if kw, ok := raw["keywords"].([]any); ok {
		for _, k := range kw {
			if s, ok := k.(string); ok {
				m.Tags = append(m.Tags, s)
			}
		}
	}
	return m, nil
}

// Manifest of the egm plugin from its root plugin.json
func (egmPlugin) Manifest() plugins.Manifest {
	m, err := manifestFromJSON(egmRoot + "/plugin.json")
	if err != nil {
		panic(err)
	}
	return m
}

// Registers the seven EGM skills and the nine personas from the materialized host layout
func (egmPlugin) Register(ctx *plugins.Context) error {
	t, err := Materialize()
	if err != nil {
		return err
	}
	for _, dir := range egmSkillDirs() {
		content, err := payloadFile(egmRoot + "/skills/" + dir + "/SKILL.md")
		if err != nil {
			return err
		}
		fm, _ := skills.ParseFrontmatter(string(content))
		name, _ := fm["name"].(string)
		desc, _ := fm["description"].(string)
		if _, err := ctx.RegisterSkill(name, t.Host("egm/skills/"+dir+"/SKILL.md"), strings.TrimSpace(desc), fm); err != nil {
			return err
		}
	}
	personas, err := Personas()
	if err != nil {
		return err
	}
	for _, p := range personas {
		if _, err := ctx.RegisterSkill(p.Name, t.Host("egm/personas/"+p.Name+"/SKILL.md"), p.Description, p.Frontmatter); err != nil {
			return err
		}
	}
	return nil
}

// Sorted EGM skill directory names that carry a SKILL.md
func egmSkillDirs() []string {
	var out []string
	for _, rel := range payloadUnder(egmRoot + "/skills") {
		parts := strings.Split(strings.TrimPrefix(rel, egmRoot+"/skills/"), "/")
		if len(parts) == 2 && parts[1] == "SKILL.md" {
			out = append(out, parts[0])
		}
	}
	sort.Strings(out)
	return out
}

// Every EGM persona from the embedded agents directory sorted by frontmatter name
func Personas() ([]Persona, error) {
	var out []Persona
	seen := map[string]string{}
	for _, rel := range payloadUnder(egmRoot + "/agents") {
		if path.Ext(rel) != ".md" {
			continue
		}
		data, err := payloadFile(rel)
		if err != nil {
			return nil, err
		}
		fm, body := skills.ParseFrontmatter(string(data))
		name, _ := fm["name"].(string)
		desc, _ := fm["description"].(string)
		if name == "" {
			return nil, fmt.Errorf("builtin: missing canonical name: %s", rel)
		}
		file := strings.TrimPrefix(rel, egmRoot+"/")
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf("builtin: ambiguous persona name %s in %s and %s", name, prev, file)
		}
		seen[name] = file
		stem := strings.TrimSuffix(path.Base(rel), ".md")
		reviewer := false
		for _, r := range egmReviewers {
			reviewer = reviewer || r == stem
		}
		out = append(out, Persona{Name: name, Description: desc, File: file, Reviewer: reviewer, Frontmatter: fm, Body: body})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Persona registered under a frontmatter name such as egm-zoom-out
func LoadPersona(name string) (Persona, error) {
	personas, err := Personas()
	if err != nil {
		return Persona{}, err
	}
	for _, p := range personas {
		if p.Name == name {
			return p, nil
		}
	}
	return Persona{}, fmt.Errorf("builtin: no EGM persona named %s", name)
}

// Delegate task context telling a general purpose subagent to read the persona file first
func (p Persona) DelegateContext(t *Tree) string {
	bundle := t.Payload(egmRoot)
	persona := t.Payload(egmRoot + "/" + p.File)
	context := "EGM bundle root: " + bundle + ". " +
		"Resolve agents/, resources/, and docs/ against this root. " +
		"Resolve references/ against the canonical skill directory. " +
		"Resolve user files and outputs against the project working directory. "
	if !p.Reviewer {
		return context + "Read " + persona + " first and follow it as your persona."
	}
	return context + "Read " + persona + " before the target and supplied standards. " +
		"Return findings to the caller. Do not write reports or delegate. " +
		"Read prior reports only when explicitly supplied and required by the canonical stage. " +
		"Do not retrieve unsolicited history, memory, or earlier reviewer findings. " +
		"Report inaccessible required files as an incomplete review."
}

// Bundle mirror at the host root, skill and persona dirs with agents and resources beside them, links and plugin root resolved
func egmHostLayout(t *Tree, files map[string]treeFile) error {
	origin := map[string]string{}
	copyTree := func(prefix, dest string) {
		for _, rel := range payloadUnder(prefix) {
			name := path.Join(dest, strings.TrimPrefix(rel, prefix+"/"))
			files[name] = files["payload/"+rel]
			origin[name] = rel
		}
	}
	shared := func(dest string) {
		copyTree(egmRoot+"/agents", dest+"/agents")
		copyTree(egmRoot+"/resources", dest+"/resources")
	}
	copyTree(egmRoot, "host/egm")
	for _, dir := range egmSkillDirs() {
		shared("host/egm/skills/" + dir)
	}
	personas, err := Personas()
	if err != nil {
		return err
	}
	for _, p := range personas {
		dest := "host/egm/personas/" + p.Name
		files[dest+"/SKILL.md"] = files["payload/"+egmRoot+"/"+p.File]
		origin[dest+"/SKILL.md"] = egmRoot + "/" + p.File
		shared(dest)
	}
	root := []byte(t.Payload(egmRoot))
	for name, rel := range origin {
		f := files[name]
		data := f.Data
		if strings.HasSuffix(name, ".md") {
			data = egmRelinkCopy(files, name, rel, data)
		}
		if isEGMPromptBody(name) {
			data = bytes.ReplaceAll(data, egmPluginRootVar, root)
		}
		files[name] = treeFile{Data: data, Mode: f.Mode}
	}
	return nil
}

// Markdown link with its text and target
var egmLinkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^\)]+)\)`)

// Points a relative link that breaks in the host copy but resolves from its payload original at that original
func egmRelinkCopy(files map[string]treeFile, name, rel string, data []byte) []byte {
	return egmLinkRe.ReplaceAllFunc(data, func(m []byte) []byte {
		sub := egmLinkRe.FindSubmatch(m)
		file, frag, hasFrag := strings.Cut(strings.TrimSpace(string(sub[2])), "#")
		if file == "" || strings.Contains(file, ":") || path.IsAbs(file) || strings.ContainsAny(file, "{<") || treeHas(files, path.Join(path.Dir(name), file)) {
			return m
		}
		src := "payload/" + path.Join(path.Dir(rel), file)
		if !treeHas(files, src) {
			return m
		}
		target, err := filepath.Rel(path.Dir(name), src)
		if err != nil {
			return m
		}
		target = filepath.ToSlash(target)
		if strings.HasSuffix(file, "/") {
			target += "/"
		}
		if hasFrag {
			target += "#" + frag
		}
		return []byte("[" + string(sub[1]) + "](" + target + ")")
	})
}

// Reports whether a slash path names a tree file or a directory holding one
func treeHas(files map[string]treeFile, p string) bool {
	p = strings.TrimSuffix(p, "/")
	if _, ok := files[p]; ok {
		return true
	}
	for name := range files {
		if strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

// Claude plugin root variable the skill bodies use for agents, resources and docs
var egmPluginRootVar = []byte("${CLAUDE_PLUGIN_ROOT}")

// Reports whether a host file is a skill or agent body the plugin root substitution applies to
func isEGMPromptBody(name string) bool {
	if !strings.HasPrefix(name, "host/egm/skills/") && !strings.HasPrefix(name, "host/egm/personas/") {
		return false
	}
	return strings.HasSuffix(name, "/SKILL.md") || (strings.Contains(name, "/agents/") && strings.HasSuffix(name, ".md"))
}
