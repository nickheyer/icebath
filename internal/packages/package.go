package packages

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// A tool or runtime a feature needs, and how the archives of its pin become a working store entry
type Package struct {
	Name string
	// Packages installed before it, on every target and on some only
	Deps       []string
	TargetDeps map[string][]string
	// Installed only when asked for by name or by a feature that needs it
	Optional bool
	// Installed with the required ones though optional, unless the user declined it
	Default bool
	// Tooling of icebath itself: never listed as an application package nor put on the user's PATH
	Internal bool
	// Adds no folder to PATH
	OffPath bool
	// Why there is no build of it for a target
	Gaps map[string]string
	// "minor" when pm update compares versions by major and minor only, semver otherwise
	Style string
	// Targets running a binary of another architecture under emulation
	Emulated []string
	// Version and archives every target installs, which the user's pins in the package manager's lock override
	Pin Pin
	// Addresses of the archives of a version for a target, in extraction order
	URLs func(version, target string) ([]string, error)
	// Newest first versions upstream offers for a target, asked with the variables of the profile whose update it is, nil for a package
	// pinned by hand
	Latest func(cfg *config.Config, target, locked string) ([]string, error)
	// Digest upstream publishes for an archive, empty when it publishes none and the archive is hashed instead
	Digest func(version, url string) (string, error)
	// Store entry of a version, name-version-target when nil
	Entry func(version, target string) string
	// Binary relative to the entry, by exact target, else by "win32" or "posix"
	Binary map[string]string
	// Finds the binary when its place in the entry isn't fixed, in place of Binary
	Locate func(entry, target string) string
	// Keeps the archive's own top folder rather than hoisting a lone one
	Nested bool
	// Arguments of the smoke run of the binary on this machine, --version when nil
	Probe []string
	// No smoke run, the binary only checked to be there and of the target's architecture
	NoProbe bool
	// Environment of the smoke run, the installed dependencies' over this process's when nil
	ProbeEnv func() map[string]string
	// Smoke run from the binary's own folder
	ProbeHere bool
	// Termux package serving the bionic target
	Deb *Deb
	// Turns one downloaded archive into the staged tree, extraction by file name when nil
	Unpack func(b *Build, archive string) error
	// Readies the staged tree once every archive is in, hoisting a lone top folder when nil
	Stage func(b *Build) error
	// Why an entry does not work on the target, empty when it does; standard runs the usual checks
	Verify func(entry, target string, standard func() string) string
	// Fixes what made a fresh entry fail its checks, returning the checks' new verdict and a remedy for what it could not fix
	Repair func(ctx context.Context, h *Packages, r Reporter, entry, target, reason string) (string, string)
	// Variables the entry gives the programs that use it, its binary's folder on PATH when nil
	Env func(entry, target string) Env
}

// Termux package a package installs from on the bionic target, by ar and tar extraction, never by dpkg or its scripts
type Deb struct {
	// Main file under the prefix
	Main string
	// Prefix the payload sits under, the Termux usr folder when empty
	Prefix string
}

func (d *Deb) prefix() string {
	return cmp.Or(d.Prefix, "data/data/com.termux/files/usr")
}

// One store entry being built: its staged tree, version and target, and the archives of the pin in force it is built from
type Build struct {
	Ctx      context.Context
	Dir      string
	Version  string
	Target   string
	Archives Archives
	host     *Packages
	report   Reporter
}

// Store entry of another installed package on the build's target, for a build that takes from it
func (b *Build) Entry(name string) (string, error) {
	p, err := b.host.Package(name)
	if err != nil {
		return "", err
	}
	loc, err := b.host.located(p, b.host.lock(), b.Target, locating{})
	if err != nil {
		return "", err
	}
	if loc == nil {
		return "", fmt.Errorf("%s is not installed. Run `icebath pm install %s` first.", name, name)
	}
	return loc.entry(name), nil
}

// Binary of another installed package
func (b *Build) Binary(name string) (string, error) {
	entry, err := b.Entry(name)
	if err != nil {
		return "", err
	}
	p, _ := b.host.Package(name)
	return p.binary(entry, b.Target), nil
}

// Variables an entry gives the programs that use it: folders put first on PATH and values set
type Env struct {
	Path []string
	Vars map[string]string
}

func (e Env) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	for k, v := range e.Vars {
		out[k] = v
	}
	if e.Path != nil {
		out["PATH"] = e.Path
	}
	return json.Marshal(out)
}

func (e *Env) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*e = Env{}
	for k, v := range raw {
		if k == "PATH" {
			var dirs []string
			if err := json.Unmarshal(v, &dirs); err != nil {
				var dir string
				if err := json.Unmarshal(v, &dir); err != nil {
					return err
				}
				dirs = []string{dir}
			}
			e.Path = dirs
			continue
		}
		var value string
		if err := json.Unmarshal(v, &value); err != nil {
			return err
		}
		if e.Vars == nil {
			e.Vars = map[string]string{}
		}
		e.Vars[k] = value
	}
	return nil
}

