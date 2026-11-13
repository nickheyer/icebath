package config

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Persona text seeded into SOUL.md on first run and used by sessions that have no SOUL.md
const DefaultSoul = "You are icebath, built by heyer.app. Be direct: match the length of your reply to the weight of " +
	"the ask — a one-line question gets a one-line answer, and finished work gets a short report of what " +
	"changed, what's verified, and what's left, never a replay of the process. No filler (\"Great question,\" " +
	"\"I'd be happy to\"), no restating the request back, no re-summarizing what you already said, no narrating " +
	"tool calls the user can see. Plain claims over adjectives; when unsure, say so plainly. Agree because it's " +
	"right, not because the user said it. Depth is earned — give it when the user asks for detail, teaches, or " +
	"the stakes demand it, not by default."

// Opening and closing of the comment only persona scaffold older installers wrote
const (
	soulScaffoldHead = "# icebath Persona\n\n<!--\nThis file defines the agent's personality and tone.\n" +
		"The agent will embody whatever you write here.\nEdit this to customize how icebath communicates with you.\n\n"
	soulScaffoldTail = "This file is loaded fresh each message -- no restart needed.\n" +
		"Delete the contents (or this file) to use the default personality.\n-->"
)

// Seeded persona texts that carry no user intent, safe to upgrade in place
func legacyTemplateSouls() []string {
	return []string{
		soulScaffoldHead + "Examples:\n" +
			"  - \"You are a warm, playful assistant who uses kaomoji occasionally.\"\n" +
			"  - \"You are a concise technical expert. No fluff, just facts.\"\n" +
			"  - \"You speak like a friendly coworker who happens to know everything.\"\n\n" + soulScaffoldTail,
		soulScaffoldHead + soulScaffoldTail,
		"You are icebath, an intelligent AI assistant created by heyer.app. You are helpful, " +
			"knowledgeable, and direct. You assist users with a wide range of tasks including answering questions, " +
			"writing and editing code, analyzing information, creative work, and executing actions via your tools. " +
			"You communicate clearly, admit uncertainty when appropriate, and prioritize being genuinely useful over " +
			"being verbose unless otherwise directed below. Be targeted and efficient in your exploration and " +
			"investigations.",
		strings.ReplaceAll(DefaultSoul, "—", "--"),
	}
}

// Text with line endings unified, a leading byte order mark dropped and surrounding whitespace trimmed
func normalizeSoul(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	return strings.TrimSpace(strings.TrimLeft(text, "\xef\xbb\xbf"))
}

// Reports a SOUL.md that still holds an automatically seeded text nobody edited
func IsLegacyTemplateSoul(text string) bool {
	return slices.ContainsFunc(legacyTemplateSouls(), func(t string) bool {
		return normalizeSoul(text) == normalizeSoul(t)
	})
}

// Ensures the home of ctx exists with its directory skeleton, its config.yaml and its default persona
func (s *Store) EnsureHome(ctx context.Context) error {
	return s.homes.EnsureHome(ctx, func(h string) error {
		if err := s.seedConfig(ctx, h); err != nil {
			return err
		}
		return ensureDefaultSoul(ctx, h)
	})
}

// Writes the commented template as config.yaml when the home has none, unless the install is managed or config lives elsewhere
func (s *Store) seedConfig(ctx context.Context, h string) (err error) {
	path := home.Config.In(h)
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if home.ManagedSystem(h) != "" {
		return nil
	}
	b, err := s.backend(ctx)
	if err != nil || !b.SupportsFileTooling() {
		return err
	}
	ctx, release, err := s.file.lockFile(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	text, err := configTemplate()
	if err != nil {
		return err
	}
	if err := fileutil.WriteText(ctx, path, text, fileutil.KeepPerm()); err != nil {
		return err
	}
	home.ApplySecureFilePolicy(path, h)
	return nil
}

// Creates the config.yaml of ctx from the commented template when it is missing, reporting whether it was written
func (s *Store) SeedConfigFile(ctx context.Context) (bool, error) {
	h, path, err := s.configPath(ctx)
	if err != nil {
		return false, err
	}
	if err := s.homes.Mkdir(ctx, h); err != nil {
		return false, err
	}
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	}
	if err := s.seedConfig(ctx, h); err != nil {
		return false, err
	}
	_, err = os.Lstat(path)
	return err == nil, nil
}

