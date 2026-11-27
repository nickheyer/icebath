package provider

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Provider a profile has, kept under providers and its name there: a kind of provider with a title, base URL, default model, models and
// accounts of its own. A profile can have any number of the same kind
type Instance struct {
	// Name in the profile
	ID string
	// Kind of the provider, with the title, base URL and default model the provider sets
	Spec Spec
	// Whether a plugin registers its kind
	Known bool
	// Models picked for it, every one it lists when empty
	Models []string
	// Account in use, empty while it has none: the one the profile holds it to, else the first the way it chooses would take
	Account string
	// Its accounts in the order they are tried
	Accounts []string
	// Held to the one account named in Account
	Pinned bool
	// Takes turns between its accounts marked for rotation; neither this nor Pinned takes every account in order
	Rotate   bool
	Settings config.View
}

func (i Instance) Label() string {
	return cmp.Or(i.Spec.DisplayName, i.ID)
}

// Fields of one account of the provider
func (i Instance) AccountFields(name string) config.View {
	return i.Settings.View(providerconfig.Accounts).View(name)
}

// Settings of every provider of the profile
func providers(cfg *config.Config) config.View {
	return cfg.View(providerconfig.Key)
}

// Every provider of the profile in use, by title
func (r *Registry) Instances(cfg *config.Config) []Instance {
	var instances []Instance
	for _, id := range providers(cfg).Names() {
		instances = append(instances, r.instance(cfg, id))
	}
	slices.SortStableFunc(instances, func(a, b Instance) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Label()), strings.ToLower(b.Label())), strings.Compare(a.ID, b.ID))
	})
	return instances
}

// Provider of the profile with the name, or the first of the kind with it as a name or alias
func (r *Registry) Instance(cfg *config.Config, name string) (Instance, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if config.Named(name) && providers(cfg).View(name).Exists() {
		return r.instance(cfg, name), true
	}
	spec, ok := r.Spec(name)
	if !ok {
		return Instance{}, false
	}
	for _, instance := range r.Instances(cfg) {
		if instance.Spec.Name == spec.Name {
			return instance, true
		}
	}
	return Instance{}, false
}

func (r *Registry) instance(cfg *config.Config, id string) Instance {
	settings := providers(cfg).View(id)
	kind, _ := settings.Stored(providerconfig.Type).(string)
	spec, known := r.exact(kind)
	if !known {
		spec = Spec{Name: kind}
	}
	spec.DisplayName = settings.GetString(providerconfig.Title)
	spec.BaseURL = settings.GetString(providerconfig.BaseURL)
	spec.DefaultModel = settings.GetString(providerconfig.DefaultModel)
	if mode := settings.GetString(providerconfig.APIMode); KnownMode(mode) {
		spec.Mode = APIMode(mode)
	}
	if spec.Resolve != nil {
		spec = spec.Resolve(spec, settings)
	}
	instance := Instance{ID: id, Spec: spec, Known: known, Models: settings.GetStringSlice(providerconfig.Models), Settings: settings}
	instance.Accounts = ordered(settings.View(providerconfig.Accounts))
	instance.Account = settings.GetString(providerconfig.Account)
	instance.Rotate = instance.Account == providerconfig.Rotation
	instance.Pinned = slices.Contains(instance.Accounts, instance.Account)
	if !instance.Pinned {
		instance.Account = ""
		for _, name := range instance.Accounts {
			if !instance.Rotate || instance.AccountFields(name).GetBool(providerconfig.InRotation) {
				instance.Account = name
				break
			}
		}
	}
	return instance
}

// Names of the accounts in the order they are tried: by their place, then by name
func ordered(accounts config.View) []string {
	names := accounts.Names()
	slices.SortStableFunc(names, func(a, b string) int {
		return cmp.Compare(accounts.View(a).GetInt(providerconfig.Priority), accounts.View(b).GetInt(providerconfig.Priority))
	})
	return names
}

// Spec of the kind with exactly the name
func (r *Registry) exact(name string) (Spec, bool) {
	for _, spec := range r.Specs() {
		if spec.Name == name {
			return spec, true
		}
	}
	return Spec{}, false
}

// Values a setting naming a provider of the profile in use may take, read each time they are shown
func (r *Registry) Options(cfg *config.Config) func() []config.Option {
	return func() []config.Option {
		var options []config.Option
		for _, instance := range r.Instances(cfg) {
			options = append(options, config.Option{Value: instance.ID, Title: instance.Label(), About: instance.Spec.Description})
		}
		return options
	}
}

// Adds a provider of the kind to the profile under the name, or under the kind's name, numbered when one has it already
func (r *Registry) Add(cfg *config.Config, kind, id, title string) (Instance, error) {
	spec, ok := r.Spec(kind)
	if !ok || spec.Hidden {
		return Instance{}, fmt.Errorf("No kind of provider named %s.", kind)
	}
	id = strings.ToLower(strings.TrimSpace(id))
	switch {
	case id != "" && !config.Named(id):
		return Instance{}, fmt.Errorf("%q can't name a provider. Use lower case letters, digits, dashes or underscores.", id)
	case id != "" && providers(cfg).View(id).Exists():
		return Instance{}, fmt.Errorf("The profile already has a provider named %s.", id)
	case id == "":
		id = free(spec.Name, func(name string) bool { return providers(cfg).View(name).Exists() })
	}
	settings := providers(cfg).View(id)
	if err := settings.Set(providerconfig.Type, spec.Name); err != nil {
		return Instance{}, err
	}
	if title = strings.TrimSpace(title); title != "" {
		if err := settings.Set(providerconfig.Title, title); err != nil {
			return Instance{}, err
		}
	}
	return r.instance(cfg, id), nil
}

