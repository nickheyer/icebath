package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Takes every write a person asks for of a provider's settings and of the model list's settings that name providers, so that the page,
// the command line and the settings service end in the same function and none stores what another would refuse
func (s *service) guard() {
	s.config.Guard(providerconfig.Key, config.Guard{Check: s.checkProvider, Write: s.writeProvider})
	s.config.Guard(modelsconfig.Overrides, config.Guard{Check: s.checkLimits, Write: s.writeLimits})
	s.config.Guard(modelsconfig.CatalogExcluded, config.Guard{Check: s.checkHidden})
	s.config.Guard(modelsconfig.CatalogProviders, config.Guard{Check: s.checkCatalogs})
}

// Parts of a key below providers: the provider, the setting, and for a field of an account the account and the field
type place struct {
	provider, setting, account, field string
	// Parts past the setting or the field, such as the model of a table of models
	rest []string
}

func placed(key string) place {
	parts := strings.Split(key, ".")[1:]
	var at place
	if len(parts) > 0 {
		at.provider = parts[0]
	}
	if len(parts) > 1 {
		at.setting = parts[1]
	}
	if at.setting == providerconfig.Accounts && len(parts) > 2 {
		at.account = parts[2]
		if len(parts) > 3 {
			at.field, at.rest = parts[3], parts[4:]
		}
		return at
	}
	if len(parts) > 2 {
		at.rest = parts[2:]
	}
	return at
}

// Makes a write of a provider's settings through the one function of what it changes: removing or renaming the provider or an account,
// its address, its models, the account it uses, an account's key or variable, taking turns, its place, and the places its credential is
// left out of. What no function owns is checked, then left to the generic write
func (s *service) writeProvider(cfg *config.Config, key string, value any) (bool, error) {
	ctx := context.Background()
	at := placed(key)
	if at.provider == "" {
		if value != nil {
			return false, errors.New("Providers are added one at a time, in Providers or with icebath providers add.")
		}
		for _, instance := range s.providers.Instances(cfg) {
			if _, err := s.providers.Remove(ctx, cfg, instance.ID); err != nil {
				return true, err
			}
		}
		return true, nil
	}
	instance, ok := s.providers.Instance(cfg, at.provider)
	if !ok || instance.ID != at.provider {
		return false, fmt.Errorf("The profile has no provider named %s.", at.provider)
	}
	text, _ := value.(string)
	switch {
	case at.setting == "":
		if value != nil {
			return false, errors.New("A provider is added in Providers or with icebath providers add, not written whole.")
		}
		_, err := s.providers.Remove(ctx, cfg, instance.ID)
		return true, err
	case at.setting == providerconfig.Accounts && at.account == "":
		if value != nil {
			return false, errors.New("An account is added in Providers or with icebath auth add, once it is connected.")
		}
		for _, name := range instance.Accounts {
			if _, err := s.providers.RemoveAccount(ctx, cfg, instance.ID, name); err != nil {
				return true, err
			}
		}
		return true, nil
	case at.account != "":
		return s.writeAccount(ctx, cfg, instance, at, value)
	case len(at.rest) > 0:
		if at.setting != providerconfig.ModelTimeouts {
			return false, nil
		}
		// A key names a model and then at most one of the limits it takes, so more parts are the pieces of a name with dots in it
		if len(at.rest) > 2 || len(at.rest) == 2 && !slices.Contains(timeoutFields, at.rest[1]) {
			return false, fmt.Errorf("%s Set its time limits in Providers, or write Limits by model whole with icebath config set %s.%s.%s and "+
				"a table of the models.", dottedModel, providerconfig.Key, instance.ID, at.setting)
		}
		if value != nil && !timed(cfg, instance, at.rest[0]) {
			return false, s.checkModel(ctx, cfg, instance, at.rest[0])
		}
		return false, nil
	}
	switch at.setting {
	case providerconfig.Type:
		return false, fmt.Errorf("The kind of %s can't change. Add a provider of the other kind instead.", instance.Label())
	case providerconfig.Title:
		_, err := s.providers.Rename(ctx, cfg, instance.ID, text)
		return true, err
	case providerconfig.BaseURL:
		return true, s.providers.SetAddress(cfg, instance.ID, text)
	case providerconfig.DefaultModel:
		return true, s.setDefaultModel(ctx, cfg, instance, text)
	case providerconfig.Models:
		return true, s.setModels(ctx, cfg, instance, value)
	case providerconfig.Account:
		return true, s.providers.UseAccount(cfg, instance.ID, text)
	case providerconfig.Ignored:
		return true, s.setIgnored(cfg, instance, value)
	case providerconfig.ModelTimeouts:
		table, _ := value.(map[string]any)
		for _, model := range slices.Sorted(maps.Keys(table)) {
			if !timed(cfg, instance, model) {
				if err := s.checkModel(ctx, cfg, instance, model); err != nil {
					return false, err
				}
			}
		}
	}
	return false, s.checkProvider(cfg, key, value)
}

