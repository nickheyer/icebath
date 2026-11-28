package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Longest one extra's install may take, a source build on a slow machine included
const extraTimeout = 90 * time.Minute

// A Python dependency set a feature installs on request, its requirements and the hash pinned resolution of them for every target
type Extra struct {
	// Canonical name, the key of its entry
	Name string `json:"-"`
	// What it is for, as the person reads it
	Feature string   `json:"feature"`
	Aliases []string `json:"aliases,omitempty"`
	// Modules whose presence proves it installed
	Anchors []string `json:"anchors"`
	// Requirements it is resolved from
	Requires []string `json:"requires"`
	// Package indexes some requirements come from, on the platforms their marker admits
	Sources map[string]Index `json:"sources,omitempty"`
	// Programs its source builds call, the package manager's own unless the system's on that platform
	Tools []string `json:"tools,omitempty"`
	// Why it can't be installed on a target
	Unsupported map[string]string `json:"unsupported,omitempty"`
	// Packages each target compiles from source
	Build map[string][]string `json:"build,omitempty"`
	// The resolution: every package with the targets its marker admits and every artifact the lock allows
	Packages  []Locked `json:"packages"`
	BuildDeps []Locked `json:"build_requires,omitempty"`
	// Python minor the resolution is for
	Python string `json:"-"`
}

type Index struct {
	Index  string `json:"index"`
	Marker string `json:"marker,omitempty"`
}

// One locked package of a resolution
type Locked struct {
	Name    string       `json:"name"`
	Version string       `json:"version"`
	Marker  string       `json:"marker,omitempty"`
	Index   string       `json:"index,omitempty"`
	Wheels  []LockedFile `json:"wheels,omitempty"`
	Sdist   *LockedFile  `json:"sdist,omitempty"`
}

type LockedFile struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Document of extras and their resolutions for one Python minor
type extrasDoc struct {
	Schema int               `json:"schema"`
	Python string            `json:"python"`
	Extras map[string]*Extra `json:"extras"`
}

// Extras of a document a declaring package embeds; one that does not read is a mistake of the code
func Extras(data []byte) []Extra {
	var doc extrasDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		panic("packages: embedded extras do not read: " + err.Error())
	}
	var out []Extra
	for _, name := range slices.Sorted(maps.Keys(doc.Extras)) {
		e := *doc.Extras[name]
		e.Name, e.Python = name, doc.Python
		out = append(out, e)
	}
	return out
}

// Extras the user relocked with pm lock, over the declared resolutions
func (h *Packages) relocked() extrasDoc {
	var doc extrasDoc
	data, err := os.ReadFile(filepath.Join(h.stateDir(), "extras.json"))
	if err != nil || json.Unmarshal(data, &doc) != nil {
		return extrasDoc{}
	}
	return doc
}

// Every extra by canonical name, the user's resolution over the declared one
func (h *Packages) allExtras() map[string]*Extra {
	_, declared := h.index()
	user := h.relocked()
	out := map[string]*Extra{}
	for name, e := range declared {
		merged := *e
		if got, ok := user.Extras[name]; ok && user.Python != "" {
			merged.Packages, merged.BuildDeps, merged.Build, merged.Unsupported, merged.Python = got.Packages, got.BuildDeps, got.Build, got.Unsupported,
				user.Python
		}
		out[name] = &merged
	}
	return out
}

// Canonical extra a name or alias selects, false for a name no feature declares
func (h *Packages) extra(name string) (*Extra, bool) {
	key := extraKey(name)
	all := h.allExtras()
	if e, ok := all[key]; ok {
		return e, true
	}
	for _, canonical := range slices.Sorted(maps.Keys(all)) {
		if slices.ContainsFunc(all[canonical].Aliases, func(a string) bool { return extraKey(a) == key }) {
			return all[canonical], true
		}
	}
	return nil, false
}

// Canonical names of every extra, sorted
func (h *Packages) extraNames() []string {
	return slices.Sorted(maps.Keys(h.allExtras()))
}

