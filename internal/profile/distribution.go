package profile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Files of a distribution: its manifest, the template of its .env, and what that template is kept as in the profile
const (
	ManifestFile = "distribution.yaml"
	envTemplate  = ".env.template"
	envExample   = ".env.EXAMPLE"
)

// Key of the least version of icebath a manifest asks for, and the one manifests written before its rename carry
const (
	requiresKey = "icebath_requires"
	retiredKey  = "hermes_requires"
)

// What a distribution owns when its manifest names nothing, config.yaml kept on update unless forced
var distOwned = []string{"SOUL.md", "config.yaml", "mcp.json", "skills", "cron", ManifestFile}

// Top entries a distribution never writes: what is the person's own, what this machine keeps of its runs and installs, and what no export
// carries
var userOwned = func() map[string]bool {
	out := map[string]bool{}
	for _, name := range []string{"memories", "sessions", "plans", "workspace", "home", "backups", "cache", "local", "installs", "tools",
		"icebath-agent", ".worktrees", "profiles", "bin", "node_modules", ".icebath-runtime", "node", "state.db", "state.db-shm", "state.db-wal",
		"icebath_state.db", "response_store.db", "response_store.db-shm", "response_store.db-wal", "gateway.pid", "gateway_state.json", "processes.json",
		"auth.json", ".env", "auth.lock", ".config.yaml.lock", ".env.lock", "active_profile", ".update_check", "source-checks", "errors.log",
		".icebath_history", "image_cache", "audio_cache", "document_cache", "browser_screenshots", "checkpoints", "sandboxes", "logs"} {
		out[name] = true
	}
	return out
}()

// Store of cron jobs a distribution ships
var cronStore = []string{"cron", "jobs.json"}

// Folder beside a skills tree a directory is swapped in through
const swapDir = ".dist-swap"

// Install, update or manifest failure of a distribution
type DistributionError struct{ Msg string }

func (e *DistributionError) Error() string { return e.Msg }

func distError(format string, args ...any) error {
	return &DistributionError{Msg: fmt.Sprintf(format, args...)}
}

// Variable a distribution needs, and whether it can do without
type Requirement struct {
	Name        string
	Description string
	Required    bool
	Default     any
}

// What distribution.yaml says, with where it was installed from and when, once it is
type Manifest struct {
	Name        string
	Version     string
	Description string
	Requires    string
	Author      string
	License     string
	Env         []Requirement
	Owned       []string
	Source      string
	InstalledAt string
}

// What a YAML value is, as a refusal names it
func described(value any) string {
	switch value.(type) {
	case nil:
		return "nothing"
	case map[string]any:
		return "a set of named values"
	case []any:
		return "a list"
	case string:
		return "text"
	}
	return "a single value"
}

// Text of a manifest key that is set, the fallback otherwise
func field(data map[string]any, key, fallback string) string {
	if value := text(data[key]); value != "" {
		return value
	}
	return fallback
}

// Manifest of a parsed distribution.yaml, refusing one that is no mapping, has no name or malformed lists
func parseManifest(doc any) (*Manifest, error) {
	data, ok := doc.(map[string]any)
	if !ok {
		return nil, distError("%s must hold a set of named values, and it holds %s.", ManifestFile, described(doc))
	}
	name := strings.TrimSpace(field(data, "name", ""))
	if name == "" {
		return nil, distError("%s has no name. Add a line such as name: research.", ManifestFile)
	}
	var env []any
	if raw := data["env_requires"]; raw != nil && text(raw) != "" {
		if env, ok = raw.([]any); !ok {
			return nil, distError("env_requires in %s must be a list.", ManifestFile)
		}
	}
	owned, isList := data["distribution_owned"].([]any)
	if raw := data["distribution_owned"]; raw != nil && text(raw) != "" && !isList {
		return nil, distError("distribution_owned in %s must be a list.", ManifestFile)
	}
	m := &Manifest{Name: name, Version: field(data, "version", "0.1.0"), Description: field(data, "description", ""),
		Requires: field(data, requiresKey, field(data, retiredKey, "")), Author: field(data, "author", ""), License: field(data, "license", ""),
		Source: field(data, "source", ""), InstalledAt: field(data, "installed_at", "")}
	for _, entry := range env {
		row, ok := entry.(map[string]any)
		if !ok {
			return nil, distError("Each entry of env_requires in %s must be a set of named values, and one is %s.", ManifestFile, described(entry))
		}
		variable := strings.TrimSpace(field(row, "name", ""))
		if variable == "" {
			return nil, distError("An entry of env_requires in %s has no name.", ManifestFile)
		}
		required := true
		if value, set := row["required"]; set {
			required = text(value) != ""
		}
		m.Env = append(m.Env, Requirement{Name: variable, Description: field(row, "description", ""), Required: required, Default: row["default"]})
	}
	for _, item := range owned {
		if p := strings.Trim(strings.TrimSpace(text(item)), "/"); p != "" {
			m.Owned = append(m.Owned, p)
		}
	}
	return m, nil
}