func (e Env) equal(o Env) bool {
	if !slices.Equal(e.Path, o.Path) || len(e.Vars) != len(o.Vars) {
		return false
	}
	for k, v := range e.Vars {
		if w, ok := o.Vars[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Each value with old replaced by new
func (e Env) replace(old, new string) Env {
	out := Env{}
	for _, dir := range e.Path {
		out.Path = append(out.Path, strings.ReplaceAll(dir, old, new))
	}
	for k, v := range e.Vars {
		if out.Vars == nil {
			out.Vars = map[string]string{}
		}
		out.Vars[k] = strings.ReplaceAll(v, old, new)
	}
	return out
}

// Version and archives of a package, by target in extraction order, "any" holding those every target shares
type Pin struct {
	Version   string              `json:"version"`
	Artifacts map[string]Archives `json:"artifacts"`
}

// Archives of one target, written as one object when there is only one
type Archives []Artifact

type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func (a Archives) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]Artifact(a))
}

func (a *Archives) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		var one Artifact
		if err := json.Unmarshal(data, &one); err != nil {
			return err
		}
		*a = Archives{one}
		return nil
	}
	var many []Artifact
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// Archives of a target, those for every target when it has none of its own
func (p Pin) For(target string) Archives {
	if found, ok := p.Artifacts[target]; ok && len(found) > 0 {
		return found
	}
	return p.Artifacts["any"]
}

