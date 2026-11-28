package secrets

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// How a store stands for a profile
type State struct {
	On bool
	// What keeps it from filling anything, in the few words a row says, such as Needs a token, and as the sentence its page opens on; empty
	// when nothing does
	Needs, Lacks string
	// Variables it fills: those bound to its references for a store that fills named ones, those its last fetch set for one that fills all
	// it holds
	Fills int
	// Variable of the credential it signs in with and whether the profile holds one, for a store that has one
	TokenEnv string
	TokenSet bool
}

// Store that says what it lacks to fill anything besides what the settings it fetches with hold, such as its program or a sign-in
type Needer interface {
	// What keeps it from working for the profile, in a few words and as a sentence; empty when nothing does
	Needs(ctx context.Context, fetch Fetch) (short, sentence string)
}

// How the store stands for the profile: on or off, what it lacks, how many variables it fills, and its credential
func (h *Secrets) State(ctx context.Context, cfg *config.Config, source Source) State {
	spec := source.Spec()
	plain := cfg.Plain()
	env := h.unscoped(plain, gaps(plain))
	state := State{On: h.Enabled(cfg, spec.Name)}
	if spec.TokenSetting != "" || spec.TokenEnv != "" {
		state.TokenEnv = h.TokenEnv(cfg, spec)
		state.TokenSet = strings.TrimSpace(env[state.TokenEnv]) != ""
	}
	if needer, ok := source.(Needer); ok {
		state.Needs, state.Lacks = needer.Needs(ctx, Fetch{Config: plain, Env: env})
	}
	if spec.Shape == Mapped {
		state.Fills = len(valid(spec, h.Bindings(cfg, spec.Name)))
	}
	if !state.On {
		return state
	}
	for _, done := range h.Report(cfg).Sources {
		if done.Name != spec.Name {
			continue
		}
		if spec.Shape == Bulk {
			state.Fills = len(done.Applied)
		}
		if done.Result.Error != "" && state.Needs == "" {
			state.Needs, state.Lacks = "Failed", "Its last fetch failed. "+strs.Sentence(done.Result.Error)
		}
	}
	return state
}

// Variables a store that fills named ones binds to its references, each named as it is written
func (h *Secrets) Bindings(cfg *config.Config, name string) map[string]string {
	out := map[string]string{}
	table, _ := cfg.Get(secretsconfig.Of(name, secretsconfig.Env)).(map[string]any)
	for variable, value := range table {
		text, ok := value.(string)
		if !ok {
			text = fmt.Sprint(value)
		}
		out[variable] = strings.TrimSpace(text)
	}
	return out
}

// Bindings that name a variable and a reference of the store
func valid(spec Spec, bindings map[string]string) map[string]string {
	out := map[string]string{}
	for variable, reference := range bindings {
		if checkBinding(spec, variable, reference) == nil {
			out[variable] = reference
		}
	}
	return out
}

// Why a variable can't be filled from the reference in the store, nil when it can: a name no variable can have, or a reference of another
// store
func checkBinding(spec Spec, variable, reference string) error {
	switch {
	case !config.EnvName(variable):
		return fmt.Errorf("%q can't be the name of a variable. A name has only letters, digits and underscores, and starts with a letter.", variable)
	case spec.Scheme != "" && !strings.HasPrefix(strings.TrimSpace(reference), spec.Scheme+"://"):
		return fmt.Errorf("%q is not a reference of %s. A reference starts with %s://, such as %s.", reference, spec.Label, spec.Scheme, spec.Example)
	}
	return nil
}

// Store of the name that fills named variables, a refusal in a person's words for any other
func (h *Secrets) mapped(name string) (Spec, error) {
	source, ok := h.Source(name)
	if !ok {
		return Spec{}, fmt.Errorf("icebath has no secret store named %s.", name)
	}
	if spec := source.Spec(); spec.Shape == Mapped {
		return spec, nil
	}
	return Spec{}, fmt.Errorf("%s fills every variable it holds, so none is set from it one by one.", source.Spec().Label)
}