// Manifest as distribution.yaml keeps it, in its order, keys set alone written
type manifestFile struct {
	Name        string            `yaml:"name"`
	Version     string            `yaml:"version"`
	Description string            `yaml:"description,omitempty"`
	Requires    string            `yaml:"icebath_requires,omitempty"`
	Author      string            `yaml:"author,omitempty"`
	License     string            `yaml:"license,omitempty"`
	Env         []requirementFile `yaml:"env_requires,omitempty"`
	Owned       []string          `yaml:"distribution_owned,omitempty"`
	Source      string            `yaml:"source,omitempty"`
	InstalledAt string            `yaml:"installed_at,omitempty"`
}

// Variable a manifest asks for as distribution.yaml keeps it, required only written when it is not
type requirementFile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    *bool  `yaml:"required,omitempty"`
	Default     any    `yaml:"default,omitempty"`
}

func (m *Manifest) file() manifestFile {
	out := manifestFile{Name: m.Name, Version: m.Version, Description: m.Description, Requires: m.Requires, Author: m.Author, License: m.License,
		Owned: m.Owned, Source: m.Source, InstalledAt: m.InstalledAt}
	for _, r := range m.Env {
		row := requirementFile{Name: r.Name, Description: r.Description, Default: r.Default}
		if !r.Required {
			row.Required = new(bool)
		}
		out.Env = append(out.Env, row)
	}
	return out
}

// Manifest of a profile's folder, nil when it has none, an error when it does not parse
func ReadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	if !fsx.IsFile(path) {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, distError("%s can't be read: %s", path, strs.Sentence(err.Error()))
	}
	var doc any
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf")), &doc); err != nil {
		return nil, distError("%s can't be read: %s", path, strs.Sentence(err.Error()))
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return parseManifest(doc)
}

func writeManifest(dir string, m *Manifest) error {
	data, err := yaml.Marshal(m.file())
	if err != nil {
		return err
	}
	return fsx.Replace(filepath.Join(dir, ManifestFile), data, 0o644)
}

var (
	versionOp = regexp.MustCompile(`^\s*(>=|<=|==|!=|>|<)\s*(.+?)\s*$`)
	semverCut = regexp.MustCompile(`[-+]`)
)

// Major, minor and patch of a version, a pre-release or build suffix dropped
func semver(v string) ([3]int64, error) {
	head := strings.TrimLeft(strings.TrimSpace(v), "v")
	if loc := semverCut.FindStringIndex(head); loc != nil {
		head = head[:loc[0]]
	}
	parts := strings.Split(head, ".")
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	var out [3]int64
	for i := range out {
		n, err := strconv.ParseInt(strings.TrimSpace(parts[i]), 10, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return out, distError("%q is not a version number such as 1.2.0.", v)
		}
		out[i] = n
	}
	return out, nil
}

// Error unless the version of icebath meets what the distribution asks, a bare version asking at least that one
func checkRequires(spec, current string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	op, target := ">=", strings.TrimSpace(spec)
	if m := versionOp.FindStringSubmatch(spec); m != nil {
		op, target = m[1], m[2]
	}
	have, err := semver(current)
	if err != nil {
		return distError("This distribution needs icebath %s%s, and the version of this icebath is unknown, so icebath can't tell whether it fits.",
			op, target)
	}
	want, err := semver(target)
	if err != nil {
		return err
	}
	order := slices.Compare(have[:], want[:])
	if !map[string]bool{">=": order >= 0, "<=": order <= 0, "==": order == 0, "!=": order != 0, ">": order > 0, "<": order < 0}[op] {
		return distError("This distribution needs icebath %s%s, and you have %s.", op, target, current)
	}
	return nil
}

// Version of this build of icebath, unknown for one built from source
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "unknown"
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}