// Limits a model of a provider takes for itself, as its entry of the limits by model names them
var timeoutFields = []string{providerconfig.ModelRequestTimeout, providerconfig.ModelStaleTimeout}

// Refusal of a key that names a model with a dot in its name, which a key split on its dots can't say
const dottedModel = "A model whose name holds a dot can't be named in a key, as a key is split on its dots."

// Whether the provider keeps time limits of its own for the model, the model one name whatever it holds
func timed(cfg *config.Config, instance provider.Instance, model string) bool {
	kept, _ := cfg.Stored(providerconfig.Key + "." + instance.ID + "." + providerconfig.ModelTimeouts).(map[string]any)
	return kept[model] != nil
}

// Makes a write of a field of an account through the one function of what it changes: its name, its key or variable, taking turns and its
// place; removing it when the whole account is cleared
func (s *service) writeAccount(ctx context.Context, cfg *config.Config, instance provider.Instance, at place, value any) (bool, error) {
	if !slices.Contains(instance.Accounts, at.account) {
		return false, fmt.Errorf("%s has no account named %s.", instance.Label(), at.account)
	}
	text, _ := value.(string)
	switch {
	case at.field == "" && value == nil:
		_, err := s.providers.RemoveAccount(ctx, cfg, instance.ID, at.account)
		return true, err
	case at.field == "":
		return false, errors.New("An account is changed one field at a time, or connected again in Providers.")
	case len(at.rest) > 0:
		return false, nil
	}
	switch at.field {
	case providerconfig.Title:
		_, err := s.providers.RenameAccount(ctx, cfg, instance.ID, at.account, text)
		return true, err
	case providerconfig.APIKeyField, providerconfig.KeyEnv:
		if strings.TrimSpace(text) == "" {
			if !credentials.Of(instance, at.account).Holds(at.field) {
				return false, fmt.Errorf("The account %s keeps nothing else to sign in with, so it stays as it is. Remove the account instead.",
					instance.AccountTitle(at.account))
			}
			return false, nil
		}
		_, err := s.providers.Connect(cfg, instance.ID, at.account, "", "", map[string]any{at.field: text})
		return true, err
	case providerconfig.InRotation:
		on, _ := value.(bool)
		return true, s.providers.SetTurns(cfg, instance.ID, at.account, on)
	case providerconfig.Priority:
		n, _ := config.Whole(value)
		_, err := s.providers.MoveAccount(cfg, instance.ID, at.account, n)
		return true, err
	}
	return false, s.checkProvider(cfg, providerconfig.Key+"."+instance.ID+"."+providerconfig.Accounts+"."+at.account+"."+at.field, value)
}

