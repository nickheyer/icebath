package profile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// What a feature keeps for each profile, removed with it when it is deleted, and removed again by purge-identity when that failed. Removing
// what is already gone is no failure
type Registrant interface {
	Delete(profile string) error
}

// What a feature does once the profile in use changes, for one that keeps something of the profile in use
type User interface {
	Use(profile string) error
}

// What a feature runs for a profile from its folder, such as its screen or its servers, stopped before the folder moves to a new name
type Stopper interface {
	Stop(profile string) error
}

// What a feature keeps under a profile's name, moved to its new name once its folder moved, and moved again by migrate-identity when that
// failed. Moving what already moved is no failure
type Renamer interface {
	Rename(old, renamed string) error
}

// What a feature has under way in a profile that its folder must not move or go from under, such as a reply being written: why it can't
// now, empty when nothing is
type Holder interface {
	Holds(profile string) string
}

// A Renamer that says what it moved, such as how many projects followed the profile's folder: lines the rename reports
type Mover interface {
	Move(old, renamed string) ([]string, error)
}

// What a feature puts into a profile just made before it is handed over, such as its bundled skills: lines the create reports first
type Seeder interface {
	Seed(making Making) ([]string, error)
}

// What a feature carries over into a profile just made from the profile it came from, such as its logins or its model: lines the create
// reports after the profile's alias and keys
type Maker interface {
	Made(making Making) ([]string, error)
}

// Failure of a feature on one store it keeps, such as a database, which a delete or rename names
type StoreError struct {
	Store string
	Err   error
}

func (e *StoreError) Error() string { return e.Store + ": " + e.Err.Error() }

func (e *StoreError) Unwrap() error { return e.Err }

// Failure a feature words in full, said as it is
type Worded string

func (w Worded) Error() string { return string(w) }

// Each failure of a joined error
func failures(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, one := range joined.Unwrap() {
			out = append(out, failures(one)...)
		}
		return out
	}
	return []error{err}
}

// Line of a failure of what a profile keeps, after what happened to the profile: deleted or renamed, and what failed: identity purge or
// migration
func failed(happened, what string, err error) string {
	if worded, ok := errors.AsType[Worded](err); ok {
		return "⚠ " + string(worded)
	}
	if store, ok := errors.AsType[*StoreError](err); ok {
		return fmt.Sprintf("⚠ Profile was %s, but %s failed for %s: %v", happened, what, store.Store, store.Err)
	}
	return fmt.Sprintf("⚠ Profile was %s, but %s failed: %v", happened, what, err)
}

// What a feature tells of a profile for its listing and description, such as the model it runs or the skills it has
type Summarizer interface {
	Summarize(cfg *config.Config) Summary
}

// What a new profile is made from
type Making struct {
	Profile string
	// Profile its settings or files were cloned from, empty for a fresh one
	Source string
	// Every file of the source was copied, not its settings alone
	All bool
	// Profile the request came from, whose keys, logins and model a fresh profile takes unless Bare
	Launch string
	Bare   bool
	// Messaging channels of the source were copied with it
	Channels bool
	// Model to pin, with its provider
	Model, Provider string
}