// Text of the .env.template a distribution's manifest makes when it ships none, its optional variables commented out
func templateOf(m *Manifest) string {
	lines := []string{"# Variables this icebath profile distribution needs.", "# Copy this file to .env and fill in your own values before you use the profile.", ""}
	for _, r := range m.Env {
		if r.Description != "" {
			lines = append(lines, "# "+r.Description)
		}
		fallback := ""
		if r.Default != nil {
			fallback = fmt.Sprint(r.Default)
		}
		if r.Required {
			lines = append(lines, "# Required", r.Name+"="+fallback, "")
		} else {
			lines = append(lines, "# Optional", "# "+r.Name+"="+fallback, "")
		}
	}
	return strings.TrimRightFunc(strings.Join(lines, "\n"), unicode.IsSpace) + "\n"
}

var githubShorthand = regexp.MustCompile(`^github\.com/[\p{L}\p{N}_.-]+/[\p{L}\p{N}_.-]+/?$`)

// Whether a source is a git remote, every http address included since git is the only remote way in
func gitURL(source string) bool {
	source = strings.TrimSpace(source)
	for _, prefix := range []string{"git@", "ssh://", "git://", "http://", "https://"} {
		if strings.HasPrefix(source, prefix) {
			return true
		}
	}
	return strings.HasSuffix(source, ".git") || githubShorthand.MatchString(source)
}

// What an install would do: the manifest, where it was staged from and where it lands
type Plan struct {
	Manifest   *Manifest
	Staged     string
	Provenance string
	Target     string
	// The profile is there already
	Existing bool
	// The profile already being a distribution, rather than another profile installed over
	Distribution bool
	// The profile's config.yaml is left as it is, as an update leaves it unless told otherwise; an install writes the distribution's
	PreservesConfig bool
	HasCron         bool
	// Of each variable the manifest needs, whether the environment or the profile's .env sets it
	Set map[string]bool
}

// Folder a source is staged in: a git address cloned shallow into workdir without its .git, a local folder used where it is
func (p *Profiles) stage(ctx context.Context, source, workdir string) (string, string, error) {
	src := strings.TrimSpace(source)
	var staged, provenance, missingText string
	switch {
	case gitURL(src):
		staged, provenance = filepath.Join(workdir, "clone"), src
		address := src
		if githubShorthand.MatchString(src) {
			address = "https://" + strings.TrimRight(src, "/")
		}
		result, err := git.Fetch(ctx, "", 10*time.Minute, address, p.config.Getenv, "clone", "--depth", "1", address, staged)
		if errors.Is(err, exec.ErrNotFound) {
			return "", "", distError("Installing from a git address needs git, which is not installed.")
		}
		if err != nil {
			return "", "", err
		}
		if result.Code != 0 {
			return "", "", distError("git could not copy the repository: %s", strs.Sentence(result.Stderr))
		}
		if err := os.RemoveAll(filepath.Join(staged, ".git")); err != nil {
			return "", "", err
		}
		missingText = fmt.Sprintf("%q has no %s at its top, so it is not an icebath profile distribution.", src, ManifestFile)
	case fsx.IsDir(fsx.ExpandHome(src)):
		abs, err := filepath.Abs(fsx.ExpandHome(src))
		if err != nil {
			return "", "", err
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		staged, provenance = abs, abs
		missingText = fmt.Sprintf("The folder %q has no %s at its top. A folder you install from must hold one.",
			filepath.Clean(fsx.ExpandHome(src)), ManifestFile)
	default:
		return "", "", distError("%q is neither a git address nor a folder. Give a git address such as github.com/user/repo, or the path of a folder.", source)
	}
	if !fsx.IsFile(filepath.Join(staged, ManifestFile)) {
		return "", "", &DistributionError{Msg: missingText}
	}
	return staged, provenance, nil
}

// Refuses a staged tree holding any link before a file of it is read or copied
func refuseLinks(staged string) error {
	return filepath.WalkDir(staged, func(at string, entry fs.DirEntry, err error) error {
		if err != nil || at == staged {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(staged, at)
			return distError("A profile distribution can't hold links, and %s is one.", rel)
		}
		return nil
	})
}

// Stages the source and works out the install it would make, the name and source stamped into its manifest
func (p *Profiles) Plan(ctx context.Context, source, workdir, name string) (*Plan, error) {
	staged, provenance, err := p.stage(ctx, source, workdir)
	if err != nil {
		return nil, err
	}
	if err := refuseLinks(staged); err != nil {
		return nil, err
	}
	manifest, err := ReadManifest(staged)
	if err != nil {
		return nil, err
	}
	if manifest == nil {
		return nil, distError("There is no %s at the top of this source, so it is not an icebath profile distribution.", ManifestFile)
	}
	if err := checkRequires(manifest.Requires, version()); err != nil {
		return nil, err
	}
	if name == "" {
		name = manifest.Name
	}
	canon, err := Canon(name)
	if err != nil {
		return nil, err
	}
	if canon == Default {
		return nil, distError("A distribution can't be installed as default, the built-in profile in %s. Pass --name with another name.", p.Dir(Default))
	}
	manifest.Name, manifest.Source = canon, provenance
	manifest.InstalledAt = time.Now().UTC().Format("2006-01-02T15:04:05") + "+00:00"
	target := p.Dir(canon)
	existing := fsx.IsDir(target)
	plan := &Plan{Manifest: manifest, Staged: staged, Provenance: provenance, Target: target, Existing: existing,
		Distribution: fsx.IsFile(filepath.Join(target, ManifestFile)), HasCron: fsx.IsFile(filepath.Join(append([]string{staged}, cronStore...)...)),
		Set: map[string]bool{}}
	// Set in this process, or for a profile already there in what it reads: its .env and its secret stores
	for _, r := range manifest.Env {
		_, set := os.LookupEnv(r.Name)
		if existing && !set {
			_, set = p.config.In(canon).LookupEnv(r.Name)
		}
		plan.Set[r.Name] = set
	}
	return plan, nil
}

// Whether a path of a distribution is the installer's to keep: a credential store, a file beside the cron store, a dotfile at the top of
// skills
func installerOwned(parts []string) bool {
	if private(parts) {
		return true
	}
	if len(parts) < 2 {
		return false
	}
	if parts[0] == "cron" {
		return !slices.Equal(parts[:2], cronStore)
	}
	return parts[0] == "skills" && len(parts) == 2 && strings.HasPrefix(parts[1], ".")
}

// One path a distribution owns, in the staged tree and by its parts
type owned struct {
	src   string
	parts []string
}

// Staged paths the distribution owns: those its manifest names, or the whole payload but what is the person's own
func ownedEntries(staged string, m *Manifest) ([]owned, error) {
	var out []owned
	if len(m.Owned) == 0 {
		entries, err := os.ReadDir(staged)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !userOwned[entry.Name()] && !installerOwned([]string{entry.Name()}) {
				out = append(out, owned{filepath.Join(staged, entry.Name()), []string{entry.Name()}})
			}
		}
		return out, nil
	}
	for _, rel := range m.Owned {
		cleaned, err := memberPath(rel)
		if err != nil || cleaned == "." {
			continue
		}
		parts := strings.Split(cleaned, "/")
		if userOwned[parts[0]] || installerOwned(parts) {
			continue
		}
		src := filepath.Join(append([]string{staged}, parts...)...)
		if fsx.Exists(src) {
			out = append(out, owned{src, parts})
		}
	}
	return out, nil
}