// Turns the places the provider leaves its credential out of on and off one at a time, the one control of each, to the list given
func (s *service) setIgnored(cfg *config.Config, instance provider.Instance, value any) error {
	var wanted []string
	switch typed := value.(type) {
	case nil:
	case []string:
		wanted = typed
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				wanted = append(wanted, text)
			}
		}
	default:
		return errors.New("The places a provider leaves out are a list of variables and programs.")
	}
	have := instance.Settings.GetStringSlice(providerconfig.Ignored)
	for _, source := range have {
		if !slices.Contains(wanted, source) {
			if _, err := s.providers.SetOutside(cfg, instance.ID, source, true); err != nil {
				return err
			}
		}
	}
	for _, source := range wanted {
		if !slices.Contains(have, source) {
			if _, err := s.providers.SetOutside(cfg, instance.ID, source, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// Why a value kept under a provider's settings can't be used, without asking the provider anything: an account it uses that it does not
// have or taking turns with none that does, an address that is no address, a variable name that is none. A path is checked by the one rule
// of paths its setting declares, as it is saved
func (s *service) checkProvider(cfg *config.Config, key string, value any) error {
	at := placed(key)
	instance, ok := s.providers.Instance(cfg, at.provider)
	if at.provider == "" || at.setting == "" || !ok || instance.ID != at.provider || len(at.rest) > 0 {
		return nil
	}
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if at.account != "" {
		if at.field == providerconfig.KeyEnv && text != "" && !config.EnvName(text) {
			return fmt.Errorf("%s can't name a variable. A variable name has only letters, digits and underscores, and starts with a letter.", text)
		}
		return nil
	}
	switch {
	case at.setting == providerconfig.Account && text == providerconfig.Rotation:
		if !slices.ContainsFunc(instance.Accounts, func(name string) bool { return instance.AccountFields(name).GetBool(providerconfig.InRotation) }) {
			return fmt.Errorf("None of the accounts of %s takes turns yet. Turn on Takes turns for the accounts that should.", instance.Label())
		}
	case at.setting == providerconfig.Account && text != "" && !slices.Contains(instance.Accounts, text):
		return fmt.Errorf("%s has no account named %s.", instance.Label(), text)
	case at.setting == providerconfig.BaseURL && text != "":
		return provider.Address(strings.TrimRight(text, "/"))
	}
	return nil
}

// Checks a model a setting of the provider names by the one check /model makes: refused when the provider's own list lacks it, taken when
// the list can't be read or the provider can't be asked yet, as a name typed on purpose
func (s *service) checkModel(ctx context.Context, cfg *config.Config, instance provider.Instance, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("Name the model.")
	}
	if _, _, err := s.credentials.Ready(instance, s.transports.Factories()); err != nil {
		return nil
	}
	route, err := s.credentials.Route(instance, model)
	if err != nil {
		return nil
	}
	if verdict := s.models.Validate(ctx, cfg, route); !verdict.Accepted {
		return errors.New(verdict.Message)
	}
	return nil
}

// Default model of the provider, checked as /model checks one; empty puts the kind's own back
func (s *service) setDefaultModel(ctx context.Context, cfg *config.Config, instance provider.Instance, model string) error {
	if model = strings.TrimSpace(model); model == "" {
		return instance.Settings.Set(providerconfig.DefaultModel, nil)
	}
	if err := s.checkModel(ctx, cfg, instance, model); err != nil {
		return err
	}
	return instance.Settings.Set(providerconfig.DefaultModel, model)
}

// Models of the provider the model list offers, each checked as /model checks one; none offers every model it lists
func (s *service) setModels(ctx context.Context, cfg *config.Config, instance provider.Instance, value any) error {
	var models []string
	items, _ := value.([]any)
	for _, item := range items {
		model, _ := item.(string)
		if model = strings.TrimSpace(model); model == "" || slices.Contains(models, model) {
			continue
		}
		if err := s.checkModel(ctx, cfg, instance, model); err != nil {
			return err
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		return instance.Settings.Set(providerconfig.Models, nil)
	}
	return instance.Settings.Set(providerconfig.Models, models)
}

// Checks a write of the limits and abilities of models, then leaves it to the generic write: a model named that the provider lacks is
// refused as /model refuses it
func (s *service) writeLimits(cfg *config.Config, key string, value any) (bool, error) {
	if err := s.checkLimits(cfg, key, value); err != nil {
		return false, err
	}
	parts := strings.Split(key, ".")[2:]
	models := map[string]any{}
	switch len(parts) {
	case 0:
		table, _ := value.(map[string]any)
		for name, inner := range table {
			for model := range asTable(inner) {
				models[name+"\x00"+model] = true
			}
		}
	case 1:
		for model := range asTable(value) {
			models[parts[0]+"\x00"+model] = true
		}
	case 2:
		if value != nil {
			models[parts[0]+"\x00"+parts[1]] = true
		}
	}
	for _, both := range slices.Sorted(maps.Keys(models)) {
		name, model, _ := strings.Cut(both, "\x00")
		if model == modelsconfig.EveryModel || limited(cfg, name, model) {
			continue
		}
		instance, _ := s.providers.Instance(cfg, name)
		if err := s.checkModel(context.Background(), cfg, instance, model); err != nil {
			return false, err
		}
	}
	return false, nil
}

func asTable(value any) map[string]any {
	table, _ := value.(map[string]any)
	return table
}

// Why limits and abilities kept for models can't be used: a name that is no provider of the profile, an entry that is no table of the
// fields a limit sets, a field of the wrong kind
func (s *service) checkLimits(cfg *config.Config, key string, value any) error {
	parts := strings.Split(key, ".")[2:]
	switch len(parts) {
	case 0:
		if value == nil {
			return nil
		}
		table, ok := value.(map[string]any)
		if !ok {
			return errors.New("Limits and abilities are set for each provider and each of its models.")
		}
		for _, name := range slices.Sorted(maps.Keys(table)) {
			if err := s.checkLimits(cfg, modelsconfig.Overrides+"."+name, table[name]); err != nil {
				return err
			}
		}
		return nil
	case 1:
		if instance, ok := s.providers.Instance(cfg, parts[0]); !ok || instance.ID != parts[0] {
			return fmt.Errorf("The profile has no provider named %s, so no limits are kept for it.", parts[0])
		}
		if value == nil {
			return nil
		}
		table, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("Limits and abilities of %s are set for each of its models.", parts[0])
		}
		for _, model := range slices.Sorted(maps.Keys(table)) {
			if err := checkEntry(model, table[model]); err != nil {
				return err
			}
		}
		return nil
	case 2:
		if err := s.checkLimits(cfg, modelsconfig.Overrides+"."+parts[0], nil); err != nil {
			return err
		}
		if value == nil {
			return nil
		}
		return checkEntry(parts[1], value)
	}
	if err := s.checkLimits(cfg, modelsconfig.Overrides+"."+parts[0], nil); err != nil {
		return err
	}
	// A key names a provider, a model and then at most one field of its limits, so more parts, or a part that is no field, are the pieces
	// of a name with dots in it
	whole := fmt.Sprintf("%s Set the limits of such a model in Providers, or write the limits of %s whole with icebath config set %s.%s and a "+
		"table of its models.", dottedModel, parts[0], modelsconfig.Overrides, parts[0])
	switch {
	case len(parts) > 3:
		return errors.New(whole)
	case !slices.Contains(overrideFields, parts[2]):
		return fmt.Errorf("%s is not something a limit sets. A limit sets %s. %s", parts[2], strings.Join(overrideFields, ", "), whole)
	}
	return checkField(parts[2], value)
}

// Why an entry of limits and abilities can't be used
func checkEntry(model string, value any) error {
	entry, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("The limits of %s are a set of named values: %s.", model, strings.Join(overrideFields, ", "))
	}
	for _, field := range slices.Sorted(maps.Keys(entry)) {
		if err := checkField(field, entry[field]); err != nil {
			return err
		}
	}
	return nil
}

// Fields an entry of limits and abilities sets
var overrideFields = []string{modelsconfig.OverrideContext, modelsconfig.OverrideTools, modelsconfig.OverrideVision, modelsconfig.OverrideReasoning,
	modelsconfig.OverrideFamily}

// Why a field of an entry of limits and abilities can't take the value
func checkField(field string, value any) error {
	if value == nil {
		return nil
	}
	switch field {
	case modelsconfig.OverrideContext:
		if n, ok := config.Whole(value); !ok || n < 0 {
			return errors.New("The context window is a whole number of tokens.")
		}
	case modelsconfig.OverrideTools, modelsconfig.OverrideVision, modelsconfig.OverrideReasoning:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s takes true or false.", field)
		}
	case modelsconfig.OverrideFamily:
		if _, ok := value.(string); !ok {
			return errors.New("The model family is a name, such as claude-sonnet.")
		}
	default:
		return fmt.Errorf("%s is not something a limit sets. A limit sets %s.", field, strings.Join(overrideFields, ", "))
	}
	return nil
}

