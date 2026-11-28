// Connect service of the browser tools, through which /browser and `icebath browser` reach the browsers of the machine the server runs on
package server

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	browsertools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/browser/tools"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/tools/redact"
	browserv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/browser/v1/browserv1connect"
)

type service struct {
	config  *config.Config
	browser *browsertools.Browser
	host    *browser.Browser
}

func Register(registry *server.Registry, cfg *config.Config, b *browsertools.Browser, host *browser.Browser) {
	registry.Register(&service{config: cfg, browser: b, host: host})
}

func (s *service) Mount(services *connect.Server) {
	browserv1connect.RegisterBrowserServiceHandler(services, s)
}

// Endpoint safe to show, its credentials masked
func shown(endpoint string) string { return redact.Text(endpoint, false, redact.Ends) }

func (s *service) Status(_ context.Context, request *browserv1.StatusRequest) (*browserv1.StatusResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	endpoint := s.browser.Endpoint(cfg)
	return &browserv1.StatusResponse{Connected: endpoint != "", Url: shown(endpoint), Variable: s.browser.Holder(cfg)}, nil
}

func (s *service) Connect(ctx context.Context, request *browserv1.ConnectRequest, out browserv1connect.BrowserServiceConnectServerStream) error {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return err
	}
	var sent error
	say := func(message, level string) {
		if sent == nil {
			sent = out.Send(&browserv1.ConnectEvent{Event: &browserv1.ConnectEvent_Progress{Progress: &browserv1.Progress{Message: message, Level: level}}})
		}
	}
	connected, endpoint, err := s.browser.Connect(ctx, cfg, request.GetUrl(), say)
	if bad, ok := errors.AsType[*browsertools.BadAddress](err); ok {
		return connect.NewError(connect.CodeInvalidArgument, bad.Error())
	}
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, err.Error())
	}
	if sent != nil {
		return sent
	}
	note := "Connected to your browser at " + shown(endpoint) + ". Every session of the agent browses there."
	if !connected {
		note = "Your browser is not connected yet. The steps above say why."
	}
	return out.Send(&browserv1.ConnectEvent{Event: &browserv1.ConnectEvent_Connected{Connected: &browserv1.Connected{Connected: connected,
		Url: shown(endpoint), Note: note}}})
}

func (s *service) Disconnect(ctx context.Context, request *browserv1.DisconnectRequest) (*browserv1.DisconnectResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	note, held, err := s.browser.Disconnect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &browserv1.DisconnectResponse{Note: note, Held: held}, nil
}

func (s *service) CloseProfile(_ context.Context, request *browserv1.CloseProfileRequest) (*browserv1.CloseProfileResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	closed, message := s.browser.CloseProfile(cfg, request.GetBrowser())
	return &browserv1.CloseProfileResponse{Closed: closed, Message: message}, nil
}

func (s *service) Services(_ context.Context, request *browserv1.ServicesRequest) (*browserv1.ServicesResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &browserv1.ServicesResponse{}
	if chosen, picked := browser.Chosen(cfg); picked {
		response.Chosen = chosen
	}
	for _, place := range s.host.Places(cfg) {
		response.Services = append(response.Services, &browserv1.Service{Name: place.Name, Title: place.Title, About: place.About, State: place.State,
			Ready: place.Ready, Section: place.Section, Chosen: place.Chosen, Keys: place.Keys, Group: place.Group})
	}
	return response, nil
}

func (s *service) Installed(context.Context, *browserv1.InstalledRequest) (*browserv1.InstalledResponse, error) {
	response := &browserv1.InstalledResponse{}
	for _, program := range browsertools.InstalledBrowsers() {
		response.Programs = append(response.Programs, &browserv1.Program{Title: program.Title, Path: program.Path})
	}
	var profiles []browsertools.OwnProfile
	response.Browser, profiles = browsertools.OwnProfiles()
	for _, p := range profiles {
		response.Profiles = append(response.Profiles, &browserv1.OwnProfile{Folder: p.Folder, Name: p.Name, Last: p.Last})
	}
	return response, nil
}