// Removes the provider from the profile, with its accounts
func (r *Registry) Remove(cfg *config.Config, id string) error {
	if _, ok := r.Instance(cfg, id); !ok || !providers(cfg).View(id).Exists() {
		return fmt.Errorf("The profile has no provider named %s.", id)
	}
	return providers(cfg).View(id).Remove()
}

// Adds an account to the provider under the name, or under default, numbered when one has it already. It is the one in use when the
// provider had none
func (r *Registry) AddAccount(cfg *config.Config, id, name, title string) (string, error) {
	instance, ok := r.Instance(cfg, id)
	if !ok {
		return "", fmt.Errorf("The profile has no provider named %s.", id)
	}
	name = strings.ToLower(strings.TrimSpace(name))
	switch {
	case name != "" && !config.Named(name):
		return "", fmt.Errorf("%q can't name an account. Use lower case letters, digits, dashes or underscores.", name)
	case name != "" && slices.Contains(instance.Accounts, name):
		return "", fmt.Errorf("%s already has an account named %s.", instance.Label(), name)
	case name == "":
		name = free("default", func(name string) bool { return slices.Contains(instance.Accounts, name) })
	}
	fields := instance.AccountFields(name)
	if err := fields.Set(providerconfig.Title, cmp.Or(strings.TrimSpace(title), name)); err != nil {
		return "", err
	}
	if n := len(instance.Accounts); n > 0 {
		last := instance.AccountFields(instance.Accounts[n-1]).GetInt(providerconfig.Priority)
		if err := fields.Set(providerconfig.Priority, max(last+1, n)); err != nil {
			return "", err
		}
		return name, nil
	}
	return name, instance.Settings.Set(providerconfig.Account, name)
}

// Removes an account of the provider, with everything it keeps: the variable it reads its key from cleared from the profile's .env too,
// unless another account reads it. The variables cleared
func (r *Registry) RemoveAccount(cfg *config.Config, id, name string) ([]string, error) {
	instance, ok := r.Instance(cfg, id)
	if !ok || !slices.Contains(instance.Accounts, name) {
		return nil, fmt.Errorf("No account named %s.", name)
	}
	linked := strings.TrimSpace(instance.AccountFields(name).GetString(providerconfig.KeyEnv))
	if err := instance.AccountFields(name).Remove(); err != nil {
		return nil, err
	}
	var cleared []string
	if linked != "" && !r.linked(cfg, linked) {
		removed, err := cfg.RemoveEnv(linked)
		if err != nil {
			return nil, err
		}
		if removed {
			cleared = append(cleared, linked)
		}
	}
	if instance.Settings.Stored(providerconfig.Account) != name {
		return cleared, nil
	}
	// The account in use from now on is the first left whose credential still works, none when none does
	var next any
	for _, other := range instance.Accounts {
		if status, _ := instance.AccountFields(other).Get(providerconfig.Status).(map[string]any); other != name && status["state"] != "dead" {
			next = other
			break
		}
	}
	return cleared, instance.Settings.Set(providerconfig.Account, next)
}

// Whether an account of any provider of the profile reads its key from the variable
func (r *Registry) linked(cfg *config.Config, variable string) bool {
	for _, instance := range r.Instances(cfg) {
		for _, account := range instance.Accounts {
			if strings.TrimSpace(instance.AccountFields(account).GetString(providerconfig.KeyEnv)) == variable {
				return true
			}
		}
	}
	return false
}

// Leaves a variable or program out of the places the provider takes its credential from, a variable cleared from the profile's .env too.
// The variables cleared
func (r *Registry) Ignore(cfg *config.Config, id, source string) ([]string, error) {
	instance, ok := r.Instance(cfg, id)
	source = strings.TrimSpace(source)
	switch {
	case !ok:
		return nil, fmt.Errorf("The profile has no provider named %s.", id)
	case source == "":
		return nil, fmt.Errorf("Name the variable or program %s should leave out.", instance.Label())
	}
	if ignored := instance.Settings.GetStringSlice(providerconfig.Ignored); !slices.Contains(ignored, source) {
		if err := instance.Settings.Set(providerconfig.Ignored, append(slices.Clone(ignored), source)); err != nil {
			return nil, err
		}
	}
	if !config.EnvName(source) {
		return nil, nil
	}
	removed, err := cfg.RemoveEnv(source)
	if err != nil || !removed {
		return nil, err
	}
	return []string{source}, nil
}