// Removes one entry of the profile without following a link
func removeEntry(at string) error {
	if info, err := os.Lstat(at); err == nil && info.IsDir() {
		return os.RemoveAll(at)
	}
	if err := os.Remove(at); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func token() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Replaces dest with src whole, a folder copied beside it and swapped in so a failure keeps the old one
func replaceEntry(src, dest string) error {
	if !fsx.IsDir(src) {
		if err := removeEntry(dest); err != nil {
			return err
		}
		return fsx.Copy(src, dest)
	}
	holder := filepath.Join(filepath.Dir(dest), swapDir)
	stamp := token()
	staging := filepath.Join(holder, filepath.Base(dest)+".staging-"+stamp)
	backup := filepath.Join(holder, filepath.Base(dest)+".old-"+stamp)
	if err := os.MkdirAll(holder, 0o755); err != nil {
		return err
	}
	defer func() {
		os.RemoveAll(staging)
		os.Remove(holder)
	}()
	if err := copyTree(src, staging, func(string, fs.DirEntry) bool { return false }); err != nil {
		return err
	}
	if fsx.Lexists(dest) {
		if err := os.Rename(dest, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, dest); err != nil {
		if fsx.Lexists(backup) {
			if restore := os.Rename(backup, dest); restore != nil {
				return restore
			}
		}
		return err
	}
	return removeEntry(backup)
}

// Refuses a path of the profile that is a link, which a distribution never writes through
func refuseLink(at string) error {
	if info, err := os.Lstat(at); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return distError("%s is a link, and icebath does not replace links. Remove the link or put a real folder there, then run the command again.", at)
	}
	return nil
}

// Folder of base joined with parts as real folders: a file in the way replaced, a link refused
func realDir(base string, parts []string) (string, error) {
	at := base
	for _, part := range parts {
		at = filepath.Join(at, part)
		if err := refuseLink(at); err != nil {
			return "", err
		}
		if fsx.Exists(at) && !fsx.IsDir(at) {
			if err := removeEntry(at); err != nil {
				return "", err
			}
		}
		if err := os.MkdirAll(at, 0o755); err != nil {
			return "", err
		}
	}
	return at, nil
}

// Whether a folder only groups what is inside it: a skills category with no SKILL.md at or above it, or a folder of metadata alone
func container(at string, rel []string) (bool, error) {
	if !fsx.IsDir(at) {
		return false, nil
	}
	if rel[0] == "skills" {
		for i, cur := 0, at; i < len(rel); i, cur = i+1, filepath.Dir(cur) {
			if fsx.IsFile(filepath.Join(cur, "SKILL.md")) {
				return false, nil
			}
		}
		return true, nil
	}
	entries, err := os.ReadDir(at)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != "DESCRIPTION.md" && !strings.HasPrefix(entry.Name(), ".") && entry.Type().IsRegular() {
			return false, nil
		}
	}
	return true, nil
}