// Fills the variable from the reference in the store, the reference checked before it is kept, then fetches the stores again
func (h *Secrets) Bind(cfg *config.Config, name, variable, reference string) error {
	spec, err := h.mapped(name)
	if err != nil {
		return err
	}
	variable, reference = strings.TrimSpace(variable), strings.TrimSpace(reference)
	if err := checkBinding(spec, variable, reference); err != nil {
		return err
	}
	if err := cfg.Set(secretsconfig.Of(name, secretsconfig.Env)+"."+variable, reference); err != nil {
		return err
	}
	h.Reset(cfg)
	return nil
}

// Stops filling the variable from the store, then fetches the stores again; false when the store did not fill it
func (h *Secrets) Unbind(cfg *config.Config, name, variable string) (bool, error) {
	if _, err := h.mapped(name); err != nil {
		return false, err
	}
	variable = strings.TrimSpace(variable)
	if _, bound := h.Bindings(cfg, name)[variable]; !bound {
		return false, nil
	}
	if err := cfg.Set(secretsconfig.Of(name, secretsconfig.Env)+"."+variable, nil); err != nil {
		return false, err
	}
	h.Reset(cfg)
	return true, nil
}

// Turns the store on or off for the profile, then fetches the stores again so what it fills comes or goes at once
func (h *Secrets) Switch(cfg *config.Config, name string, on bool) error {
	if _, ok := h.Source(name); !ok {
		return fmt.Errorf("icebath has no secret store named %s. It has %s.", name, h.names())
	}
	if err := cfg.Set(secretsconfig.Of(name, secretsconfig.Enabled), on); err != nil {
		return err
	}
	h.Reset(cfg)
	return nil
}

// Removes the token the store signs in with from the profile's keys file, then fetches the stores again; false when the keys file held none
func (h *Secrets) Forget(cfg *config.Config, name string) (bool, error) {
	source, ok := h.Source(name)
	if !ok {
		return false, fmt.Errorf("icebath has no secret store named %s. It has %s.", name, h.names())
	}
	spec := source.Spec()
	if spec.TokenSetting == "" && spec.TokenEnv == "" {
		return false, fmt.Errorf("%s signs in with no token.", spec.Label)
	}
	removed, err := cfg.RemoveEnv(h.TokenEnv(cfg, spec))
	if err != nil || !removed {
		return removed, err
	}
	h.Reset(cfg)
	return true, nil
}

// Names of every store, for a sentence
func (h *Secrets) names() string {
	var names []string
	for _, source := range h.all() {
		names = append(names, source.Spec().Name)
	}
	return strings.Join(names, ", ")
}

// Why the stores can't be asked in the order named, nil when they can: a name no store has, or one named twice
func (h *Secrets) checkOrder(names []string) error {
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if _, ok := h.Source(name); !ok {
			return fmt.Errorf("icebath has no secret store named %s. It has %s.", name, h.names())
		}
		if seen[name] {
			return fmt.Errorf("%s is named twice in the order of the stores.", name)
		}
		seen[name] = true
	}
	return nil
}

// Asks the stores in the order named, those not named after them in the order they are registered, then fetches them again
func (h *Secrets) Reorder(cfg *config.Config, names []string) error {
	if err := h.checkOrder(names); err != nil {
		return err
	}
	if err := cfg.Set(secretsconfig.Sources, names); err != nil {
		return err
	}
	h.Reset(cfg)
	return nil
}

// What a fetch of a store does with one variable: export, override, skip_set, skip_token or unresolved; exported or overrode once set
type Outcome struct{ Name, Action string }

