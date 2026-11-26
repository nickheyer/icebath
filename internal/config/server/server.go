package server

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
)

// Value a secret shows once it is set, the secret itself never leaving the server
const masked = "••••••••"

var kinds = map[config.Kind]configv1.Kind{
	config.Text:   configv1.Kind_KIND_TEXT,
	config.Number: configv1.Kind_KIND_NUMBER,
	config.Toggle: configv1.Kind_KIND_TOGGLE,
	config.Choice: configv1.Kind_KIND_CHOICE,
	config.List:   configv1.Kind_KIND_LIST,
	config.Table:  configv1.Kind_KIND_TABLE,
}

var sources = map[config.Source]configv1.Source{
	config.FromDefault:     configv1.Source_SOURCE_DEFAULT,
	config.FromFile:        configv1.Source_SOURCE_FILE,
	config.FromEnvironment: configv1.Source_SOURCE_ENVIRONMENT,
}

type service struct {
	config *config.Config
}

func Register(host *server.Server, cfg *config.Config) {
	host.Register(&service{config: cfg})
}

func (s *service) Mount(services *connect.Server) {
	configv1connect.RegisterConfigServiceHandler(services, s)
}

// Settings of the profile a request names, the one in use when it names none
func (s *service) in(profile string) (*config.Config, error) {
	if profile != "" && !s.config.Exists(profile) {
		return nil, connect.NewError(connect.CodeNotFound, "No profile named "+profile)
	}
	return s.config.In(profile), nil
}

// Every section, those of a parent by title, and the settings of each in the order they were registered
func (s *service) List(_ context.Context, request *configv1.ListRequest) (*configv1.ListResponse, error) {
	cfg, err := s.in(request.GetProfile())
	if err != nil {
		return nil, err
	}
	sections := cfg.Sections()
	slices.SortStableFunc(sections, func(a, b config.Section) int {
		return cmp.Or(strings.Compare(a.Parent, b.Parent), strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)))
	})
	response := &configv1.ListResponse{Profile: cfg.Profile()}
	for _, section := range sections {
		response.Sections = append(response.Sections, &configv1.Section{Key: section.Key, Parent: section.Parent, Title: section.Title, Description: section.About, Global: cfg.Global(section.Key)})
		for _, setting := range section.Settings {
			message, err := describe(cfg, section, setting)
			if err != nil {
				return nil, err
			}
			response.Settings = append(response.Settings, message)
		}
	}
	return response, nil
}

func (s *service) Get(_ context.Context, request *configv1.GetRequest) (*configv1.GetResponse, error) {
	cfg, err := s.in(request.GetProfile())
	if err != nil {
		return nil, err
	}
	section, setting, ok := cfg.Setting(request.GetKey())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "No setting named "+request.GetKey())
	}
	message, err := describe(cfg, section, setting)
	if err != nil {
		return nil, err
	}
	return &configv1.GetResponse{Setting: message}, nil
}

// Saves a value written in YAML, null putting the default back, once the setting it is for takes it
func (s *service) Set(_ context.Context, request *configv1.SetRequest) (*configv1.SetResponse, error) {
	cfg, err := s.in(request.GetProfile())
	if err != nil {
		return nil, err
	}
	_, setting, ok := cfg.Setting(request.GetKey())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "No setting named "+request.GetKey())
	}
	var value any
	if err := yaml.Unmarshal([]byte(request.GetValue()), &value); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	value, err = setting.Check(value)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := cfg.Set(setting.Key, value); err != nil {
		return nil, err
	}
	return &configv1.SetResponse{}, nil
}

// Setting as the profile of the values has it
func describe(values *config.Config, section config.Section, setting config.Setting) (*configv1.Setting, error) {
	source, variable := values.Source(setting)
	message := &configv1.Setting{
		Key:         setting.Key,
		Section:     section.Key,
		Title:       setting.Title,
		Description: setting.About,
		Kind:        kinds[setting.Kind()],
		Secret:      setting.Secret,
		Internal:    setting.Internal,
		Source:      sources[source],
		Variable:    variable,
		Env:         setting.Env,
	}
	if setting.Options != nil {
		for _, option := range setting.Options() {
			message.Options = append(message.Options, &configv1.Option{Value: option.Value, Title: option.Title, Description: option.About})
		}
	}
	var err error
	if message.DefaultValue, err = encode(setting.Default); err != nil {
		return nil, err
	}
	value := values.Get(setting.Key)
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
