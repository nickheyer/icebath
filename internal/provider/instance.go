package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
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
	Rotate bool
	// The address of its server is the person's to give, as its kind says before any address is given: Spec holds the address given, so
	// it no longer tells
	Own bool
	// Settings of the provider, whose writes never make it again once it is removed
	Settings config.View
}

func (i Instance) Label() string {
	return cmp.Or(i.Spec.DisplayName, i.ID)
}

// Fields of one account of the provider, whose writes never make the account again once it is removed
func (i Instance) AccountFields(name string) config.View {
	return i.Settings.View(providerconfig.Accounts).View(name).Within()
}

// Name an account of the provider is shown by: its title, else its name
func (i Instance) AccountTitle(name string) string {
	return cmp.Or(strings.TrimSpace(i.AccountFields(name).GetString(providerconfig.Title)), name)
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
	settings := providers(cfg).View(id).Within()
	kind, _ := settings.Stored(providerconfig.Type).(string)
	spec, known := r.exact(kind)
	if !known {
		spec = Spec{Name: kind}
	}
	own := spec.Own()
	spec.DisplayName = settings.GetString(providerconfig.Title)
	spec.BaseURL = settings.GetString(providerconfig.BaseURL)
	spec.DefaultModel = settings.GetString(providerconfig.DefaultModel)
	if mode := settings.GetString(providerconfig.APIMode); KnownMode(mode) {
		spec.Mode = APIMode(mode)
	}
	if spec.Resolve != nil {
		spec = spec.Resolve(spec, settings)
	}
	instance := Instance{ID: id, Spec: spec, Known: known, Models: settings.GetStringSlice(providerconfig.Models), Own: own, Settings: settings}
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

// Adds a provider of the kind to the profile with the address of its server when one is given, written whole in one write: under the name
// given, else under the key its title makes, else under the kind's name, numbered when one has it already
func (r *Registry) Add(cfg *config.Config, kind, id, title, address string) (Instance, error) {
	spec, ok := r.Spec(kind)
	if !ok || spec.Hidden {
		return Instance{}, fmt.Errorf("There is no kind of provider named %s.", kind)
	}
	id, title = strings.ToLower(strings.TrimSpace(id)), strings.TrimSpace(title)
	taken := func(name string) bool { return providers(cfg).View(name).Exists() }
	switch {
	case id != "" && !config.Named(id):
		return Instance{}, fmt.Errorf("%q can't name a provider. Use lower case letters, digits, dashes or underscores.", id)
	case id != "" && taken(id):
		return Instance{}, fmt.Errorf("The profile already has a provider named %s.", id)
	case id == "" && title != "":
		id = providerconfig.KeyOf(title, spec.Name, taken)
	case id == "":
		id = providerconfig.KeyOf(spec.Name, spec.Name, taken)
	}
	entry := map[string]any{providerconfig.Type: spec.Name}
	if title != "" {
		entry[providerconfig.Title] = title
	}
	if address = strings.TrimRight(strings.TrimSpace(address), "/"); address != "" {
		if err := Address(address); err != nil {
			return Instance{}, err
		}
		entry[providerconfig.BaseURL] = address
	}
	if err := providers(cfg).Set(id, entry); err != nil {
		return Instance{}, err
	}
	return r.instance(cfg, id), nil
}

// Fields an account keeps beside its credential, which connecting it again leaves as they are
var standing = []string{providerconfig.Title, providerconfig.Priority, providerconfig.InRotation, providerconfig.Requests, providerconfig.Used}

// Keeps a credential in an account of the provider, written whole with the provider in one write so that no account stands without the
// credential that makes it one. The account named takes it in place of the credential it kept and of the limits it ran into, its title,
// place and rotation kept, and the title given when one is. With no account named, an account is made with it after the others, named as
// the person named it, else after who signed in, else for the way it signs in: API key for a key, the program whose sign-in it uses; its
// key made from that name by the one rule. It is the one in use when the provider had none. A variable or program the credential comes
// from is one the provider takes from again. The account's name
func (r *Registry) Connect(cfg *config.Config, id, name, title, identity string, fields map[string]any) (string, error) {
	instance, ok := r.Instance(cfg, id)
	if !ok {
		return "", noProvider(id)
	}
	name = strings.ToLower(strings.TrimSpace(name))
	exists := slices.Contains(instance.Accounts, name)
	if name != "" && !exists {
		return "", noAccount(instance, id, name)
	}
	given, err := credentialFields(instance, fields, exists)
	if err != nil {
		return "", err
	}
	title = strings.TrimSpace(title)
	if !exists {
		title = cmp.Or(title, strings.TrimSpace(identity), wayTitle(instance, given))
		title = freeTitle(instance, title)
		name = providerconfig.KeyOf(title, "account", func(key string) bool { return slices.Contains(instance.Accounts, key) })
	}
	return name, r.rewrite(instance, func(kept map[string]any) error {
		accounts := table(kept, providerconfig.Accounts)
		entry := map[string]any{providerconfig.Title: cmp.Or(title, name)}
		if old, _ := accounts[name].(map[string]any); exists {
			for _, field := range standing {
				if value, ok := old[field]; ok && value != nil {
					entry[field] = value
				}
			}
			if title != "" {
				entry[providerconfig.Title] = title
			}
		} else if n := len(instance.Accounts); n > 0 {
			last := instance.AccountFields(instance.Accounts[n-1]).GetInt(providerconfig.Priority)
			entry[providerconfig.Priority] = max(last+1, n)
		}
		maps.Copy(entry, given)
		accounts[name] = entry
		if len(instance.Accounts) == 0 {
			kept[providerconfig.Account] = name
		}
		for _, field := range []string{providerconfig.KeyEnv, providerconfig.Program} {
			if source, _ := given[field].(string); source != "" {
				unignore(kept, source)
			}
		}
		return nil
	})
}

// Name a new account takes for the way its credential signs in, when the person gave none and nobody signed in: API key for a key, the
// program whose sign-in it uses, else the provider's name
func wayTitle(instance Instance, fields map[string]any) string {
	if program, _ := fields[providerconfig.Program].(string); program != "" {
		return program
	}
	if fields[providerconfig.APIKeyField] != nil || fields[providerconfig.KeyEnv] != nil {
		return "API key"
	}
	return instance.Label()
}

// The title, numbered from 2 when another account of the provider has it
func freeTitle(instance Instance, title string) string {
	taken := func(candidate string) bool {
		return slices.ContainsFunc(instance.Accounts, func(name string) bool { return strings.EqualFold(instance.AccountTitle(name), candidate) })
	}
	for n, candidate := 2, title; ; n, candidate = n+1, title+" "+strconv.Itoa(n) {
		if !taken(candidate) {
			return candidate
		}
	}
}

// Fields of the credential an account of the provider keeps, each checked against the field it fills, empty ones left out: those its kind
// declares, the variable a key is read from and the program whose sign-in it uses. An error when one is none of those or none is left
func credentialFields(instance Instance, fields map[string]any, exists bool) (map[string]any, error) {
	declared := map[string]config.Setting{}
	for _, field := range instance.Spec.AccountFields() {
		declared[field.Key] = field
	}
	for _, field := range providerconfig.Kept(instance.Spec.Keyed()) {
		if field.Key == providerconfig.KeyEnv || field.Key == providerconfig.Program {
			declared[field.Key] = field
		}
	}
	out := map[string]any{}
	for name, value := range fields {
		field, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("An account of %s can't keep what this sign-in gave, so nothing was kept.", instance.Label())
		}
		checked, err := field.Check(value)
		if err != nil {
			return nil, err
		}
		if checked, err = field.Located(checked); err != nil {
			return nil, err
		}
		if text, ok := checked.(string); ok {
			checked = strings.TrimSpace(text)
		}
		if name == providerconfig.KeyEnv && checked != "" && !config.EnvName(checked.(string)) {
			return nil, fmt.Errorf("%s can't name a variable. A variable name has only letters, digits and underscores, and starts with a letter.",
				checked)
		}
		if config.Truthy(checked) {
			out[name] = checked
		}
	}
	switch {
	case len(out) > 0:
		return out, nil
	case exists:
		return nil, fmt.Errorf("There is no API key or sign-in to keep, so the account of %s is as it was.", instance.Label())
	}
	return nil, fmt.Errorf("There is no API key or sign-in to keep, so no account of %s was made.", instance.Label())
}