// Whether a folder the distribution owns merges root by root rather than being replaced whole
func mergesPerRoot(src string, rel []string) (bool, error) {
	if !fsx.IsDir(src) {
		return false, nil
	}
	if len(rel) == 1 || holdsStore(rel) {
		return true, nil
	}
	return container(src, rel)
}

func child(rel []string, name string) []string {
	return append(append([]string(nil), rel...), name)
}

// Refuses a shipped file that would replace a folder of the profile holding credential stores
func refuseStoreReplace(dest string, rel []string) error {
	if holdsStore(rel) && fsx.IsDir(dest) {
		if info, err := os.Lstat(dest); err == nil && info.Mode()&fs.ModeSymlink == 0 {
			return distError("%s is a folder that holds sign-ins, and the distribution has a file named %s in its place. icebath does not replace that "+
				"folder.", dest, strings.Join(rel, "/"))
		}
	}
	return nil
}

// Refuses every link below the target before the first write, so a refused install leaves the profile whole
func refuseLinkedTargets(target string, entries []owned) error {
	var walk func(src, dest string, rel []string) error
	walk = func(src, dest string, rel []string) error {
		children, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, c := range children {
			parts := child(rel, c.Name())
			if installerOwned(parts) {
				continue
			}
			from, to := filepath.Join(src, c.Name()), filepath.Join(dest, c.Name())
			merge, err := mergesPerRoot(from, parts)
			if err != nil {
				return err
			}
			if !merge {
				if err := refuseStoreReplace(to, parts); err != nil {
					return err
				}
				continue
			}
			if err := refuseLink(to); err != nil {
				return err
			}
			if err := walk(from, to, parts); err != nil {
				return err
			}
		}
		return nil
	}
	for _, e := range entries {
		depth := len(e.parts) - 1
		if fsx.IsDir(e.src) {
			depth = len(e.parts)
		}
		at := target
		for _, part := range e.parts[:depth] {
			at = filepath.Join(at, part)
			if err := refuseLink(at); err != nil {
				return err
			}
		}
		merge, err := mergesPerRoot(e.src, e.parts)
		if err != nil {
			return err
		}
		if merge {
			err = walk(e.src, at, e.parts)
		} else {
			err = refuseStoreReplace(filepath.Join(at, e.parts[len(e.parts)-1]), e.parts)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Merges what src authors into dest root by root, leaving what the installer keeps where it is
func mergeDir(src, dest string, rel []string) error {
	children, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, c := range children {
		parts := child(rel, c.Name())
		if installerOwned(parts) {
			continue
		}
		from := filepath.Join(src, c.Name())
		merge, err := mergesPerRoot(from, parts)
		if err != nil {
			return err
		}
		if merge {
			sub, err := realDir(dest, []string{c.Name()})
			if err != nil {
				return err
			}
			if err := mergeDir(from, sub, parts); err != nil {
				return err
			}
			continue
		}
		if err := replaceEntry(from, filepath.Join(dest, c.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Copies what the distribution owns into the profile: config.yaml kept when preserved, its .env template as .env.EXAMPLE, its manifest last
func copyPayload(staged, target string, m *Manifest, preserveConfig bool) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	entries, err := ownedEntries(staged, m)
	if err != nil {
		return err
	}
	if err := refuseLinkedTargets(target, entries); err != nil {
		return err
	}
	for _, e := range entries {
		if len(e.parts) == 1 {
			switch name := e.parts[0]; {
			case name == envTemplate:
				if err := replaceEntry(e.src, filepath.Join(target, envExample)); err != nil {
					return err
				}
				continue
			case name == "config.yaml" && preserveConfig && fsx.IsFile(filepath.Join(target, "config.yaml")):
				continue
			}
		}
		merge, err := mergesPerRoot(e.src, e.parts)
		if err != nil {
			return err
		}
		if merge {
			dest, err := realDir(target, e.parts)
			if err != nil {
				return err
			}
			if err := mergeDir(e.src, dest, e.parts); err != nil {
				return err
			}
			continue
		}
		parent, err := realDir(target, e.parts[:len(e.parts)-1])
		if err != nil {
			return err
		}
		if err := replaceEntry(e.src, filepath.Join(parent, e.parts[len(e.parts)-1])); err != nil {
			return err
		}
	}
	if fsx.IsFile(filepath.Join(target, "config.yaml")) {
		os.Chmod(filepath.Join(target, "config.yaml"), 0o600)
	}
	if example := filepath.Join(target, envExample); len(m.Env) > 0 && !fsx.Exists(example) {
		if err := os.WriteFile(example, []byte(templateOf(m)), 0o644); err != nil {
			return err
		}
	}
	return writeManifest(target, m)
}

// Refusal of what copies the local config.yaml while the profiles' settings live in a backend that has none
func (p *Profiles) fileTooling(what string) error {
	backend, err := p.config.Backend()
	if err != nil || backend == "file" {
		return err
	}
	return &ValueError{fmt.Sprintf("%s copies the settings file config.yaml, and settings kept in the %q store have none. It does not work with that "+
		"store.", what, backend)}
}

// Installs a distribution from a git address or a local folder into a new profile, or over one with force: its plan
func (p *Profiles) Install(ctx context.Context, source, name string, force bool) (*Plan, error) {
	if err := p.fileTooling("Installing a profile distribution"); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "icebath_dist_install_")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	plan, err := p.Plan(ctx, source, work, name)
	if err != nil {
		return nil, err
	}
	if plan.Existing && !force {
		return nil, distError("A profile named %s already exists in %s. Run icebath profile update to update it, or pass --force to install over it.",
			plan.Manifest.Name, plan.Target)
	}
	for _, dir := range layout {
		if err := os.MkdirAll(filepath.Join(plan.Target, dir), 0o755); err != nil {
			return nil, err
		}
	}
	if err := copyPayload(plan.Staged, plan.Target, plan.Manifest, false); err != nil {
		return nil, err
	}
	p.config.Forget(plan.Manifest.Name)
	return plan, nil
}

// Name and folder of a profile that is there, a missing one refused as a distribution error
func (p *Profiles) distProfile(name string) (string, string, error) {
	canon, err := Canon(name)
	if err != nil {
		return "", "", err
	}
	if !p.Exists(canon) {
		return "", "", distError("%s", unknown(canon).Error())
	}
	return canon, p.Dir(canon), nil
}

// Pulls an installed distribution again from where it came from: what is the person's own kept, config.yaml too unless forced
func (p *Profiles) Update(ctx context.Context, name string, forceConfig bool) (*Plan, error) {
	canon, target, err := p.distProfile(name)
	if err != nil {
		return nil, err
	}
	current, err := ReadManifest(target)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, distError("%s was not installed from a distribution, because it has no %s. Only a profile installed with icebath profile install can "+
			"be updated.", canon, ManifestFile)
	}
	if current.Source == "" {
		return nil, distError("icebath does not know where %s was installed from. Install it again with icebath profile install <source> --name %s --force.", canon, canon)
	}
	if err := p.fileTooling("Updating a profile distribution"); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "icebath_dist_update_")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	plan, err := p.Plan(ctx, current.Source, work, canon)
	if err != nil {
		return nil, err
	}
	plan.PreservesConfig = !forceConfig
	if err := copyPayload(plan.Staged, plan.Target, plan.Manifest, plan.PreservesConfig); err != nil {
		return nil, err
	}
	p.config.Forget(canon)
	if canon == p.Active() {
		if err := p.config.Use(canon); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// Manifest of a profile, nil when it is no distribution
func (p *Profiles) Manifest(name string) (*Manifest, error) {
	_, dir, err := p.distProfile(name)
	if err != nil {
		return nil, err
	}
	return ReadManifest(dir)
}