// Refusal of an extra no feature declares, naming those there are
func (h *Packages) unknownExtra(name string) *InstallError {
	return Failure(name, "There is no extra of that name.", "The extras are "+strs.And(h.extraNames())+".")
}

// Command that provisions an extra
func installHint(extra string) string { return "icebath pm install --extra " + extra }

// Python minor and full version the pinned interpreter is
func pinnedPython(l *lockfile) (string, string, error) {
	version, ok := l.version(Python)
	if !ok {
		return "", "", errors.New("The lock file pins no Python. Run `icebath pm lock --bump python VERSION` to pin one.")
	}
	full, _, _ := strings.Cut(version, "+")
	parts := strings.Split(full, ".")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("The lock file pins Python as %s, which is not a version icebath can use.", version)
	}
	return parts[0] + "." + parts[1], full, nil
}

// Packages of the extra a target installs, each with the wheels serving that target
func (e *Extra) serving(packages []Locked, target, short, full string) ([]Locked, error) {
	env, err := markerEnv(target, short, full)
	if err != nil {
		return nil, err
	}
	var out []Locked
	for _, p := range packages {
		admitted, err := evalMarker(p.Marker, env)
		if err != nil {
			return nil, err
		}
		if !admitted {
			continue
		}
		kept := p
		kept.Wheels = nil
		for _, w := range p.Wheels {
			if slices.Contains(wheelTargets(w.URL, short), target) {
				kept.Wheels = append(kept.Wheels, w)
			}
		}
		out = append(out, kept)
	}
	return out, nil
}

// TOML basic string
func tomlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// Lock document uv installs exactly, each package with only the artifacts serving this target
func pylock(short string, packages []Locked) []byte {
	var b strings.Builder
	b.WriteString("lock-version = \"1.0\"\ncreated-by = \"icebath\"\nrequires-python = " + tomlQuote("=="+short+".*") + "\n")
	file := func(f LockedFile) string {
		return "{ url = " + tomlQuote(f.URL) + ", hashes = { sha256 = " + tomlQuote(f.SHA256) + " } }"
	}
	for _, p := range packages {
		b.WriteString("\n[[packages]]\nname = " + tomlQuote(p.Name) + "\nversion = " + tomlQuote(p.Version) + "\n")
		if p.Marker != "" {
			b.WriteString("marker = " + tomlQuote(p.Marker) + "\n")
		}
		if p.Index != "" {
			b.WriteString("index = " + tomlQuote(p.Index) + "\n")
		}
		if p.Sdist != nil {
			b.WriteString("sdist = " + file(*p.Sdist) + "\n")
		}
		if len(p.Wheels) > 0 {
			rows := make([]string, len(p.Wheels))
			for i, w := range p.Wheels {
				rows[i] = "    " + file(w)
			}
			b.WriteString("wheels = [\n" + strings.Join(rows, ",\n") + ",\n]\n")
		}
	}
	return []byte(b.String())
}