// What a removal did besides removing the thing: the variables it cleared from the profile's .env, and what named the thing, a phrase each
type Removed struct {
	Variables []string
	Named     []string
}

// Removes a provider from the profile with its accounts, then clears every setting and everything else that named it
func (r *Registry) Remove(ctx context.Context, cfg *config.Config, id string) (Removed, error) {
	instance, ok := r.Instance(cfg, id)
	if !ok || instance.ID != strings.ToLower(strings.TrimSpace(id)) {
		return Removed{}, noProvider(id)
	}
	var linked []string
	for _, account := range instance.Accounts {
		if variable := strings.TrimSpace(instance.AccountFields(account).GetString(providerconfig.KeyEnv)); variable != "" {
			linked = append(linked, variable)
		}
	}
	if err := providers(cfg).View(instance.ID).Remove(); err != nil {
		return Removed{}, err
	}
	var removed Removed
	if err := r.unlink(cfg, &removed, linked...); err != nil {
		return removed, err
	}
	named, err := r.follow(ctx, cfg, Change{Provider: instance.ID})
	removed.Named = named
	return removed, err
}

// Removes an account of the provider, with everything it keeps, in one write with the provider's choice of account: the one in use from
// then on is the first left whose credential still works. The variable it read its key from is cleared from the profile's .env too, unless
// another account reads it; then every setting and everything else that named the account is cleared
func (r *Registry) RemoveAccount(ctx context.Context, cfg *config.Config, id, name string) (Removed, error) {
	instance, ok := r.Instance(cfg, id)
	if !ok || !slices.Contains(instance.Accounts, name) {
		return Removed{}, noAccount(instance, id, name)
	}
	linked := strings.TrimSpace(instance.AccountFields(name).GetString(providerconfig.KeyEnv))
	err := r.rewrite(instance, func(kept map[string]any) error {
		delete(table(kept, providerconfig.Accounts), name)
		if kept[providerconfig.Account] != name {
			return nil
		}
		delete(kept, providerconfig.Account)
		for _, other := range instance.Accounts {
			if status, _ := instance.AccountFields(other).Get(providerconfig.Status).(map[string]any); other != name && status["state"] != "dead" {
				kept[providerconfig.Account] = other
				break
			}
		}
		return nil
	})
	if err != nil {
		return Removed{}, err
	}
	var removed Removed
	if err := r.unlink(cfg, &removed, linked); err != nil {
		return removed, err
	}
	named, err := r.follow(ctx, cfg, Change{Provider: instance.ID, Account: name})
	removed.Named = named
	return removed, err
}

