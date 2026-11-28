package server

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
)

// Value a secret shows once it is set, the secret itself never leaving the server
const masked = config.Hidden

var kinds = map[config.Kind]configv1.Kind{
	config.Text:   configv1.Kind_KIND_TEXT,
	config.Number: configv1.Kind_KIND_NUMBER,
	config.Toggle: configv1.Kind_KIND_TOGGLE,
	config.Choice: configv1.Kind_KIND_CHOICE,
	config.List:   configv1.Kind_KIND_LIST,
	config.Table:  configv1.Kind_KIND_TABLE,
	// A length of time, its value the number of its unit
	config.Duration: configv1.Kind_KIND_DURATION,
}

var applies = map[config.Apply]configv1.Apply{
	config.NextMessage: configv1.Apply_APPLY_NEXT_MESSAGE,
	config.NewSessions: configv1.Apply_APPLY_NEW_SESSIONS,
	config.Restart:     configv1.Apply_APPLY_RESTART,
}

var sources = map[config.Source]configv1.Source{
	config.FromDefault:     configv1.Source_SOURCE_DEFAULT,
	config.FromFile:        configv1.Source_SOURCE_FILE,
	config.FromEnvironment: configv1.Source_SOURCE_ENVIRONMENT,
	config.FromManaged:     configv1.Source_SOURCE_MANAGED,
}

type service struct {
	config *config.Config
	// Kinds of provider, whose key variables config check and config show list
	providers *provider.Registry
	// What else changes when config set or unset writes a credential variable of the profile's .env
	credentials Credentials
}

// What else changes when config set or unset writes a credential variable of the profile's .env: the credentials host
type Credentials interface {
	// After the variable was saved over the value it held: copies of the old value in api key settings take the new one, the providers that
	// read it take it again, and what held back the accounts it is the key of on the old value is cleared
	Saved(cfg *config.Config, name, old, value string) error
	// After the variable was removed: the providers that read it leave it out until it is set again, and copies of its old value in api key
	// settings are removed. Whether a setting held a copy
	Removed(cfg *config.Config, name, old string) (bool, error)
}

func Register(host *server.Server, cfg *config.Config, providers *provider.Registry, credentials Credentials) {
	host.Register(&service{config: cfg, providers: providers, credentials: credentials})
}

func (s *service) Mount(services *connect.Server) {
	configv1connect.RegisterConfigServiceHandler(services, s)
}

// Every section in the order the settings screen lists them, the settings of each in the order they were registered, and the groups the
// sections at the top are listed under
func (s *service) List(_ context.Context, request *configv1.ListRequest) (*configv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &configv1.ListResponse{Profile: cfg.Profile()}
	for _, group := range config.Groups() {
		response.Groups = append(response.Groups, &configv1.Group{Id: string(group.ID), Title: group.Title})
	}
	for _, section := range config.Listed(cfg.Sections()) {
		response.Sections = append(response.Sections, &configv1.Section{Key: section.Key, Parent: section.Parent, Title: section.Title,
			Description: section.About, Global: cfg.Global(section.Key), Group: string(section.Group), Area: section.Menu(), Into: section.Into})
		for _, setting := range section.Settings {
			message, err := describe(cfg, section, setting, !request.GetBare())
			if err != nil {
				return nil, err
			}
			response.Settings = append(response.Settings, message)
			if request.GetEntries() && setting.Kind() == config.Table && setting.Things() {
				fields, err := entryFields(cfg, section, setting, !request.GetBare())
				if err != nil {
					return nil, err
				}
				response.Settings = append(response.Settings, fields...)
			}
		}
	}
	return response, nil
}