// Uses the account for the provider from now on: one by name, Rotation for the accounts marked for rotation, or none for every account
// in order
func (r *Registry) UseAccount(cfg *config.Config, id, name string) error {
	instance, ok := r.Instance(cfg, id)
	switch {
	case !ok:
		return fmt.Errorf("The profile has no provider named %s.", id)
	case name == "":
		return instance.Settings.Set(providerconfig.Account, nil)
	case name == providerconfig.Rotation:
		if !slices.ContainsFunc(instance.Accounts, func(account string) bool { return instance.AccountFields(account).GetBool(providerconfig.InRotation) }) {
			return fmt.Errorf("No account of %s is in rotation. Mark accounts with `icebath auth rotation %s on` first.", instance.Label(), instance.ID)
		}
	case !slices.Contains(instance.Accounts, name):
		return fmt.Errorf("No account named %s.", name)
	}
	return instance.Settings.Set(providerconfig.Account, name)
}

// Gives an account of the provider another name, keeping everything it holds and its place
func (r *Registry) RenameAccount(cfg *config.Config, id, name, to string) error {
	instance, ok := r.Instance(cfg, id)
	to = strings.ToLower(strings.TrimSpace(to))
	switch {
	case !ok:
		return fmt.Errorf("The profile has no provider named %s.", id)
	case !slices.Contains(instance.Accounts, name):
		return fmt.Errorf("No account named %s.", name)
	case !config.Named(to):
		return fmt.Errorf("%q can't name an account. Use lower case letters, digits, dashes or underscores.", to)
	case to == name:
		return nil
	case slices.Contains(instance.Accounts, to):
		return fmt.Errorf("%s already has an account named %s.", instance.Label(), to)
	}
	accounts := instance.Settings.View(providerconfig.Accounts)
	if err := accounts.Set(to, accounts.Stored(name)); err != nil {
		return err
	}
	if err := accounts.View(name).Remove(); err != nil {
		return err
	}
	if instance.Settings.Stored(providerconfig.Account) == name {
		return instance.Settings.Set(providerconfig.Account, to)
	}
	return nil
}

// Moves an account of the provider to the place given from 0, the first tried, clamped to the places there are; the place it took
func (r *Registry) MoveAccount(cfg *config.Config, id, name string, place int) (int, error) {
	instance, ok := r.Instance(cfg, id)
	if !ok || !slices.Contains(instance.Accounts, name) {
		return 0, fmt.Errorf("No account named %s.", name)
	}
	others := slices.DeleteFunc(slices.Clone(instance.Accounts), func(other string) bool { return other == name })
	place = min(max(place, 0), len(others))
	for i, account := range slices.Insert(others, place, name) {
		if err := instance.AccountFields(account).Set(providerconfig.Priority, i); err != nil {
			return 0, err
		}
	}
	return place, nil
}

// The name, or the name numbered from 2 when it is taken
func free(name string, taken func(string) bool) string {
	for n, candidate := 2, name; ; n, candidate = n+1, name+"-"+strconv.Itoa(n) {
		if !taken(candidate) {
			return candidate
		}
	}
}

// Fields an account of the kind keeps
func (s Spec) AccountFields() []config.Setting {
	if s.Account != nil {
		return s.Account
	}
	return []config.Setting{providerconfig.APIKey()}
}

// Whether accounts of the kind keep an API key
func (s Spec) Keyed() bool {
	return slices.ContainsFunc(s.AccountFields(), func(field config.Setting) bool { return field.Key == providerconfig.APIKeyField })
}

// One settings section for each provider of the profile, with a section of its accounts holding one for each of them. Made only from what
// is kept in the file, since reading any setting looks through these sections
type sections struct {
	registry *Registry
}

func (s sections) Sections(values *config.Config) []config.Section {
	var out []config.Section
	all := providers(values)
	for _, id := range all.Names() {
		settings := all.View(id)
		kind, _ := settings.Stored(providerconfig.Type).(string)
		spec, _ := s.registry.exact(kind)
		title, _ := settings.Stored(providerconfig.Title).(string)
		label := cmp.Or(title, spec.Label(), id)
		accounts := settings.View(providerconfig.Accounts)
		names := accounts.Names()
		out = append(out,
			config.Section{Key: settings.Prefix(), Parent: providerconfig.Key, Title: label, About: spec.Description,
				Settings: providerconfig.Under(settings.Prefix(), slices.Concat(providerconfig.Instance(kind, cmp.Or(spec.Label(), id), spec.BaseURL, spec.BaseURLEnv, spec.DefaultModel, names), providerconfig.Rotations(), spec.Settings))},
			config.Section{Key: accounts.Prefix(), Parent: settings.Prefix(), Title: "Accounts", About: "Sign-ins and keys of " + label},
		)
		for _, name := range names {
			fields := accounts.View(name)
			title, _ := fields.Stored(providerconfig.Title).(string)
			out = append(out, config.Section{Key: fields.Prefix(), Parent: accounts.Prefix(), Title: cmp.Or(title, name),
				Settings: providerconfig.Under(fields.Prefix(), slices.Concat([]config.Setting{providerconfig.AccountTitle(name)}, spec.AccountFields(), providerconfig.Kept(spec.Keyed())))})
		}
	}
	return out
}