// Clears the variables from the profile's .env that no account of any provider reads any more, noting each one cleared
func (r *Registry) unlink(cfg *config.Config, removed *Removed, variables ...string) error {
	for _, variable := range variables {
		if variable == "" || r.linked(cfg, variable) || slices.Contains(removed.Variables, variable) {
			continue
		}
		cleared, err := cfg.RemoveEnv(variable)
		if err != nil {
			return err
		}
		if cleared {
			removed.Variables = append(removed.Variables, variable)
		}
	}
	return nil
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

// Uses the account for the provider from now on: one by name, Rotation for the accounts that take turns, or none for every account in
// order. Taking turns is refused while no account takes turns
func (r *Registry) UseAccount(cfg *config.Config, id, name string) error {
	instance, ok := r.Instance(cfg, id)
	switch {
	case !ok:
		return noProvider(id)
	case name == "":
		return instance.Settings.Set(providerconfig.Account, nil)
	case name == providerconfig.Rotation:
		if !slices.ContainsFunc(instance.Accounts, func(account string) bool { return instance.AccountFields(account).GetBool(providerconfig.InRotation) }) {
			return fmt.Errorf("None of the accounts of %s takes turns yet. Turn on Takes turns for the accounts that should.", instance.Label())
		}
	case !slices.Contains(instance.Accounts, name):
		return noAccount(instance, id, name)
	}
	return instance.Settings.Set(providerconfig.Account, name)
}

// Gives an account the name a person typed, the one rule of every rename: the name shown changes, and the key the account is kept under
// follows it to a free key made from the name, in one write with the provider's choice of account. Every setting and everything else that
// named the account by its key carries over to the new one. The key it is kept under
func (r *Registry) RenameAccount(ctx context.Context, cfg *config.Config, id, name, title string) (string, error) {
	instance, ok := r.Instance(cfg, id)
	title = strings.TrimSpace(title)
	switch {
	case !ok || !slices.Contains(instance.Accounts, name):
		return "", noAccount(instance, id, name)
	case title == "":
		return "", errors.New("Give the account a name with at least one letter or digit.")
	}
	to := providerconfig.KeyOf(title, "account", func(key string) bool { return key != name && slices.Contains(instance.Accounts, key) })
	err := r.rewrite(instance, func(kept map[string]any) error {
		accounts := table(kept, providerconfig.Accounts)
		entry, _ := accounts[name].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		entry[providerconfig.Title] = title
		delete(accounts, name)
		accounts[to] = entry
		if kept[providerconfig.Account] == name {
			kept[providerconfig.Account] = to
		}
		return nil
	})
	if err != nil || to == name {
		return to, err
	}
	_, err = r.follow(ctx, cfg, Change{Provider: instance.ID, Account: name, To: to})
	return to, err
}

// Gives a provider the name a person typed, by the same rule: the name shown changes and the key the provider is kept under follows it,
// the kind's name for no name, in one write. Every setting and everything else that named the provider carries over. Its key
func (r *Registry) Rename(ctx context.Context, cfg *config.Config, id, title string) (string, error) {
	instance, ok := r.Instance(cfg, id)
	if !ok || instance.ID != strings.ToLower(strings.TrimSpace(id)) {
		return "", noProvider(id)
	}
	title = strings.TrimSpace(title)
	all, _ := clone(providers(cfg).Stored("")).(map[string]any)
	taken := func(key string) bool { return key != instance.ID && all[key] != nil }
	to := providerconfig.KeyOf(cmp.Or(title, instance.Spec.Name), instance.Spec.Name, taken)
	entry, _ := all[instance.ID].(map[string]any)
	if entry == nil {
		return "", noProvider(id)
	}
	if title == "" {
		delete(entry, providerconfig.Title)
	} else {
		entry[providerconfig.Title] = title
	}
	delete(all, instance.ID)
	all[to] = entry
	if err := providers(cfg).Replace(all); err != nil || to == instance.ID {
		return to, err
	}
	_, err := r.follow(ctx, cfg, Change{Provider: instance.ID, To: to})
	return to, err
}

// Moves an account of the provider to the place given from 0, the first tried, clamped to the places there are, every account's place
// written in one write; the place it took
func (r *Registry) MoveAccount(cfg *config.Config, id, name string, place int) (int, error) {
	instance, ok := r.Instance(cfg, id)
	if !ok || !slices.Contains(instance.Accounts, name) {
		return 0, noAccount(instance, id, name)
	}
	others := slices.DeleteFunc(slices.Clone(instance.Accounts), func(other string) bool { return other == name })
	place = min(max(place, 0), len(others))
	return place, r.rewrite(instance, func(kept map[string]any) error {
		accounts := table(kept, providerconfig.Accounts)
		for i, account := range slices.Insert(others, place, name) {
			if entry, ok := accounts[account].(map[string]any); ok {
				entry[providerconfig.Priority] = i
			}
		}
		return nil
	})
}

// Writes the provider's settings in one write, as the edit leaves a copy of what is kept, so that no reader sees half of a change; nothing
// once the provider is gone
func (r *Registry) rewrite(instance Instance, edit func(kept map[string]any) error) error {
	kept, _ := clone(instance.Settings.Stored("")).(map[string]any)
	if kept == nil {
		return noProvider(instance.ID)
	}
	if err := edit(kept); err != nil {
		return err
	}
	return instance.Settings.Replace(kept)
}

// Table under the name in a copy of what is kept, made when there is none
func table(kept map[string]any, name string) map[string]any {
	inner, ok := kept[name].(map[string]any)
	if !ok {
		inner = map[string]any{}
		kept[name] = inner
	}
	return inner
}

// Copy of a value decoded from the settings, its tables and lists copied all the way down
func clone(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			out[key] = clone(inner)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = clone(inner)
		}
		return out
	case []string:
		return slices.Clone(typed)
	}
	return value
}

