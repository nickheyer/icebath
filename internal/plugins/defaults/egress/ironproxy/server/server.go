package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
	registry.Register(&service{config: cfg, egress: egress, secrets: sources})
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

func mappings(list []*egressv1.Mapping) []ironterminal.Mapping {
	out := make([]ironterminal.Mapping, len(list))
	for i, m := range list {
		out[i] = ironterminal.Mapping{Token: m.GetToken(), Env: m.GetEnv(), Hosts: m.GetHosts(), Headers: m.GetHeaders(), Aliases: m.GetAliases()}
	}
	return out
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

func (s *service) Authority(ctx context.Context, request *egressv1.AuthorityRequest) (*egressv1.AuthorityResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	certificate, _, err := s.egress.Authority(ctx, cfg, false)
	if err != nil {
		return nil, refuse(err)
	}
	return &egressv1.AuthorityResponse{Certificate: certificate}, nil
}

// Names of the secrets of the Bitwarden project for a setup that discovers its keys there, or the lines saying why there are none
func (s *service) bitwardenNames(ctx context.Context, cfg *config.Config) ([]string, []string) {
	host := s.secrets.Host()
	source, ok := host.Source(ironterminal.Bitwarden)
	if !ok || !host.Enabled(cfg, ironterminal.Bitwarden) {
		return nil, []string{"✗ --from-bitwarden reads keys from Bitwarden, but Load from Bitwarden is off in /settings under Secret stores › Bitwarden.",
			"Run `icebath secrets bitwarden setup` first, or run setup without --from-bitwarden."}
	}
	tokenEnv := host.TokenEnv(cfg, source.Spec())
	if strings.TrimSpace(host.Existing(cfg)[tokenEnv]) == "" {
		return nil, []string{"✗ --from-bitwarden reads keys from Bitwarden, but " + tokenEnv + " is not set in the environment.",
			"Export the Bitwarden access token as " + tokenEnv + ", then run setup again."}
	}
	result := host.Fetch(ctx, cfg, source, true)
	if !result.OK() {
		return nil, []string{"✗ The secrets in Bitwarden could not be listed: " + strings.TrimRight(result.Error, ".") + ".",
			"Fix the Bitwarden settings and try again, or run setup without --from-bitwarden, so the proxy reads keys from the environment of this " +
				"computer when it starts."}
	}
	if len(result.Secrets) == 0 {
		return nil, []string{"✗ Bitwarden listed no secrets.", "Check Project in /settings under Secret stores › Bitwarden, and that the access " +
			"token may read that project."}
	}
	names := make([]string, 0, len(result.Secrets))
	for name := range result.Secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

func (s *service) Mint(ctx context.Context, request *egressv1.MintRequest) (*egressv1.MintResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &egressv1.MintResponse{}
	var names []string
	if request.GetFromBitwarden() {
		if names, response.Refusal = s.bitwardenNames(ctx, cfg); names == nil {
			return response, nil
		}
		response.Pulled = int32(len(names))
	} else {
		dotenv := cfg.Dotenv()
		for _, name := range append(ironterminal.BearerNames(), ironterminal.Uncoverable...) {
			shell, _ := s.secrets.Host().Shell(name)
			if strings.TrimSpace(dotenv[name]) != "" && strings.TrimSpace(shell) == "" {
				response.Loaded++
			}
		}
	}
	existing := s.egress.Mappings(cfg)
	response.Existing = int32(len(existing))
	if request.GetRotate() && len(existing) > 0 {
		if response.Backup, err = s.egress.Backup(cfg); err != nil {
			response.BackupError = err.Error()
		}
	}
	for _, m := range ironterminal.Merge(existing, ironterminal.Discover(cfg, names), request.GetRotate()) {
		response.Mappings = append(response.Mappings, mapping(m))
	}
	response.Uncovered = ironterminal.Uncovered(cfg, names)
	return response, nil
}

func (s *service) Write(ctx context.Context, request *egressv1.WriteRequest) (*egressv1.WriteResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	port := int(request.GetTunnelPort())
	if port != 0 && (port < 1 || port > 65534) {
		return &egressv1.WriteResponse{BadPort: true}, nil
	}
	if port == 0 {
		port = cfg.GetInt(ironconfig.TunnelPort)
	}
	hosts := slices.Clone(ironterminal.DefaultHosts)
	for _, h := range cfg.GetStringSlice(ironconfig.ExtraHosts) {
		if !slices.Contains(ironterminal.DefaultHosts, h) {
			hosts = append(hosts, h)
		}
	}
	certificate, key, err := s.egress.Authority(ctx, cfg, false)
	if err != nil {
		return nil, refuse(err)
	}
	response := &egressv1.WriteResponse{}
	audit := filepath.Join(s.egress.Dir(cfg), "audit.log")
	if err := ironterminal.Audit(audit); err != nil {
		response.AuditError = err.Error()
	} else {
		response.Audit = audit
	}
	plan := ironterminal.Plan{Mappings: mappings(request.GetMappings()), CA: certificate, Key: key, Port: port, Hosts: hosts,
		DenyCIDRs: cfg.GetStringSlice(ironconfig.DenyCIDRs), DenySet: cfg.Stored(ironconfig.DenyCIDRs) != nil}
	if response.Config, err = s.egress.WriteConfig(cfg, s.egress.Build(ctx, plan)); err != nil {
		return nil, err
	}
	if response.Mappings, err = s.egress.WriteMappings(cfg, plan.Mappings); err != nil {
		return nil, err
	}
	if _, err := s.egress.Management(cfg, false); err != nil {
		return nil, err
	}
	existing := cfg.GetString(ironconfig.Source)
	source := ironconfig.FromEnv
	switch {
	case request.GetFromBitwarden():
		source = ironconfig.FromBitwarden
	case request.GetNoBitwarden():
		if existing == ironconfig.FromBitwarden {
			response.Notes = append(response.Notes, "Key source is now Environment instead of Bitwarden.")
		}
	case existing == ironconfig.FromBitwarden:
		source = existing
		response.Notes = append(response.Notes, "Key source stays Bitwarden, as it was. Run setup with --no-bitwarden to read keys from the environment instead.")
	}
	for key, value := range map[string]any{ironconfig.TunnelPort: port, ironconfig.Enabled: true, ironconfig.Source: source} {
		if err := cfg.Set(key, value); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (s *service) Start(ctx context.Context, request *egressv1.StartRequest) (*egressv1.StartResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	start := ironterminal.Start{Install: cfg.GetBool(ironconfig.AutoInstall)}
	response := &egressv1.StartResponse{}
	if !request.GetSetup() {
		if !cfg.GetBool(ironconfig.Enabled) {
			response.Refusal = "The sandbox proxy is off. Run `icebath egress setup` first."
			return response, nil
		}
		fallback, source := cfg.GetBool(ironconfig.Fallback), cfg.GetString(ironconfig.Source)
		host := s.secrets.Host()
		_, registered := host.Source(ironterminal.Bitwarden)
		refresh := source == ironconfig.FromBitwarden && registered && host.Enabled(cfg, ironterminal.Bitwarden)
		if source == ironconfig.FromBitwarden && !refresh {
			if !fallback {
				response.Refusal = "Key source is Bitwarden, but Bitwarden is off or not set up."
				response.Hint = "Turn on Load from Bitwarden in /settings under Secret stores › Bitwarden, read keys from the environment again with " +
					"`icebath egress setup --no-bitwarden`, or turn on Fall back to environment in /settings under Sandbox proxy."
				return response, nil
			}
			response.Notes = append(response.Notes, "⚠ Key source is Bitwarden, but Bitwarden is off or not set up, so the proxy reads keys from the "+
				"environment, as Fall back to environment allows. Keys you change in Bitwarden do not reach it.")
		}
		if refresh {
			source, _ := host.Source(ironterminal.Bitwarden)
			if tokenEnv := host.TokenEnv(cfg, source.Spec()); strings.TrimSpace(host.Existing(cfg)[tokenEnv]) == "" {
				response.Refusal = fmt.Sprintf("Key source is Bitwarden, but %s is not set in the environment.", tokenEnv)
				response.Hint = "Export the Bitwarden access token as " + tokenEnv + ", or run `icebath egress setup --no-bitwarden` to read keys from " +
					"the environment instead."
				return response, nil
			}
			if configured, missing := host.Configured(cfg, source); !configured {
				if _, setting, ok := cfg.Setting(missing); ok && setting.Title != "" {
					missing = setting.Title + " in /settings under Secret stores › Bitwarden"
				}
				response.Refusal = "Key source is Bitwarden, but " + missing + " is not set."
				response.Hint = "Run `icebath secrets bitwarden setup` to choose the project, or run `icebath egress setup --no-bitwarden` to read keys " +
					"from the environment instead."
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
		return nil, refuse(errors.New("iron-proxy did not start. Its log is iron-proxy.log in the folder of the sandbox proxy."))
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

func (s *service) Disable(ctx context.Context, request *egressv1.DisableRequest) (*egressv1.DisableResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if !cfg.GetBool(ironconfig.Enabled) {
		return &egressv1.DisableResponse{}, nil
	}
	if err := cfg.Set(ironconfig.Enabled, false); err != nil {
		return nil, err
	}
	return &egressv1.DisableResponse{WasEnabled: true, Running: s.egress.Status(ctx, cfg).PID != 0}, nil
}

func (s *service) Text(ctx context.Context, request *egressv1.TextRequest) (*egressv1.TextResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return &egressv1.TextResponse{Text: s.egress.Text(ctx, cfg, request.GetShowTokens())}, nil
}
