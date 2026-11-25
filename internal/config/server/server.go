package server

import (
	"context"
	"regexp"
	"strings"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
)

var secret = regexp.MustCompile(`(key|token|secret|password)$`)

type service struct {
	config *config.Config
}

func Register(host *server.Server, cfg *config.Config) {
	host.Register(&service{config: cfg})
}

func (s *service) Mount(services *connect.Server) {
	configv1connect.RegisterConfigServiceHandler(services, s)
}

func (s *service) List(context.Context, *configv1.ListRequest) (*configv1.ListResponse, error) {
	response := &configv1.ListResponse{}
	for _, key := range s.config.Keys() {
		setting, err := s.setting(key)
		if err != nil {
			return nil, err
		}
		response.Settings = append(response.Settings, setting)
	}
	return response, nil
}

func (s *service) Get(_ context.Context, request *configv1.GetRequest) (*configv1.GetResponse, error) {
	setting, err := s.setting(request.GetKey())
	if err != nil {
		return nil, err
	}
	return &configv1.GetResponse{Setting: setting}, nil
}

func (s *service) Set(_ context.Context, request *configv1.SetRequest) (*configv1.SetResponse, error) {
	if request.GetKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "a setting needs a key")
	}
	var value any
	if err := yaml.Unmarshal([]byte(request.GetValue()), &value); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := s.config.Set(request.GetKey(), value); err != nil {
		return nil, err
	}
	return &configv1.SetResponse{}, nil
}

func (s *service) setting(key string) (*configv1.Setting, error) {
	setting := &configv1.Setting{Key: key}
	value := s.config.Get(key)
	if value == nil {
		return setting, nil
	}
	if secret.MatchString(key) {
		setting.Value = "••••"
		return setting, nil
	}
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return nil, err
	}
	if node.Kind != yaml.ScalarNode {
		node.Style = yaml.FlowStyle
	}
	text, err := yaml.Marshal(&node)
	if err != nil {
		return nil, err
	}
	setting.Value = strings.TrimSpace(string(text))
	return setting, nil
}