// The declared fields of every entry the table holds, each a setting keyed below its entry, the entries by name
func entryFields(cfg *config.Config, section config.Section, table config.Setting, options bool) ([]*configv1.Setting, error) {
	held, _ := cfg.Get(table.Key).(map[string]any)
	var out []*configv1.Setting
	for _, name := range slices.Sorted(maps.Keys(held)) {
		// An entry whose name can't be one part of a key, such as one written by hand with a dot in it, has no field a key could name: it
		// is listed by what it holds, and renamed or removed whole
		if config.KeyName(name) != nil {
			continue
		}
		for _, declared := range table.Entry.Fields {
			if declared.Key == "" {
				continue
			}
			field, _ := table.EntryField(name + "." + declared.Key)
			message, err := describe(cfg, section, field, options)
			if err != nil {
				return nil, err
			}
			entryMessage(cfg, field, table, message)
			out = append(out, message)
		}
	}
	return out, nil
}

func (s *service) Get(_ context.Context, request *configv1.GetRequest) (*configv1.GetResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	section, setting, ok := cfg.Setting(request.GetKey())
	entryOf := ""
	if !ok {
		// A declared field of one entry of a table is a setting of its own too
		if table, path, under := cfg.Under(request.GetKey()); under {
			setting, ok = table.EntryField(path)
			section, _, _ = cfg.Setting(table.Key)
			entryOf = table.Key
		}
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "There is no setting named "+request.GetKey()+".")
	}
	message, err := describe(cfg, section, setting, true)
	if err != nil {
		return nil, err
	}
	if entryOf != "" {
		table, _, _ := cfg.Under(request.GetKey())
		entryMessage(cfg, setting, table, message)
	}
	return &configv1.GetResponse{Setting: message}, nil
}

// Marks a declared field of one entry of a table as such. A field the file holds at its default reads as the default: the entry does the
// same without it, and the settings file keeps no empty entry, so a thing with nothing changed is kept by its fields' defaults
func entryMessage(cfg *config.Config, field, table config.Setting, message *configv1.Setting) {
	message.EntryOf = table.Key
	if message.Source == configv1.Source_SOURCE_FILE && same(cfg.Get(field.Key), field.DefaultValue()) {
		message.Source = configv1.Source_SOURCE_DEFAULT
	}
}

// Whether two values are the same value, a whole number and the same number with a point being one
func same(a, b any) bool {
	x, errX := encode(a)
	y, errY := encode(b)
	if errX != nil || errY != nil {
		return false
	}
	if x == y {
		return true
	}
	numberX, errX := strconv.ParseFloat(x, 64)
	numberY, errY := strconv.ParseFloat(y, 64)
	return errX == nil && errY == nil && numberX == numberY
}

// Saves a value written in YAML, null putting the default back, once the setting it is for takes it; a part of a table setting, such as one
// entry, takes any value. The owner of a key that guards it checks the write and may make it itself
func (s *service) Set(_ context.Context, request *configv1.SetRequest) (*configv1.SetResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	_, setting, known := cfg.Setting(request.GetKey())
	if !known {
		table, path, under := cfg.Under(request.GetKey())
		if !under {
			return nil, connect.NewError(connect.CodeNotFound, "There is no setting named "+request.GetKey()+".")
		}
		setting.Key = cfg.Key(request.GetKey())
		// A declared field of one entry is checked as the field it is
		if field, ok := table.EntryField(path); ok {
			setting, known = field, true
		}
	}
	var value any
	if err := yaml.Unmarshal([]byte(request.GetValue()), &value); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if known {
		if value, err = setting.Check(value); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
		if value, err = setting.Located(value); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
	}
	if err := cfg.Change(setting.Key, value); err != nil {
		return nil, refusal(err)
	}
	return &configv1.SetResponse{}, nil
}

// Error of a write as the caller reads it: a locked key or a config.yaml that can't be replaced refused as a failed precondition
func refusal(err error) error {
	if locked, ok := errors.AsType[*config.LockedError](err); ok {
		return connect.NewError(connect.CodePermissionDenied, locked.Error())
	}
	if refused, ok := errors.AsType[*config.RefusedError](err); ok {
		return connect.NewError(connect.CodeFailedPrecondition, refused.Error())
	}
	return err
}

