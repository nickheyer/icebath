package provider

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// A provider of the profile or one of its accounts removed or renamed
type Change struct {
	Provider string
	// Account of the provider the change is of, empty for the provider itself
	Account string
	// New name, empty when the provider or account was removed
	To string
}

// What keeps names of providers or of their accounts beyond the settings that declare what they pick, such as the sessions held to an
// account: told of every change, it clears what named a thing removed and carries a renamed one over, saying what it changed, a phrase
// each such as "2 sessions"
type Follower interface {
	Follow(ctx context.Context, cfg *config.Config, change Change) ([]string, error)
}

func (r *Registry) Follow(follower Follower) {
	r.followers = append(r.followers, follower)
}

// Followers of the registry and of the registries nested in it
func (r *Registry) followed() []Follower {
	out := slices.Clone(r.followers)
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			out = append(out, nested.followed()...)
		}
	}
	return out
}

// Tells everything that names providers and accounts of a change: the settings that pick one, then the followers. What each changed
func (r *Registry) follow(ctx context.Context, cfg *config.Config, change Change) ([]string, error) {
	said, err := references(cfg, change)
	if err != nil {
		return said, err
	}
	for _, follower := range r.followed() {
		phrases, err := follower.Follow(ctx, cfg, change)
		said = append(said, phrases...)
		if err != nil {
			return said, err
		}
	}
	return said, nil
}

// Settings that name the provider or account of the change, cleared for one removed and carried over to a renamed one: those that declare
// they pick a provider, a model with its provider or an account, and the fields that do in the entries of a table. The provider's own
// settings are left to the function that changed it. A phrase for each setting changed
func references(cfg *config.Config, change Change) ([]string, error) {
	var said []string
	done := map[string]bool{}
	held := file{cfg}
	for _, section := range cfg.Sections() {
		for _, setting := range section.Settings {
			if owner := providerconfig.Owner(setting.Key); owner == change.Provider || owner == change.To && change.To != "" {
				continue
			}
			if setting.Picks != "" {
				changed, err := carry(held, setting, setting.Key, change, done)
				if err != nil {
					return said, err
				}
				if changed {
					said = append(said, setting.Title+" of "+section.Title)
				}
			}
			if setting.Kind() != config.Table || setting.Entry == nil {
				continue
			}
			entries, err := carryEntries(held, setting, change, done)
			if err != nil {
				return said, err
			}
			for _, name := range entries {
				said = append(said, "the "+strings.ToLower(setting.Entry.Noun)+" "+name+" of "+setting.Title)
			}
		}
	}
	return said, nil
}

// Changes one value that picks a provider, a model or an account, as the change asks; whether it changed it
func carry(v held, setting config.Setting, key string, change Change, done map[string]bool) (bool, error) {
	// A setting that lists the models of some providers alone picks a model all the same
	setting.Picks = providerconfig.Kind(setting.Picks)
	switch setting.Picks {
	case providerconfig.PickProvider, providerconfig.PickModel, providerconfig.PickAccount:
	default:
		// A value that names a thing of another kind, such as a tool or a shell, names no provider even when it is spelled as one
		return false, nil
	}
	value := v.get(key)
	if value == nil || done[key] || v.locked(key) {
		return false, nil
	}
	// A list or a table names providers, or models each with its provider; accounts are named one by one, beside their provider
	whole := change.Account == ""
	if _, text := value.(string); !text && setting.Picks == providerconfig.PickAccount {
		return false, nil
	}
	if list, ok := value.([]string); ok {
		items := make([]any, len(list))
		for i, item := range list {
			items[i] = item
		}
		value = items
	}
	switch typed := value.(type) {
	case string:
		switch setting.Picks {
		case providerconfig.PickProvider:
			if !whole || typed != change.Provider {
				return false, nil
			}
			// The model kept beside a provider removed goes with it
			if model := modelOf(key); change.To == "" && model != "" && v.get(model) != nil && !v.locked(model) {
				if err := put(v, done, model, ""); err != nil {
					return false, err
				}
			}
			return true, put(v, done, key, change.To)
		case providerconfig.PickModel, providerconfig.PickAccount:
			beside := providerconfig.Beside(key)
			owner := providerconfig.Owner(key)
			if beside != "" {
				owner, _ = v.get(beside).(string)
			}
			if owner != change.Provider {
				return false, nil
			}
			switch {
			case whole && change.To == "":
				if beside != "" {
					if err := put(v, done, beside, ""); err != nil {
						return false, err
					}
				}
				return true, put(v, done, key, "")
			case whole && beside != "":
				return true, put(v, done, beside, change.To)
			case setting.Picks == providerconfig.PickAccount && typed == change.Account:
				return true, put(v, done, key, change.To)
			}
		}
	case []any:
		if !whole {
			return false, nil
		}
		var kept []any
		changed := false
		for _, item := range typed {
			text, _ := item.(string)
			named, rest := text, ""
			if setting.Picks == providerconfig.PickModel {
				named, rest = providerconfig.Split(text)
			}
			switch {
			case named != change.Provider:
				kept = append(kept, item)
			case change.To != "" && setting.Picks == providerconfig.PickModel:
				kept, changed = append(kept, providerconfig.Pair(change.To, rest)), true
			case change.To != "":
				kept, changed = append(kept, change.To), true
			default:
				changed = true
			}
		}
		if !changed {
			return false, nil
		}
		if kept == nil {
			kept = []any{}
		}
		done[key] = true
		return true, v.set(key, kept)
	case map[string]any:
		if !whole {
			return false, nil
		}
		table := maps.Clone(typed)
		changed := false
		for name, entry := range typed {
			named, rest := name, ""
			if setting.Picks == providerconfig.PickModel {
				named, rest = providerconfig.Split(name)
			}
			if named != change.Provider {
				continue
			}
			delete(table, name)
			changed = true
			switch {
			case change.To != "" && setting.Picks == providerconfig.PickModel:
				table[providerconfig.Pair(change.To, rest)] = entry
			case change.To != "":
				table[change.To] = entry
			}
		}
		if !changed {
			return false, nil
		}
		done[key] = true
		return true, v.set(key, table)
	}
	return false, nil
}