// Fetches the store now, neither reading nor writing its cache, and says what the next start does with each variable it holds and why a
// bound one could not be read. Apply sets those it would set for the profile until the stores are fetched again
func (h *Secrets) Preview(ctx context.Context, cfg *config.Config, name string, apply bool) ([]Outcome, []string, error) {
	source, ok := h.Source(name)
	if !ok {
		return nil, nil, fmt.Errorf("icebath has no secret store named %s. It has %s.", name, h.names())
	}
	spec := source.Spec()
	result := h.Fetch(ctx, cfg, source, true)
	if !result.OK() {
		return nil, result.Warnings, fmt.Errorf("%s", strs.Sentence(result.Error))
	}
	tokenEnv, existing, override := h.TokenEnv(cfg, spec), h.Existing(cfg), h.Overrides(cfg, name)
	var outcomes []Outcome
	set := map[string]string{}
	for _, variable := range slices.Sorted(maps.Keys(result.Secrets)) {
		action := Action(variable, tokenEnv, existing, override)
		if apply && (action == "export" || action == "override") {
			set[variable] = result.Secrets[variable]
			action = map[string]string{"export": "exported", "override": "overrode"}[action]
		}
		outcomes = append(outcomes, Outcome{Name: variable, Action: action})
	}
	if spec.Shape == Mapped {
		for _, variable := range slices.Sorted(maps.Keys(valid(spec, h.Bindings(cfg, name)))) {
			if _, got := result.Secrets[variable]; !got {
				outcomes = append(outcomes, Outcome{Name: variable, Action: "unresolved"})
			}
		}
	}
	if apply {
		h.Export(cfg, name, set)
	}
	return outcomes, result.Warnings, nil
}

// Takes every write a person asks for of the settings of the stores: the order they are asked in and whether one is on, each checked and
// made through the functions the page uses, the variables of a store that fills named ones checked against its references, and every other
// setting of theirs, after which the stores are fetched again so the change holds at once
func (h *Secrets) guard(cfg *config.Config) {
	cfg.Guard(secretsconfig.Key, config.Guard{Check: h.check, Write: h.write})
}

// Parts of a key under the settings of the stores: the store and its setting, then what is under that, such as the name of a variable
func parts(key string) (store, setting, rest string) {
	under, _ := strings.CutPrefix(key, secretsconfig.Key+".")
	store, after, _ := strings.Cut(under, ".")
	setting, rest, _ = strings.Cut(after, ".")
	return store, setting, rest
}

// Why the value can't be kept under the key, nil when it can
func (h *Secrets) check(cfg *config.Config, key string, value any) error {
	if strings.EqualFold(key, secretsconfig.Sources) {
		var order []string
		switch names := value.(type) {
		case []string:
			order = names
		case []any:
			for _, name := range names {
				order = append(order, fmt.Sprint(name))
			}
		}
		return h.checkOrder(order)
	}
	store, setting, rest := parts(key)
	source, ok := h.Source(store)
	if !ok {
		return nil
	}
	spec := source.Spec()
	if spec.TokenSetting != "" && setting == spec.TokenSetting && rest == "" {
		if name, _ := value.(string); value != nil && !config.EnvName(strings.TrimSpace(name)) {
			return fmt.Errorf("%q can't be the name of a variable. A name has only letters, digits and underscores, and starts with a letter.", name)
		}
		return nil
	}
	if setting != secretsconfig.Env {
		return nil
	}
	if rest != "" {
		if value == nil {
			return nil
		}
		return checkBinding(spec, rest, fmt.Sprint(value))
	}
	table, _ := value.(map[string]any)
	for _, variable := range slices.Sorted(maps.Keys(table)) {
		if err := checkBinding(spec, variable, fmt.Sprint(table[variable])); err != nil {
			return err
		}
	}
	return nil
}

// Makes a write the check takes: a store turned on or off through Switch, any other setting kept as it is, then the stores fetched again
func (h *Secrets) write(cfg *config.Config, key string, value any) (bool, error) {
	if err := h.check(cfg, key, value); err != nil {
		return false, err
	}
	store, setting, rest := parts(key)
	if on, ok := value.(bool); ok && setting == secretsconfig.Enabled && rest == "" {
		if _, known := h.Source(store); known {
			return true, h.Switch(cfg, store, on)
		}
	}
	if err := cfg.Set(key, value); err != nil {
		return true, err
	}
	h.Reset(cfg)
	return true, nil
}
