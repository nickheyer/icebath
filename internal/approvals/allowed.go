package approvals

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/config"
)

// What an entry of the allowlist names: a kind of risky action, a pattern of commands or one command exactly. The allowlist keeps them in
// this order, each group sorted
const (
	KindAction  = "kind"
	KindPattern = "pattern"
	KindCommand = "command"
)

// One entry of the allowlist as every page and command shows it
type Entry struct {
	Value string
	// KindAction, KindPattern or KindCommand
	Kind string
	// How a person reads it: a kind of risky action with a capital first letter, any other as written
	Title string
	// What it lets through, in a few words
	Through string
	// What asks about it, for a kind of risky action
	About string
}

// Has the approvals know the kinds of risky action the tools ask about, which the allowlist may hold by name
func (a *Approvals) Risky(kinds func() []config.Option) {
	a.risks = kinds
}

// Kinds of risky action the tools ask about
func (a *Approvals) Kinds() []config.Option {
	if a.risks == nil {
		return nil
	}
	return a.risks()
}

// Kind of risky action the entry names, false for a command or a pattern
func (a *Approvals) kind(entry string) (config.Option, bool) {
	for _, kind := range a.Kinds() {
		if kind.Value == entry {
			return kind, true
		}
	}
	return config.Option{}, false
}

// What the entry names
func (a *Approvals) kindOf(entry string) string {
	switch _, kind := a.kind(entry); {
	case kind:
		return KindAction
	case strings.ContainsAny(entry, "*?["):
		return KindPattern
	}
	return KindCommand
}

// Place of a kind of entry in the allowlist
func place(kind string) int {
	return slices.Index([]string{KindAction, KindPattern, KindCommand}, kind)
}

// Entries trimmed, without empty ones or one twice, the kinds of risky action first, then the patterns, then the commands, each group sorted
func (a *Approvals) arrange(entries []string) []string {
	var out []string
	for _, entry := range entries {
		if entry = strings.TrimSpace(entry); entry != "" && !slices.Contains(out, entry) {
			out = append(out, entry)
		}
	}
	slices.SortStableFunc(out, func(x, y string) int {
		return cmp.Or(cmp.Compare(place(a.kindOf(x)), place(a.kindOf(y))), strings.Compare(x, y))
	})
	return out
}

// Every entry of the profile's allowlist, in its order
func (a *Approvals) Allowed(cfg *config.Config) []Entry {
	var out []Entry
	for _, value := range a.arrange(cfg.GetStringSlice(approvalsconfig.Allowlist)) {
		entry := Entry{Value: value, Kind: a.kindOf(value), Title: value}
		switch entry.Kind {
		case KindAction:
			kind, _ := a.kind(value)
			entry.Title, entry.Through, entry.About = kind.Title, "Every action of this kind", kind.About
		case KindPattern:
			entry.Through = "Every command that matches"
		default:
			entry.Through = "This command exactly"
		}
		out = append(out, entry)
	}
	return out
}

// Keeps the entries as the profile's whole allowlist, arranged; a.allow held
func (a *Approvals) keep(cfg *config.Config, entries []string) error {
	arranged := a.arrange(entries)
	if len(arranged) == 0 {
		return cfg.Set(approvalsconfig.Allowlist, nil)
	}
	return cfg.Set(approvalsconfig.Allowlist, arranged)
}

// Adds the entries to the profile's allowlist, the one way anything is allowed for good: the page, an answer of always, /approve always,
// the command line and Settings all end here. Gives those that were new and how many it holds now
func (a *Approvals) Allow(cfg *config.Config, entries []string) ([]string, int, error) {
	a.allow.Lock()
	defer a.allow.Unlock()
	list := cfg.GetStringSlice(approvalsconfig.Allowlist)
	var added []string
	for _, entry := range entries {
		if entry = strings.TrimSpace(entry); entry != "" && !slices.Contains(list, entry) && !slices.Contains(added, entry) {
			added = append(added, entry)
		}
	}
	if len(added) == 0 {
		return nil, len(a.arrange(list)), nil
	}
	list = append(list, added...)
	if err := a.keep(cfg, list); err != nil {
		return nil, 0, err
	}
	return added, len(a.arrange(list)), nil
}

// Takes entries out of the profile's allowlist, the one way anything stops being allowed for good
func (a *Approvals) Disallow(cfg *config.Config, entries []string) error {
	a.allow.Lock()
	defer a.allow.Unlock()
	list := slices.DeleteFunc(cfg.GetStringSlice(approvalsconfig.Allowlist), func(entry string) bool { return slices.Contains(entries, entry) })
	return a.keep(cfg, list)
}

// Puts one allowed command or pattern in the place of another in one write, so neither is ever missing; refused when the new one is
// allowed already
func (a *Approvals) Change(cfg *config.Config, old, entry string) error {
	a.allow.Lock()
	defer a.allow.Unlock()
	entry = strings.TrimSpace(entry)
	list := cfg.GetStringSlice(approvalsconfig.Allowlist)
	switch {
	case entry == "":
		return errors.New("Type a command or a pattern.")
	case entry == old:
		return nil
	case slices.Contains(list, entry):
		return fmt.Errorf("%s is allowed already.", entry)
	case !slices.Contains(list, old):
		return fmt.Errorf("%s is no longer allowed, so there is nothing to change.", old)
	}
	list[slices.Index(list, old)] = entry
	return a.keep(cfg, list)
}