// Key of the model kept beside the provider at the key, empty for a key that keeps no provider by the rule
func modelOf(key string) string {
	prefix, last := "", key
	if cut := strings.LastIndex(key, "."); cut >= 0 {
		prefix, last = key[:cut+1], key[cut+1:]
	}
	switch {
	case last == providerconfig.PickProvider:
		return prefix + providerconfig.PickModel
	case strings.HasSuffix(last, "_"+providerconfig.PickProvider):
		return prefix + strings.TrimSuffix(last, providerconfig.PickProvider) + providerconfig.PickModel
	}
	return ""
}

// Changes the fields of each entry of a table that pick a provider, a model or an account, as the change asks; an entry is removed once
// a field it is nothing without is cleared. The names of the entries changed
func carryEntries(v held, setting config.Setting, change Change, done map[string]bool) ([]string, error) {
	table, _ := v.get(setting.Key).(map[string]any)
	var changed []string
	for _, name := range slices.Sorted(maps.Keys(table)) {
		if _, ok := table[name].(map[string]any); !ok {
			continue
		}
		hit, drop := false, false
		for _, field := range setting.Entry.Fields {
			if field.Picks == "" || field.Key == "" {
				continue
			}
			key := setting.Key + "." + name + "." + field.Key
			before := v.get(key)
			moved, err := carry(v, field, key, change, done)
			if err != nil {
				return changed, err
			}
			hit = hit || moved
			drop = drop || moved && field.Need && before != nil && v.get(key) == nil
		}
		if drop && !v.locked(setting.Key+"."+name) {
			if err := v.set(setting.Key+"."+name, nil); err != nil {
				return changed, err
			}
		}
		if hit {
			changed = append(changed, name)
		}
	}
	return changed, nil
}

// Saves a value one change of a name gives the key, empty removing it
func put(v held, done map[string]bool, key, value string) error {
	done[key] = true
	if value == "" {
		return v.set(key, nil)
	}
	return v.set(key, value)
}

// Values a change carries over: what the file of a profile keeps, or what a session keeps over it
type held interface {
	get(key string) any
	set(key string, value any) error
	locked(key string) bool
}

// What the files of a profile keep, the administrator's pins left as they are
type file struct{ cfg *config.Config }

func (f file) get(key string) any              { return f.cfg.Stored(key) }
func (f file) set(key string, value any) error { return f.cfg.Set(key, value) }
func (f file) locked(key string) bool          { return f.cfg.Lock(key) != nil }

// What a session keeps over the files of its profile, by key, read through to the files for a key it keeps nothing under
type layer struct {
	cfg    *config.Config
	values map[string]any
}

func (l layer) get(key string) any {
	if value, ok := l.values[key]; ok {
		return value
	}
	return l.cfg.Stored(key)
}

func (l layer) set(key string, value any) error {
	if value == nil {
		delete(l.values, key)
	} else {
		l.values[key] = value
	}
	return nil
}

func (l layer) locked(string) bool { return false }

// Values a session keeps over its profile's settings, by key, with the change carried over as the settings take it: the provider's own
// keys dropped or moved with it, its account cleared or renamed, and every value that picks a provider, a model or an account cleared or
// carried as the settings' are. A copy, and whether anything changed
func Carry(cfg *config.Config, values map[string]any, change Change) (map[string]any, bool) {
	out := maps.Clone(values)
	if out == nil {
		return nil, false
	}
	changed := false
	own := providerconfig.Key + "." + change.Provider + "."
	for _, key := range slices.Sorted(maps.Keys(values)) {
		rest, ok := strings.CutPrefix(key, own)
		if !ok {
			continue
		}
		account := providerconfig.Accounts + "." + change.Account + "."
		switch {
		case change.Account == "" && change.To == "":
			delete(out, key)
		case change.Account == "":
			delete(out, key)
			out[providerconfig.Key+"."+change.To+"."+rest] = values[key]
		case rest == providerconfig.Account && values[key] == change.Account && change.To == "":
			delete(out, key)
		case rest == providerconfig.Account && values[key] == change.Account:
			out[key] = change.To
		case strings.HasPrefix(rest, account) && change.To == "":
			delete(out, key)
		case strings.HasPrefix(rest, account):
			delete(out, key)
			out[own+providerconfig.Accounts+"."+change.To+"."+strings.TrimPrefix(rest, account)] = values[key]
		default:
			continue
		}
		changed = true
	}
	session := layer{cfg: cfg, values: out}
	done := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(out)) {
		if providerconfig.Owner(key) != "" {
			continue
		}
		if _, setting, known := cfg.Setting(key); known && setting.Picks != "" {
			if moved, _ := carry(session, setting, key, change, done); moved {
				changed = true
			}
		}
	}
	return out, changed
}

// Sentence saying what a removal cleared, empty when it cleared nothing
func Cleared(phrases []string) string {
	if len(phrases) == 0 {
		return ""
	}
	return "It was cleared from " + strs.AndMore(phrases, 4) + "."
}