// Setting as the profile of the values has it, with the values it may take when options says so
func describe(values *config.Config, section config.Section, setting config.Setting, options bool) (*configv1.Setting, error) {
	source, variable := values.Source(setting)
	message, err := shape(setting, options)
	if err != nil {
		return nil, err
	}
	message.Section, message.Source, message.Variable = section.Key, sources[source], variable
	if locked := values.Lock(setting.Key); locked != nil {
		message.Locked = locked.Reason()
	}
	value := values.Get(setting.Key)
	message.Shown = setting.Say(value)
	if setting.Secret {
		if text, _ := value.(string); text != "" {
			message.Value = masked
		}
		return message, nil
	}
	if message.Value, err = encode(value); err != nil {
		return nil, err
	}
	return message, nil
}

// What a setting is, whatever value a profile holds for it: its words, its kind, its default, what its zero value means, its bounds and
// the fields of its entries
func shape(setting config.Setting, options bool) (*configv1.Setting, error) {
	message := &configv1.Setting{
		Key:         setting.Key,
		Title:       setting.Title,
		Description: setting.About,
		Help:        setting.Help,
		Kind:        kinds[setting.Kind()],
		Secret:      setting.Secret,
		Internal:    setting.Internal,
		Advanced:    setting.Advanced,
		Apply:       applies[setting.Apply],
		Slash:       setting.Slash,
		Env:         setting.Env,
		Zero:        setting.Zero,
		Counts:      setting.Counts,
		Skill:       setting.Skill,
		Need:        setting.Need,
		Starts:      setting.Starts,
		Bounds:      setting.Bounds(),
		Custom:      setting.Custom,
		Picks:       setting.Picks,
		Ordered:     setting.Ordered,
	}
	if options && setting.Options != nil {
		for _, option := range setting.Options() {
			message.Options = append(message.Options, &configv1.Option{Value: option.Value, Title: option.Title, Description: option.About})
		}
	}
	if path := setting.Path; path != nil {
		message.Path = &configv1.Path{Kind: configv1.PathKind(path.Kind), Exists: path.Exists}
	}
	if options && setting.Names != nil {
		for _, name := range setting.Names() {
			message.Names = append(message.Names, &configv1.Option{Value: name.Value, Title: name.Title, Description: name.About})
		}
	}
	var err error
	if setting.Zero != "" {
		if message.ZeroValue, err = encode(setting.Zeroed()); err != nil {
			return nil, err
		}
	}
	switch setting.Kind() {
	case config.Duration:
		message.Unit, message.Unset = setting.UnitName(), setting.Null
		if setting.Min != nil {
			message.Least, _ = encode(setting.Kept(*setting.Min))
		}
		if setting.Max != nil {
			message.Most, _ = encode(setting.Kept(*setting.Max))
		}
	case config.Number:
		if setting.Least != nil {
			message.Least, _ = encode(*setting.Least)
		}
		if setting.Most != nil {
			message.Most, _ = encode(*setting.Most)
		}
	}
	if message.DefaultValue, err = encode(setting.DefaultValue()); err != nil {
		return nil, err
	}
	if !setting.Secret {
		message.DefaultShown = setting.Say(setting.DefaultValue())
	}
	if entry := setting.Entry; entry != nil {
		message.Entry = &configv1.Entry{Noun: entry.Noun, Name: entry.Name, IgnoreCase: entry.IgnoreCase}
		if entry.Names != nil {
			for _, name := range entry.Names() {
				message.Entry.Names = append(message.Entry.Names, &configv1.Option{Value: name.Value, Title: name.Title, Description: name.About})
			}
		}
		for _, field := range entry.Fields {
			described, err := shape(field, options)
			if err != nil {
				return nil, err
			}
			message.Entry.Fields = append(message.Entry.Fields, described)
		}
	}
	return message, nil
}

// Value as one line of YAML, lists and tables written inline
func encode(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return "", err
	}
	if node.Kind != yaml.ScalarNode {
		node.Style = yaml.FlowStyle
	}
	text, err := yaml.Marshal(&node)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(text)), nil
}
