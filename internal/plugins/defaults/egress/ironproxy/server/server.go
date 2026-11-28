package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	ironconfig "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/config"
	ironterminal "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/terminal"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/internal/server"
	egressv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/egress/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/egress/v1/egressv1connect"
)

type service struct {
	config  *config.Config
	egress  *ironterminal.Egress
	secrets *secrets.Registry
}

func Register(registry *server.Registry, cfg *config.Config, egress *ironterminal.Egress, sources *secrets.Registry) {
	s := &service{config: cfg, egress: egress, secrets: sources}
	registry.Register(s)
	cfg.Guard(ironconfig.Enabled, config.Guard{Write: s.guard})
}

func (s *service) Mount(services *connect.Server) {
	egressv1connect.RegisterEgressServiceHandler(services, s)
}

func refuse(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, err.Error())
}

func mapping(m ironterminal.Mapping) *egressv1.Mapping {
	return &egressv1.Mapping{Token: m.Token, Env: m.Env, Hosts: m.Hosts, Headers: m.Headers, Aliases: m.Aliases}
}

func (s *service) Status(ctx context.Context, request *egressv1.StatusRequest) (*egressv1.StatusResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	status := s.egress.Status(ctx, cfg)
	response := &egressv1.StatusResponse{Enabled: cfg.GetBool(ironconfig.Enabled), Binary: status.Binary, Version: status.Version, Config: status.Config,
		Authority: status.Authority, Port: int32(status.Port), Pid: int32(status.PID), Listening: status.Listening, CredentialSource: cfg.GetString(ironconfig.Source),
		Enforce: cfg.GetBool(ironconfig.Enforce), Uncovered: ironterminal.Uncovered(cfg, nil)}
	for _, m := range s.egress.Mappings(cfg) {
		response.Mappings = append(response.Mappings, mapping(m))
	}
	return response, nil
}

func (s *service) Install(ctx context.Context, request *egressv1.InstallRequest) (*egressv1.InstallResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	// Every install a person asks for hashes the entry again and repairs it, so a forced one is the same
	binary, err := s.egress.Install(ctx, cfg)
	if err != nil {
		return nil, refuse(err)
	}
	return &egressv1.InstallResponse{Binary: binary, Version: s.egress.Version(ctx, binary)}, nil
}

func (s *service) Start(ctx context.Context, request *egressv1.StartRequest) (*egressv1.StartResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return s.start(ctx, cfg, request.GetSetup())
}

// Starts the profile's proxy: refused with what to do while hiding the keys is off or the key source can't give it the keys, unless the start
// follows a setup that just wrote what it runs with
func (s *service) start(ctx context.Context, cfg *config.Config, setup bool) (*egressv1.StartResponse, error) {
	start := ironterminal.Start{Install: cfg.GetBool(ironconfig.AutoInstall)}
	response := &egressv1.StartResponse{}
	if !setup {
		if !cfg.GetBool(ironconfig.Enabled) {
			response.Refusal = "Hide your keys is off. Turn it on in Key proxy, in /sandbox, or run `icebath egress setup` first."
			return response, nil
		}
		fallback, source := cfg.GetBool(ironconfig.Fallback), cfg.GetString(ironconfig.Source)
		host := s.secrets.Host()
		_, registered := host.Source(ironterminal.Bitwarden)
		refresh := source == ironconfig.FromBitwarden && registered && host.Enabled(cfg, ironterminal.Bitwarden)
		if source == ironconfig.FromBitwarden && !refresh {
			if !fallback {
				response.Refusal = "Key source is Bitwarden, but Bitwarden is off or not set up."
				response.Hint = "Turn on Bitwarden in /secrets, set Key source to Environment in Key proxy, in /sandbox, or turn on Fall back to " +
					"environment there."
				return response, nil
			}
			response.Notes = append(response.Notes, "⚠ Key source is Bitwarden, but Bitwarden is off or not set up, so the proxy reads keys from the "+
				"environment, as Fall back to environment allows. Keys you change in Bitwarden do not reach it.")
		}
		if refresh {
			source, _ := host.Source(ironterminal.Bitwarden)
			if tokenEnv := host.TokenEnv(cfg, source.Spec()); strings.TrimSpace(host.Existing(cfg)[tokenEnv]) == "" {
				response.Refusal = fmt.Sprintf("Key source is Bitwarden, but %s is not set in the environment.", tokenEnv)
				response.Hint = "Export the Bitwarden access token as " + tokenEnv + ", or set Key source to Environment in Key proxy, in /sandbox."
				return response, nil
			}
			if configured, missing := host.Configured(cfg, source); !configured {
				if _, setting, ok := cfg.Setting(missing); ok && setting.Title != "" {
					missing = setting.Title + " of Bitwarden"
				}
				response.Refusal = "Key source is Bitwarden, but " + missing + " is not set."
				response.Hint = "Set it up in /secrets, or set Key source to Environment in Key proxy, in /sandbox."
				return response, nil
			}
		}
		start.Bitwarden, start.Fallback = refresh, fallback
	}
	status, err := s.egress.Start(ctx, cfg, start)
	if err != nil {
		return nil, refuse(err)
	}
	if status.PID == 0 {
		return nil, refuse(errors.New("iron-proxy did not start. Its log is iron-proxy.log in the folder of the key proxy."))
	}
	response.Pid, response.Port, response.Listening = int32(status.PID), int32(status.Port), status.Listening
	return response, nil
}

func (s *service) Stop(_ context.Context, request *egressv1.StopRequest) (*egressv1.StopResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	stopped, err := s.egress.Stop(cfg)
	if err != nil {
		return nil, err
	}
	return &egressv1.StopResponse{Stopped: stopped}, nil
}

func (s *service) Reload(ctx context.Context, request *egressv1.ReloadRequest) (*egressv1.ReloadResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.egress.Reload(ctx, cfg); err != nil {
		return nil, refuse(err)
	}
	return &egressv1.ReloadResponse{}, nil
}

func (s *service) Text(ctx context.Context, request *egressv1.TextRequest) (*egressv1.TextResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return &egressv1.TextResponse{Text: s.egress.Text(ctx, cfg, request.GetShowTokens())}, nil
}