// Why the providers the model list hides can't be kept: one that is no provider of the profile
func (s *service) checkHidden(cfg *config.Config, _ string, value any) error {
	items, _ := value.([]any)
	if list, ok := value.([]string); ok {
		for _, item := range list {
			items = append(items, item)
		}
	}
	for _, item := range items {
		name, _ := item.(string)
		if instance, ok := s.providers.Instance(cfg, strings.TrimSpace(name)); !ok || instance.ID != strings.ToLower(strings.TrimSpace(name)) {
			return fmt.Errorf("The profile has no provider named %s to leave out of the model list.", name)
		}
	}
	return nil
}

// Why the lists of models of single kinds can't be kept: a kind no plugin registers, an address that is no address
func (s *service) checkCatalogs(_ *config.Config, key string, value any) error {
	parts := strings.Split(key, ".")[3:]
	switch {
	case len(parts) == 0 && value != nil:
		table, ok := value.(map[string]any)
		if !ok {
			return errors.New("Lists of models are kept for each kind of provider, each by its address.")
		}
		for _, kind := range slices.Sorted(maps.Keys(table)) {
			if err := s.checkCatalog(kind, table[kind]); err != nil {
				return err
			}
		}
	case len(parts) == 1 && value != nil:
		return s.checkCatalog(parts[0], value)
	case len(parts) > 1 && parts[1] == catalogURL:
		if _, ok := s.providers.Spec(parts[0]); !ok {
			return fmt.Errorf("There is no kind of provider named %s.", parts[0])
		}
		address, _ := value.(string)
		return provider.Address(strings.TrimSpace(address))
	case len(parts) > 1:
		return fmt.Errorf("A list of models is kept by its address alone, not %s.", parts[1])
	}
	return nil
}

// Error of a write as a caller reads it: a locked key refused as denied
func refusal(err error) error {
	if locked, ok := errors.AsType[*config.LockedError](err); ok {
		return connect.NewError(connect.CodePermissionDenied, locked.Refusal())
	}
	return connect.NewError(connect.CodeFailedPrecondition, err.Error())
}