// What the features tell of a profile
type Summary struct {
	Model    string
	Provider string
	// Names of the skills it has, by category/name or name
	Skills []string
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Delete(profile string) error {
	var errs []error
	for _, registrant := range r.registrants {
		errs = append(errs, registrant.Delete(profile))
	}
	return errors.Join(errs...)
}

func (r *Registry) Use(profile string) error {
	var errs []error
	for _, registrant := range r.registrants {
		if user, ok := registrant.(User); ok {
			errs = append(errs, user.Use(profile))
		}
	}
	return errors.Join(errs...)
}

func (r *Registry) Holds(profile string) string {
	for _, registrant := range r.registrants {
		if holder, ok := registrant.(Holder); ok {
			if why := holder.Holds(profile); why != "" {
				return why
			}
		}
	}
	return ""
}

func (r *Registry) Stop(profile string) error {
	var errs []error
	for _, registrant := range r.registrants {
		if stopper, ok := registrant.(Stopper); ok {
			errs = append(errs, stopper.Stop(profile))
		}
	}
	return errors.Join(errs...)
}

func (r *Registry) Rename(old, renamed string) error {
	_, err := r.Move(old, renamed)
	return err
}

// Has every feature move what it keeps under the old name, the lines of those that say what they moved
func (r *Registry) Move(old, renamed string) ([]string, error) {
	var lines []string
	var errs []error
	for _, registrant := range r.registrants {
		switch hook := registrant.(type) {
		case Mover:
			said, err := hook.Move(old, renamed)
			lines = append(lines, said...)
			errs = append(errs, err)
		case Renamer:
			errs = append(errs, hook.Rename(old, renamed))
		}
	}
	return lines, errors.Join(errs...)
}

func (r *Registry) Seed(making Making) ([]string, error) {
	var lines []string
	var errs []error
	for _, registrant := range r.registrants {
		if seeder, ok := registrant.(Seeder); ok {
			said, err := seeder.Seed(making)
			lines = append(lines, said...)
			errs = append(errs, err)
		}
	}
	return lines, errors.Join(errs...)
}

func (r *Registry) Made(making Making) ([]string, error) {
	var lines []string
	var errs []error
	for _, registrant := range r.registrants {
		if maker, ok := registrant.(Maker); ok {
			said, err := maker.Made(making)
			lines = append(lines, said...)
			errs = append(errs, err)
		}
	}
	return lines, errors.Join(errs...)
}

func (r *Registry) Summarize(cfg *config.Config) Summary {
	var out Summary
	for _, registrant := range r.registrants {
		if summarizer, ok := registrant.(Summarizer); ok {
			told := summarizer.Summarize(cfg)
			if out.Model == "" {
				out.Model, out.Provider = told.Model, told.Provider
			}
			out.Skills = append(out.Skills, told.Skills...)
		}
	}
	return out
}

// Profiles of the config folder: one folder each under profiles, holding the settings that aren't shared by all of them
type Profiles struct {
	Registry
	config *config.Config
	mu     sync.Mutex
	active string
	// Creates under way by name, so two of one name never share a staging folder
	making map[string]*sync.Mutex
}

func New(cfg *config.Config) *Profiles {
	p := &Profiles{config: cfg, making: map[string]*sync.Mutex{}}
	profileconfig.Register(cfg, p.options)
	return p
}

func (p *Profiles) options() []config.Option {
	names, _ := p.List()
	options := make([]config.Option, len(names))
	for i, name := range names {
		options[i] = config.Option{Value: name, Title: name}
	}
	return options
}

// Variable naming the folder of icebath's settings and profiles, or the folder of one profile in it
const HomeEnv = "ICEBATH_HOME"

// Folder of icebath's settings and profiles: the one ICEBATH_HOME names, its ~ and variables expanded, else the user's config folder's;
// with the profile it names when it names one profile's folder
func Home() (root, pinned string, err error) {
	if raw := strings.TrimSpace(os.Getenv(HomeEnv)); raw != "" {
		dir, err := filepath.Abs(fsx.ExpandHome(os.ExpandEnv(raw)))
		if err != nil {
			return "", "", err
		}
		if filepath.Base(filepath.Dir(dir)) == "profiles" && id.MatchString(filepath.Base(dir)) {
			return filepath.Dir(filepath.Dir(dir)), filepath.Base(dir), nil
		}
		return dir, "", nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "icebath"), "", nil
}

// Folder of icebath of the user who ran sudo, when this process runs as root under sudo and that user has the profile and this root does
// not; empty otherwise
func SudoHome(root, name string) string {
	invoker := strings.TrimSpace(os.Getenv("SUDO_USER"))
	if name == "" || name == Default || invoker == "" || invoker == "root" || runtime.GOOS == "windows" || os.Geteuid() != 0 ||
		fsx.IsDir(filepath.Join(root, "profiles", name)) {
		return ""
	}
	account, err := user.Lookup(invoker)
	if err != nil || account.HomeDir == "" {
		return ""
	}
	dir := filepath.Join(account.HomeDir, ".config", "icebath")
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(account.HomeDir, "Library", "Application Support", "icebath")
	}
	if !fsx.IsDir(filepath.Join(dir, "profiles", name)) {
		return ""
	}
	return dir
}

// Profile the settings every profile shares under the folder keep as the one icebath starts with, read before they are loaded, empty for
// the default one
func Sticky(root string) string {
	shared, _ := readYAML(filepath.Join(root, "config.yaml"))[profileconfig.Key].(map[string]any)
	name, _ := shared[strings.TrimPrefix(profileconfig.Active, profileconfig.Key+".")].(string)
	if name = strings.TrimSpace(name); name == Default {
		return ""
	}
	return name
}