// Takes the source out of the places a copy of a provider's settings leaves out
func unignore(kept map[string]any, source string) {
	var left []any
	for _, have := range texts(kept[providerconfig.Ignored]) {
		if have != source {
			left = append(left, have)
		}
	}
	if left == nil {
		delete(kept, providerconfig.Ignored)
		return
	}
	kept[providerconfig.Ignored] = left
}

// Texts of a list decoded from the settings
func texts(value any) []string {
	switch typed := value.(type) {
	case []string:
		return slices.Clone(typed)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// Refusal of a provider the profile does not have
func noProvider(id string) error {
	return fmt.Errorf("The profile has no provider named %s.", id)
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
	// One provider of the profile with the kind it is of and the name it is shown by
	type held struct {
		id, kind, label string
		spec            Spec
	}
	all := providers(values)
	var kept []held
	for _, id := range all.Names() {
		settings := all.View(id)
		kind, _ := settings.Stored(providerconfig.Type).(string)
		spec, _ := s.registry.exact(kind)
		own, _ := settings.Stored(providerconfig.Title).(string)
		kept = append(kept, held{id: id, kind: kind, label: shown(spec, id, own), spec: spec})
	}
	slices.SortStableFunc(kept, func(a, b held) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.label), strings.ToLower(b.label)), strings.Compare(a.id, b.id))
	})
	var out []config.Section
	for place, p := range kept {
		settings := all.View(p.id)
		accounts := settings.View(providerconfig.Accounts)
		names := accounts.Names()
		instance := providerconfig.Instance(p.kind, shown(p.spec, p.id, ""), p.spec.BaseURL, p.spec.BaseURLEnv, p.spec.DefaultModel, p.spec.Own(),
			titled(accounts, names))
		out = append(out,
			config.Section{Key: settings.Prefix(), Parent: providerconfig.Key, Title: p.label, About: "The models, accounts and limits of this provider",
				Order:    (place + 1) * 10,
				Settings: providerconfig.Under(settings.Prefix(), providerconfig.Arranged(slices.Concat(instance, providerconfig.Rotations(), p.spec.Settings)))},
			config.Section{Key: accounts.Prefix(), Parent: settings.Prefix(), Title: "Accounts", About: "The accounts of this provider", Order: 10},
		)
		for place, name := range tried(accounts, names) {
			fields := accounts.View(name)
			title, _ := fields.Stored(providerconfig.Title).(string)
			out = append(out, config.Section{Key: fields.Prefix(), Parent: accounts.Prefix(), Title: cmp.Or(title, name), About: "What this account signs in with",
				Order: (place + 1) * 10,
				Settings: providerconfig.Under(fields.Prefix(), providerconfig.Arranged(slices.Concat([]config.Setting{providerconfig.AccountTitle(name)},
					p.spec.AccountFields(), providerconfig.Kept(p.spec.Keyed()))))})
		}
	}
	return out
}