// Seeds the default persona, upgrading an untouched older seed and replacing a link that cannot hold a file
func ensureDefaultSoul(ctx context.Context, h string) error {
	path := home.Soul.In(h)
	if _, err := os.Stat(path); err == nil {
		text, err := fileutil.ReadText(path)
		if err != nil || !IsLegacyTemplateSoul(text) {
			return nil
		}
	}
	err := fileutil.WriteFile(ctx, path, []byte(DefaultSoul), fileutil.KeepOwner())
	if err != nil {
		fi, lerr := os.Lstat(path)
		if lerr != nil || fi.Mode()&fs.ModeSymlink == 0 {
			return err
		}
		f, terr := os.CreateTemp(h, ".SOUL.md.*.seed")
		if terr != nil {
			return terr
		}
		_, werr := f.WriteString(DefaultSoul)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Rename(f.Name(), path)
		}
		if werr != nil {
			_ = os.Remove(f.Name())
			return werr
		}
	}
	home.ApplySecureFilePolicy(path, h)
	return nil
}

// Column the template wraps documentation comments at
const templateWidth = 80

// Commented config.yaml a new home starts from, every default shown commented out under its documentation in schema order
func configTemplate() (string, error) {
	order, err := KeyOrder()
	if err != nil {
		return "", err
	}
	n, err := encodeValue(reflect.ValueOf(Default()))
	if err != nil {
		return "", err
	}
	defaults := plainCopy(n)
	t := docTree{docs: Docs(), children: map[string][]string{}}
	for k := range t.docs {
		parts := SplitKeyPath(k)
		for i := range parts {
			parent := JoinKeyPath(parts[:i]...)
			if !slices.Contains(t.children[parent], parts[i]) {
				t.children[parent] = append(t.children[parent], parts[i])
			}
		}
	}
	for _, names := range t.children {
		slices.Sort(names)
	}
	var b strings.Builder
	b.WriteString("# icebath configuration\n")
	b.WriteString("# Every setting is shown below at its default, commented out. Uncomment a line to change it,\n")
	b.WriteString("# or run `icebath config set <section.key> <value>` to update the active profile.\n\n")
	b.WriteString("# Schema version of this file. icebath manages it: do not copy it into another config.\n")
	b.WriteString("_config_version: " + childOf(defaults, "_config_version").Value + "\n")
	for _, k := range t.ordered("", order[""]) {
		if k == "_config_version" {
			continue
		}
		b.WriteString("\n")
		t.entry(&b, k, childOf(defaults, k), k, 0)
	}
	return b.String(), nil
}

// Documentation by dotted path and the documented child names of every path
type docTree struct {
	docs     map[string]string
	children map[string][]string
}

// Keys a template mapping lists, the default keys in schema order then the documented keys the defaults leave out
func (t docTree) ordered(path string, keys []string) []string {
	out := slices.Clone(keys)
	for _, c := range t.children[path] {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// Commented lines of one key, its documentation above it and a mapping or documented children walked key by key
func (t docTree) entry(b *strings.Builder, key string, value *yaml.Node, path string, indent int) {
	pad := strings.Repeat(" ", indent)
	for _, line := range wrapWords(t.docs[path], templateWidth-indent-4) {
		b.WriteString("# " + pad + "# " + line + "\n")
	}
	if (isMapping(value) && len(value.Content) > 0) || len(t.children[path]) > 0 {
		var keys []string
		if isMapping(value) {
			for i := 0; i+1 < len(value.Content); i += 2 {
				keys = append(keys, value.Content[i].Value)
			}
		}
		w := newYAMLWriter(roundTripStyle())
		w.writeScalar(keyNode(key), indent+2, true, false, false)
		b.WriteString("# " + pad + w.b.String() + ":\n")
		for _, child := range t.ordered(path, keys) {
			t.entry(b, child, childOf(value, child), path+"."+JoinKeyPath(child), indent+2)
		}
		return
	}
	if value == nil {
		value = nullNode()
	}
	w := newYAMLWriter(roundTripStyle())
	w.writeEntry(keyNode(key), value, 0)
	for _, line := range strings.Split(strings.TrimSuffix(w.b.String(), "\n"), "\n") {
		b.WriteString("# " + pad + line + "\n")
	}
}

// Words of text grouped into lines no longer than width, a longer word on a line of its own
func wrapWords(text string, width int) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) > width:
			out = append(out, line)
			line = word
		default:
			line += " " + word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}