// Reads the settings, makes the default profile when it isn't there and uses the profile picked: by -p, ICEBATH_HOME or the variable for this
// run, else the one used last. One picked for this run that isn't there fails the start; so does one used last, unless the command only
// lists profiles or switches back to the default, which then runs in the default profile and says so to warn
func (p *Profiles) Load(recovering bool, warn io.Writer) error {
	if err := p.config.Load(); err != nil {
		return err
	}
	if err := os.MkdirAll(p.config.ProfileDir(Default), 0o700); err != nil {
		return err
	}
	_, setting, _ := p.config.Setting(profileconfig.Active)
	source, _ := p.config.Source(setting)
	picked := strings.TrimSpace(p.config.GetString(profileconfig.Active))
	if picked == "" {
		picked = Default
	}
	name, err := Canon(picked)
	switch {
	case err != nil && source != config.FromFile:
		return refuse(warn, err.Error())
	case err == nil && (name == Default || p.Exists(name)):
	case source != config.FromFile:
		return refuse(warn, missing(name).Error())
	case recovering:
		fmt.Fprintf(warn, "Warning: saved profile '%s' no longer exists; running this recovery command in the default profile.\n", picked)
		name = Default
	default:
		return refuse(warn, fmt.Sprintf("Saved profile '%s' no longer exists. Switch back with: icebath profile use default", picked))
	}
	return p.use(name)
}

// Start refused for its profile, its line already said, ending the run with status 1
type refused struct{}

func (refused) Error() string { return "" }

func (refused) ExitCode() int { return 1 }

// Says why the start is refused on a line of its own, as every refusal of a profile picked for a run is said
func refuse(warn io.Writer, text string) error {
	fmt.Fprintln(warn, "Error: "+text)
	return refused{}
}

// Profile in use
func (p *Profiles) Active() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// Settings icebath keeps and reads
func (p *Profiles) Config() *config.Config {
	return p.config
}

// Folder of the profiles
func (p *Profiles) root() string {
	return filepath.Dir(p.config.ProfileDir(Default))
}

// Every profile by name, the default one first
func (p *Profiles) List() ([]string, error) {
	entries, err := os.ReadDir(p.root())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	names := []string{Default}
	for _, entry := range entries {
		if entry.IsDir() && id.MatchString(entry.Name()) && entry.Name() != Default {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names[1:])
	return names, nil
}

// Whether the profile is there, the default always
func (p *Profiles) Exists(name string) bool {
	return name == Default || p.config.Exists(name)
}

// Folder of the profile
func (p *Profiles) Dir(name string) string {
	return p.config.ProfileDir(name)
}

// Uses the profile from now on, and the next time icebath starts; using the default one forgets the profile used last
func (p *Profiles) Use(name string) error {
	canon, err := Canon(name)
	if err != nil {
		return err
	}
	if !p.Exists(canon) {
		return missing(canon)
	}
	if err := p.use(canon); err != nil {
		return err
	}
	return p.stick(canon)
}

// Keeps the profile as the one icebath starts with, the default one by keeping none
func (p *Profiles) stick(name string) error {
	var value any = name
	if name == Default {
		value = nil
	}
	return p.config.Set(profileconfig.Active, value)
}

// Profile icebath starts with, the default one when none is kept
func (p *Profiles) sticky() string {
	if name, ok := p.config.Stored(profileconfig.Active).(string); ok && name != "" {
		return name
	}
	return Default
}

func (p *Profiles) use(name string) error {
	if err := p.config.Use(name); err != nil {
		return err
	}
	p.mu.Lock()
	p.active = name
	p.mu.Unlock()
	// What this process starts carries the profile in use, as a nested icebath picks it up
	os.Setenv(profileconfig.ActiveEnv, name)
	if _, pinned := os.LookupEnv(profileconfig.PinnedEnv); pinned {
		os.Setenv(profileconfig.PinnedEnv, name)
	}
	return p.Registry.Use(name)
}

type pickedKey struct{}

// Context of a command whose profile -p picked, which its requests to a server name
func WithPicked(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, pickedKey{}, name)
}

// Profile -p picked for the command, empty when none was and the server's profile in use serves
func Picked(ctx context.Context) string {
	name, _ := ctx.Value(pickedKey{}).(string)
	return name
}

// Profile a screen is opened on and the rest of its argument: a leading @name names the profile, the one in use when there is none
func Split(argument string) (profile, rest string) {
	argument = strings.TrimSpace(argument)
	if !strings.HasPrefix(argument, "@") {
		return "", argument
	}
	profile, rest, _ = strings.Cut(argument[1:], " ")
	return profile, strings.TrimSpace(rest)
}

// Argument opening a screen on the profile with the rest after it, the way Split reads it; no @name for the profile in use
func Join(profile, rest string) string {
	if profile == "" {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace("@" + profile + " " + rest)
}