// Pins of a lock document a declaring package embeds: {"packages": {name: {"version", "artifacts"}}}. A document that does not read is a
// mistake of the code
func Pins(data []byte) map[string]Pin {
	var doc struct {
		Packages map[string]Pin `json:"packages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		panic("packages: embedded pins do not read: " + err.Error())
	}
	return doc.Packages
}

// Failure of a package, why it failed and what the person can do about it
type InstallError struct {
	Package string
	Cause   string
	Remedy  string
	Err     error
}

func (e *InstallError) Error() string {
	text := e.Package + ": " + sentence(e.Cause, e.Package)
	if remedy := strings.TrimSpace(e.Remedy); remedy != "" && !strings.EqualFold(remedy, "none") {
		text += " " + sentence(remedy, e.Package)
	}
	return text
}

// Items joined the way a sentence lists them, the last after the word given
func joined(items []string, word string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + word + " " + items[len(items)-1]
}

// Words that stay in lower case at the start of a sentence: names of programs and of icebath
var lowerNames = map[string]bool{"icebath": true, "uv": true, "uvx": true, "npm": true, "npx": true, "node": true, "git": true, "pip": true}

// Text as a sentence: its first letter capital unless it starts with a name that keeps its case, such as the package's, and a period at
// the end unless it ends in a mark of its own
func sentence(text, name string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	word, _, _ := strings.Cut(text, " ")
	plain := word != name && !lowerNames[strings.TrimRight(word, ",:;'s")] && strings.IndexFunc(word, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\'' && r != ',' && r != ':'
	}) < 0
	if first, size := utf8.DecodeRuneInString(text); plain && unicode.IsLower(first) {
		text = string(unicode.ToUpper(first)) + text[size:]
	}
	if !strings.ContainsAny(text[len(text)-1:], ".!?:") && !strings.Contains(text, "\n") {
		text += "."
	}
	return text
}

func (e *InstallError) Unwrap() error { return e.Err }

// Install failure with the usual remedy when none is given
func Failure(name, cause, remedy string) *InstallError {
	return &InstallError{Package: name, Cause: cause, Remedy: cmp.Or(remedy, "Try again, or run `icebath pm doctor` to see what is wrong.")}
}

// Packages installed before it on the target
func (p *Package) depsOn(target string) []string {
	return append(slices.Clone(p.Deps), p.TargetDeps[target]...)
}

// Why there is no build for the target, empty when there is one
func (p *Package) Missing(target string) string {
	return p.Gaps[target]
}

func (p *Package) onPath() bool { return !p.OffPath }

func (p *Package) style() string { return cmp.Or(p.Style, "semver") }

// Store entry name of a version
func (p *Package) entryName(version, target string) string {
	if p.Entry != nil {
		return p.Entry(version, target)
	}
	return fmt.Sprintf("%s-%s-%s", p.Name, version, target)
}

// Addresses of the archives of a version for a target, refusing a package pinned by hand
func (p *Package) urls(version, target string) ([]string, error) {
	if p.URLs == nil {
		return nil, Failure(p.Name, "It has no download address.", "")
	}
	return p.URLs(version, target)
}

// Whether the bionic target installs it from a Termux package
func (p *Package) deb(target string) bool {
	return p.Deb != nil && target == Bionic
}

// Binary of an entry on the target, empty for a package without one
func (p *Package) binary(entry, target string) string {
	if p.deb(target) {
		return filepath.Join(entry, filepath.FromSlash(p.Deb.prefix()), filepath.FromSlash(p.Deb.Main))
	}
	if p.Locate != nil {
		return p.Locate(entry, target)
	}
	rel := p.Binary[target]
	if rel == "" {
		if strings.HasPrefix(target, "win32") {
			rel = p.Binary["win32"]
		} else {
			rel = p.Binary["posix"]
		}
	}
	if rel == "" {
		return ""
	}
	return filepath.Join(entry, filepath.FromSlash(rel))
}

// Variables an entry gives, the binary's folder on PATH unless it stays off PATH
func (p *Package) env(entry, target string) Env {
	if p.Env != nil {
		return p.Env(entry, target)
	}
	if bin := p.binary(entry, target); bin != "" && p.onPath() {
		return Env{Path: []string{filepath.Dir(bin)}}
	}
	return Env{}
}

// Why a binary is not there or not of the target's architecture, empty when it is
func Present(bin, entry, target string) string {
	return presence(bin, entry, target, nil)
}

func presence(bin, entry, target string, emulated []string) string {
	if st, err := os.Stat(bin); err != nil || !st.Mode().IsRegular() {
		rel, err := filepath.Rel(entry, bin)
		if err != nil {
			rel = bin
		}
		return fmt.Sprintf("%s is missing from %s, which holds %s", filepath.ToSlash(rel), entry, listing(entry, 12))
	}
	if match, known := archMatches(bin, target); known && !match && !slices.Contains(emulated, target) {
		return fmt.Sprintf("%s is not a program for %s", filepath.Base(bin), target)
	}
	return ""
}

// Why an entry does not work on the target, empty when it does
func (h *Packages) verify(p *Package, entry, target string) string {
	standard := func() string { return h.standardVerify(p, entry, target) }
	if p.Verify != nil {
		return p.Verify(entry, target, standard)
	}
	return standard()
}

// The usual checks: the binary is there and of the target, and answers its smoke run when the target is this machine
func (h *Packages) standardVerify(p *Package, entry, target string) string {
	if p.deb(target) {
		main := filepath.Join(entry, filepath.FromSlash(p.Deb.prefix()), filepath.FromSlash(p.Deb.Main))
		if _, err := os.Lstat(main); err != nil {
			rel, _ := filepath.Rel(entry, main)
			return fmt.Sprintf("%s is missing from %s", rel, entry)
		}
		return ""
	}
	bin := p.binary(entry, target)
	if bin == "" {
		if len(p.Binary) == 0 && p.Locate == nil {
			return ""
		}
		return "it has no program for " + target
	}
	if reason := presence(bin, entry, target, p.Emulated); reason != "" {
		return reason
	}
	if p.NoProbe || target != CurrentTarget() {
		return ""
	}
	args := p.Probe
	if args == nil {
		args = []string{"--version"}
	}
	described := bin + " " + strings.Join(args, " ")
	env := environ()
	if p.ProbeEnv != nil {
		env = p.ProbeEnv()
	} else if len(p.Deps) > 0 {
		env = h.EnvFor(p.Deps, nil)
	}
	dir := ""
	if p.ProbeHere {
		dir = filepath.Dir(bin)
	}
	result, err := run(context.Background(), command{argv: append([]string{bin}, args...), dir: dir, env: env, timeout: 60 * time.Second})
	switch {
	case err == errTimedOut:
		return "`" + described + "` did not finish within 60 seconds"
	case err != nil:
		return fmt.Sprintf("`%s` could not run: %s", described, err)
	case result.code != 0:
		tail := strs.Tail(strings.TrimSpace(strings.ToValidUTF8(result.stdout+result.stderr, "�")), 300)
		reason := fmt.Sprintf("`%s` ended with exit code %d", described, result.code)
		if tail != "" {
			reason += " and said: " + tail
		}
		return reason
	}
	return ""
}

// Top level names of an entry, for a diagnosis
func listing(entry string, limit int) string {
	items, err := os.ReadDir(entry)
	if err != nil {
		return "nothing, because it does not exist"
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name())
	}
	slices.Sort(names)
	if len(names) == 0 {
		return "nothing"
	}
	shown := strings.Join(names[:min(limit, len(names))], ", ")
	if len(names) > limit {
		shown += " and " + strs.Count(len(names)-limit, "more entry")
	}
	return shown
}

// Variables of this process
func environ() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			if runtime.GOOS == "windows" {
				k = strings.ToUpper(k)
			}
			if _, seen := env[k]; !seen {
				env[k] = v
			}
		}
	}
	return env
}