// Name a provider is shown by: the one the person gave it, else the name of its kind, numbered as its key is when another of the kind
// came first, so two of one kind tell apart
func shown(kind Spec, id, own string) string {
	if own = strings.TrimSpace(own); own != "" {
		return own
	}
	label := kind.Label()
	if label == "" || id == kind.Name {
		return cmp.Or(label, id)
	}
	if n, ok := strings.CutPrefix(id, kind.Name+"-"); ok {
		if _, err := strconv.Atoi(n); err == nil {
			return label + " " + n
		}
	}
	return label + " (" + id + ")"
}

// Whether the address of a provider of the kind is the person's to give, as a server of their own's is
func (s Spec) Own() bool {
	return s.Local || s.BaseURL == ""
}

// Accounts by name with the titles they are shown by, in the order they are tried, read from what is kept each time they are asked for
func titled(accounts config.View, names []string) func() []config.Option {
	return func() []config.Option {
		var options []config.Option
		for _, name := range tried(accounts, names) {
			title, _ := accounts.View(name).Stored(providerconfig.Title).(string)
			options = append(options, config.Option{Value: name, Title: cmp.Or(strings.TrimSpace(title), name)})
		}
		return options
	}
}

// Names of the accounts in the order they are tried, read from what is kept alone: by their place, then by name
func tried(accounts config.View, names []string) []string {
	place := func(name string) int {
		n, _ := config.Whole(accounts.View(name).Stored(providerconfig.Priority))
		return n
	}
	sorted := slices.Clone(names)
	slices.SortStableFunc(sorted, func(a, b string) int { return cmp.Compare(place(a), place(b)) })
	return sorted
}

// Refusal of an account the provider does not have, named by the provider's title when the profile has it
func noAccount(instance Instance, id, name string) error {
	return fmt.Errorf("%s has no account named %s.", cmp.Or(instance.Label(), id), name)
}

// Name the account of a listed provider with the name is shown by, its name when the provider lists no such account
func AccountTitle(entry *providerv1.Provider, name string) string {
	for _, account := range entry.GetAccounts() {
		if account.GetName() == name {
			return account.GetTitle()
		}
	}
	return name
}