// Build constraints pinning every build requirement by version and the hashes of its artifacts
func constraints(packages []Locked) []byte {
	var b strings.Builder
	for _, p := range packages {
		b.WriteString(p.Name + "==" + p.Version)
		files := slices.Clone(p.Wheels)
		if p.Sdist != nil {
			files = append(files, *p.Sdist)
		}
		for _, f := range files {
			b.WriteString(" \\\n    --hash=sha256:" + f.SHA256)
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// Compiler suites per system, what an extra built from source needs first
var compilerInstall = []string{
	"Debian or Ubuntu   sudo apt install build-essential",
	"Fedora or RHEL     sudo dnf install gcc gcc-c++ make",
	"Arch               sudo pacman -S base-devel",
	"openSUSE           sudo zypper install gcc gcc-c++ make",
	"Alpine             sudo apk add build-base",
	"macOS              xcode-select --install",
	"Windows            winget install --id Microsoft.VisualStudio.2022.BuildTools --override \"--add Microsoft.VisualStudio.Workload.VCTools --includeRecommended --passive\"",
}

// Git per system, the one build tool left to the system outside Windows
var gitInstall = []string{
	"Debian or Ubuntu   sudo apt install git",
	"Fedora or RHEL     sudo dnf install git",
	"Arch               sudo pacman -S git",
	"openSUSE           sudo zypper install git",
	"Alpine             sudo apk add git",
	"macOS              xcode-select --install",
}

func installLines(lines []string) string { return "\n      " + strings.Join(lines, "\n      ") }

// Whether this machine has a C and a C++ compiler a source build can call
func compilerPresent() bool {
	path := os.Getenv("PATH")
	switch runtime.GOOS {
	case "windows":
		if Which("cl.exe", path) != "" {
			return true
		}
		component := "Microsoft.VisualStudio.Component.VC.Tools.x86.x64"
		if runtime.GOARCH == "arm64" {
			component = "Microsoft.VisualStudio.Component.VC.Tools.ARM64"
		}
		vswhere := filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft Visual Studio", "Installer", "vswhere.exe")
		out, err := exec.Command(vswhere, "-latest", "-products", "*", "-requires", component, "-property", "installationPath").Output()
		return err == nil && strings.TrimSpace(string(out)) != ""
	case "darwin":
		return exec.Command("xcode-select", "-p").Run() == nil
	}
	has := func(names ...string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return n != "" && Which(n, path) != "" })
	}
	return has(os.Getenv("CC"), "cc", "gcc", "clang") && has(os.Getenv("CXX"), "c++", "g++", "clang++")
}

// Refuses an extra the target can't carry, or whose source builds need a compiler or git this machine lacks, before anything downloads
func preflight(name string, e *Extra, target string, r Reporter) error {
	r = reporter(r)
	if reason, ok := e.Unsupported[target]; ok {
		return Failure(name, e.Feature+" cannot be installed on "+target+": "+reason, "Use a platform the extra supports.")
	}
	if builds := slices.Sorted(slices.Values(e.Build[target])); len(builds) > 0 {
		if !compilerPresent() {
			return Failure(name, fmt.Sprintf("%s compiles %s from source on %s, and this computer has no C and C++ compiler.", e.Feature,
				strs.And(builds), target), "Install a compiler, then run `"+installHint(name)+"` again. The usual commands:"+installLines(compilerInstall))
		}
		r.Line(fmt.Sprintf("  %s builds %s from source with the compiler of this computer.", name, strs.And(builds)))
	}
	for _, t := range e.Tools {
		if !(t == Git && runtime.GOOS != "windows") {
			r.Line(fmt.Sprintf("  %s builds with the %s that comes with icebath.", name, t))
			continue
		}
		found := Which(t, os.Getenv("PATH"))
		if found == "" {
			return Failure(name, fmt.Sprintf("%s fetches sources with %s while it builds, and this computer has no %s.", e.Feature, t, t),
				"Install it, then run `"+installHint(name)+"` again. The usual commands:"+installLines(gitInstall))
		}
		r.Line(fmt.Sprintf("  %s builds with %s.", name, found))
	}
	return nil
}

// Pinned packages and build requirements of extras merged for a target, refusing two that pin one package differently
func (h *Packages) merged(names []string, target string) ([]byte, []Locked, error) {
	short, full, err := pinnedPython(h.lock())
	if err != nil {
		return nil, nil, err
	}
	merge := func(pick func(e *Extra) []Locked, verb string) ([]Locked, error) {
		seen, owner := map[string]Locked{}, map[string]string{}
		var out []Locked
		for _, name := range names {
			e, ok := h.extra(name)
			if !ok {
				return nil, h.unknownExtra(name)
			}
			if e.Python != "" && e.Python != short {
				return nil, Failure(e.Name, fmt.Sprintf("Its packages were worked out for Python %s, but the pinned Python is %s.", e.Python, short),
					"Run `icebath pm lock` to work out the extras again for this Python.")
			}
			packages, err := e.serving(pick(e), target, short, full)
			if err != nil {
				return nil, err
			}
			for _, p := range packages {
				key := normalName(p.Name)
				if prior, ok := seen[key]; ok {
					if prior.Version != p.Version {
						return nil, Failure(e.Name, fmt.Sprintf("%s %s %s %s, but %s %s %s %s.", e.Name, verb, p.Name, p.Version, owner[key], verb, p.Name,
							prior.Version), "Build these extras in separate environments.")
					}
					continue
				}
				seen[key], owner[key] = p, e.Name
				out = append(out, p)
			}
		}
		return out, nil
	}
	packages, err := merge(func(e *Extra) []Locked { return e.Packages }, "pins")
	if err != nil {
		return nil, nil, err
	}
	buildDeps, err := merge(func(e *Extra) []Locked { return e.BuildDeps }, "builds with")
	if err != nil {
		return nil, nil, err
	}
	return pylock(short, packages), buildDeps, nil
}

// Digests of pins and build constraints a generation records, to know when its pins change
func pinInputs(pins []byte, buildDeps []Locked) map[string]any {
	digest := func(data []byte) string {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	inputs := map[string]any{"pins": digest(pins)}
	if len(buildDeps) > 0 {
		inputs["build"] = digest(constraints(buildDeps))
	}
	return inputs
}

// Builds or reuses a generation of root holding exactly the pins, built with exactly the build constraints
func (h *Packages) pinnedGeneration(ctx context.Context, cfg *config.Config, name, root string, pins []byte, buildDeps []Locked, record map[string]any,
	tools []string, r Reporter) (string, error) {
	inputs := pinInputs(pins, buildDeps)
	selection := maps.Clone(inputs)
	maps.Copy(inputs, record)
	maps.Copy(selection, record)
	build := func(generation, python string) (string, error) {
		env, err := h.buildTools(ctx, cfg, tools, r)
		if err != nil {
			return "", err
		}
		e, err := h.pyenv(ctx, cfg, filepath.Join(generation, "venv"), env, true, r)
		if err != nil {
			return "", err
		}
		e.python = python
		if err := e.create(ctx); err != nil {
			return "", err
		}
		if err := h.installLocked(ctx, e, pins, buildDeps, r); err != nil {
			return "", err
		}
		return e.executable(), nil
	}
	return h.generation(ctx, cfg, name, root, inputs, selection, build, true, "", r)
}

// Environment putting the package manager's build tools on PATH, nil when there are none to add
func (h *Packages) buildTools(ctx context.Context, cfg *config.Config, tools []string, r Reporter) (map[string]string, error) {
	var env map[string]string
	for _, name := range tools {
		if name == Git && runtime.GOOS != "windows" {
			continue
		}
		bar := newProgress(r, name)
		runner, err := h.Ensure(ctx, cfg, name, Ensuring{Explicit: true, Report: r, Step: func(_, stage string, done, total int64, label string) {
			bar.report(stage, done, total, label)
		}})
		bar.finish()
		if err != nil {
			return nil, err
		}
		if bar.fetched {
			reporter(r).Line("  ✓ Installed " + name + ".")
		}
		if env == nil {
			env = runner.Env
			continue
		}
		key := pathKey(env)
		known := filepath.SplitList(env[key])
		for _, dir := range filepath.SplitList(runner.Env[pathKey(runner.Env)]) {
			if !slices.Contains(known, dir) {
				env[key] = dir + string(os.PathListSeparator) + env[key]
			}
		}
	}
	return env, nil
}

// Installs a lock document exactly as locked, source builds taking their requirements only from hash checked locked files
func (h *Packages) installLocked(ctx context.Context, e *pyenv, pins []byte, buildDeps []Locked, r Reporter) error {
	temporary, err := os.MkdirTemp("", "pm-extra-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	lockPath, buildPath, links := filepath.Join(temporary, "pylock.toml"), filepath.Join(temporary, "build-constraints.txt"),
		filepath.Join(temporary, "build-requirements")
	if err := os.WriteFile(lockPath, pins, 0o666); err != nil {
		return err
	}
	if err := os.WriteFile(buildPath, constraints(buildDeps), 0o666); err != nil {
		return err
	}
	if err := os.Mkdir(links, 0o777); err != nil {
		return err
	}
	for _, p := range buildDeps {
		files := slices.Clone(p.Wheels)
		if len(files) == 0 && p.Sdist != nil {
			files = append(files, *p.Sdist)
		}
		for _, f := range files {
			dest := filepath.Join(links, filepath.Base(strings.SplitN(f.URL, "?", 2)[0]))
			if _, err := h.Download([]Source{{URL: f.URL, Dest: dest, SHA256: f.SHA256}}).Run(ctx, nil); err != nil {
				remedy := retryDownloadRemedy
				if IsDownload(err, HashMismatch) {
					remedy = "The file that arrived is not the one the lock file pins, so nothing was built with it. Check which proxy or mirror " +
						"sits between this computer and the address, then run the same command again."
				}
				return Failure(p.Name, fmt.Sprintf("The build requirement %s %s could not be downloaded: %s", p.Name, p.Version, err), remedy)
			}
			reporter(r).Line(fmt.Sprintf("  ✓ Downloaded the build requirement %s %s.", p.Name, p.Version))
		}
	}
	args := []string{"pip", "install", "--no-config", "--python", e.executable(), "--preview-features", "pylock,build-dependency-hashes", "--requirements",
		lockPath, "--require-build-hashes", "--no-index", "--find-links", links}
	if len(buildDeps) > 0 {
		args = append(args, "--build-constraints", buildPath)
	}
	result, err := e.recovering(ctx, "pip", func() (uvResult, error) { return e.run(ctx, args, temporary, extraTimeout) })
	if err != nil {
		return err
	}
	if result.code != 0 {
		return classify("pip", result.code, result.output)
	}
	return nil
}

// Folder of the provisioned extras' environments
func (h *Packages) extrasRoot() string {
	return filepath.Join(h.stateDir(), "extras")
}

// Provisions an extra in its own environment, the steps told to the person, and returns its interpreter; a feature switched on runs it
func (h *Packages) ProvisionExtra(ctx context.Context, cfg *config.Config, name string, r Reporter) (string, error) {
	e, ok := h.extra(name)
	if !ok {
		return "", h.unknownExtra(name)
	}
	target := CurrentTarget()
	if err := preflight(e.Name, e, target, r); err != nil {
		return "", err
	}
	pins, buildDeps, err := h.merged([]string{e.Name}, target)
	if err != nil {
		return "", err
	}
	python, err := h.pinnedGeneration(ctx, cfg, e.Name, filepath.Join(h.extrasRoot(), e.Name), pins, buildDeps, map[string]any{"extra": e.Name}, e.Tools, r)
	if err != nil {
		if ie, ok := errors.AsType[*InstallError](err); ok {
			return "", provisioningFailure(e.Name, ie, installHint(e.Name))
		}
		return "", err
	}
	if _, err := h.recordDeclinedExtras(nil, []string{e.Name}); err != nil {
		return "", err
	}
	receiptOf(ctx).step("extra "+e.Name, true, python)
	return python, nil
}

// Install error of a failed provisioning with the command to run again in place of a generic remedy
func provisioningFailure(name string, ie *InstallError, retry string) *InstallError {
	remedy := ie.Remedy
	if remedy == retryDownloadRemedy || remedy == Failure("", "", "").Remedy {
		remedy = "Check that this computer reaches the address in the error, through its proxy when it uses one, then run `" + retry + "` again."
	}
	return &InstallError{Package: name, Cause: ie.Cause, Remedy: remedy, Err: ie.Err}
}

// Side environment NAME of the profile holding the pinned packages of the extras
func (h *Packages) ExtrasEnvironment(ctx context.Context, cfg *config.Config, name string, extras []string, r Reporter) (string, error) {
	root, err := environmentRoot(cfg, name)
	if err != nil {
		return "", err
	}
	target := CurrentTarget()
	var canonical, tools []string
	for _, extra := range extras {
		e, ok := h.extra(extra)
		if !ok {
			return "", h.unknownExtra(extra)
		}
		if err := preflight(e.Name, e, target, r); err != nil {
			return "", err
		}
		if !slices.Contains(canonical, e.Name) {
			canonical = append(canonical, e.Name)
		}
		for _, t := range e.Tools {
			if !slices.Contains(tools, t) {
				tools = append(tools, t)
			}
		}
	}
	pins, buildDeps, err := h.merged(canonical, target)
	if err != nil {
		return "", err
	}
	python, err := h.pinnedGeneration(ctx, cfg, name, root, pins, buildDeps, map[string]any{"extras": strings.Join(canonical, ",")}, tools, r)
	if err != nil {
		if ie, ok := errors.AsType[*InstallError](err); ok {
			return "", provisioningFailure(name, ie, "icebath pm environment "+name+" --extra "+strings.Join(canonical, " --extra "))
		}
		return "", err
	}
	return python, nil
}

// Interpreter of a provisioned extra, false when it is unknown or not provisioned
func (h *Packages) extraPython(name string) (string, bool) {
	e, ok := h.extra(name)
	if !ok {
		return "", false
	}
	python, found, err := selectedPython(e.Name, filepath.Join(h.extrasRoot(), e.Name))
	return python, err == nil && found
}

// Deletes an extra's environment
func (h *Packages) RemoveExtra(name string) error {
	e, ok := h.extra(name)
	if !ok {
		return h.unknownExtra(name)
	}
	return os.RemoveAll(filepath.Join(h.extrasRoot(), e.Name))
}

// Provisioning state of one extra
type ExtraState struct {
	Name        string
	Feature     string
	Provisioned bool
	// Holds the pins this build carries
	Current     bool
	Unsupported string
}

// State of every declared extra on this machine
func (h *Packages) ExtraStates() ([]ExtraState, error) {
	target := CurrentTarget()
	var out []ExtraState
	for _, name := range h.extraNames() {
		e, _ := h.extra(name)
		state := ExtraState{Name: name, Feature: e.Feature, Unsupported: e.Unsupported[target]}
		_, state.Provisioned = h.extraPython(name)
		if state.Provisioned && state.Unsupported == "" {
			pins, buildDeps, err := h.merged([]string{name}, target)
			if err != nil {
				return nil, err
			}
			record, err := selected(filepath.Join(h.extrasRoot(), name))
			if err != nil {
				return nil, err
			}
			want := pinInputs(pins, buildDeps)
			state.Current = record["pins"] == want["pins"] && record["build"] == want["build"]
		}
		out = append(out, state)
	}
	return out, nil
}

// Extras the user dropped with pm install --without, by canonical name
func (h *Packages) declinedExtras() (map[string]bool, error) {
	path := filepath.Join(h.stateDir(), "declined-extras.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	var doc struct {
		Declined []string `json:"declined"`
	}
	if err == nil {
		err = json.Unmarshal(data, &doc)
	}
	if err != nil {
		return nil, Failure("Left-out extras", fmt.Sprintf("The file %s cannot be read: %s", path, err), "Fix or delete the file, then try again.")
	}
	out := map[string]bool{}
	for _, name := range doc.Declined {
		out[extraKey(name)] = true
	}
	return out, nil
}

// Adds and removes declined extras, writing only on change, and returns the new set
func (h *Packages) recordDeclinedExtras(add, remove []string) (map[string]bool, error) {
	current, err := h.declinedExtras()
	if err != nil {
		return nil, err
	}
	updated := maps.Clone(current)
	for _, name := range add {
		updated[extraKey(name)] = true
	}
	for _, name := range remove {
		delete(updated, extraKey(name))
	}
	if maps.Equal(updated, current) {
		return updated, nil
	}
	return updated, writeJSON(filepath.Join(h.stateDir(), "declined-extras.json"), map[string]any{"schema": 1, "declined": slices.Sorted(maps.Keys(updated))})
}