// Write of the allowlist that Settings and the command line ask for, kept arranged as every other way keeps it
func (a *Approvals) writeAllowlist(cfg *config.Config, key string, value any) (bool, error) {
	if key != cfg.Key(approvalsconfig.Allowlist) {
		return false, nil
	}
	a.allow.Lock()
	defer a.allow.Unlock()
	if value == nil {
		return true, cfg.Set(approvalsconfig.Allowlist, nil)
	}
	_, setting, _ := cfg.Setting(approvalsconfig.Allowlist)
	checked, err := setting.Check(value)
	if err != nil {
		return true, err
	}
	entries, _ := checked.([]string)
	return true, a.keep(cfg, entries)
}

// What When to ask takes, as a refusal says it
var errModes = fmt.Errorf("When to ask takes %s, %s or %s.", approvalsconfig.Manual, approvalsconfig.Smart, approvalsconfig.Off)

// Why the value can't be When to ask, nil when it can
func checkMode(_ *config.Config, _ string, value any) error {
	switch value {
	case nil, approvalsconfig.Manual, approvalsconfig.Smart, approvalsconfig.Off:
		return nil
	}
	return errModes
}

// Write of When to ask that its row and the command line ask for, made by SetMode as every other change of it; the default put back as
// any other default is
func (a *Approvals) writeMode(cfg *config.Config, _ string, value any) (bool, error) {
	if value == nil {
		return false, nil
	}
	mode, _ := value.(string)
	_, err := a.SetMode(cfg, mode)
	return true, err
}

// Title of a value of When to ask as its row shows it, read from the choices the setting declares
func ModeTitle(cfg *config.Config, mode string) string {
	if _, setting, ok := cfg.Setting(approvalsconfig.Mode); ok && setting.Options != nil {
		for _, option := range setting.Options() {
			if option.Value == mode {
				return option.Title
			}
		}
	}
	return mode
}

// Sets when the agent asks before risky commands, for every session of the profile: the one way When to ask changes from a command, with
// the note its row gives and, as a second sentence, what wins over it while something does
func (a *Approvals) SetMode(cfg *config.Config, mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" || checkMode(cfg, approvalsconfig.Mode, mode) != nil {
		return "", errModes
	}
	if err := cfg.Set(approvalsconfig.Mode, mode); err != nil {
		return "", err
	}
	note := "When to ask is now " + ModeTitle(cfg, mode) + "."
	if winning := a.Winning(cfg); winning != "" {
		return note + " " + winning, nil
	}
	return note, nil
}

// What wins over When to ask now, as a sentence, empty while nothing does: icebath started with every session running without asking, or
// the administrator or an environment variable sets another mode
func (a *Approvals) Winning(cfg *config.Config) string {
	if a.launch {
		return launched
	}
	_, setting, _ := cfg.Setting(approvalsconfig.Mode)
	switch source, variable := cfg.Source(setting); source {
	case config.FromManaged:
		return "Your administrator sets When to ask to " + ModeTitle(cfg, Mode(cfg)) + ", which wins."
	case config.FromEnvironment:
		return "The environment variable " + variable + " sets When to ask to " + ModeTitle(cfg, Mode(cfg)) + ", which wins."
	}
	return ""
}

// Skips asking before risky commands in one session, or asks again, for the switch of the Approvals page, /yolo and /approvals --session:
// through SetYolo, which every way to change it ends in, and with the note they give
func (a *Approvals) Skip(id string, on bool) (string, error) {
	switch {
	case id == "":
		return "", errors.New(approvalsconfig.NoSession)
	case !on && a.launch:
		return "", errors.New(launched)
	}
	a.SetYolo(id, on)
	return SkipNote(on), nil
}

// Why nothing asks while icebath runs, which wins over When to ask and over a chat that asks again
const launched = "icebath started with ICEBATH_YOLO_MODE set, so no session asks until icebath starts again."

// What skipping approvals in a session says once it changed
func SkipNote(on bool) string {
	if on {
		return approvalsconfig.SkipTitle + " is now On. The agent runs every command without asking."
	}
	return approvalsconfig.SkipTitle + " is now Off. The agent asks again before risky commands."
}

// What a chat says when it opens on a session whose record kept skipping approvals
func SkipKept() string {
	return approvalsconfig.SkipTitle + " is still On. The agent runs every command without asking."
}

// When to ask and how the session stands against it, for a command that shows them: the mode, then what wins over it, or that the session
// skips asking
func (a *Approvals) Standing(cfg *config.Config, id string) string {
	said := "When to ask is " + ModeTitle(cfg, Mode(cfg)) + "."
	switch winning := a.Winning(cfg); {
	case winning != "":
		return said + " " + winning
	case id != "" && a.Yolo(id):
		return said + " " + approvalsconfig.SkipTitle + " is On."
	}
	return said
}
