package server

import (
	"context"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	bitwardenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/config"
	bitwardensecrets "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/secrets"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/internal/server"
	bitwardenv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1/bitwardenv1connect"
)

type service struct {
	config *config.Config
	store  *bitwardensecrets.Store
}

func Register(registry *server.Registry, cfg *config.Config, store *bitwardensecrets.Store) {
	registry.Register(&service{config: cfg, store: store})
}

func (s *service) Mount(services *connect.Server) {
	bitwardenv1connect.RegisterBitwardenServiceHandler(services, s)
}

// Refusal of a request that carries no access token and finds none kept
const needsToken = "Bitwarden needs an access token. Make one in the Bitwarden web app under Secrets Manager › Machine accounts › Access tokens."

// Refusal of a change for what was asked or what Bitwarden answered
func refuse(text string) error {
	return connect.NewError(connect.CodeFailedPrecondition, text)
}

// Access token the profile holds, in its .env or this process's environment
func (s *service) stored(cfg *config.Config) string {
	return strings.TrimSpace(s.store.Host().Existing(cfg)[s.store.TokenEnv(cfg)])
}

func (s *service) State(ctx context.Context, request *bitwardenv1.StateRequest) (*bitwardenv1.StateResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	state := &bitwardenv1.StateResponse{Enabled: cfg.GetBool(bitwardenconfig.Enabled), TokenEnv: s.store.TokenEnv(cfg), TokenSet: s.stored(cfg) != "",
		ProjectId: strings.TrimSpace(cfg.GetString(bitwardenconfig.ProjectID)), ServerUrl: strings.TrimSpace(cfg.GetString(bitwardenconfig.ServerURL)),
		Override: cfg.GetBool(bitwardenconfig.Override), CacheTtl: int64(cfg.GetInt(bitwardenconfig.CacheTTL)), AutoInstall: cfg.GetBool(bitwardenconfig.AutoInstall),
		EnvPath: cfg.EnvPath()}
	if state.Binary = s.store.Find(ctx, cfg, false); state.Binary != "" {
		state.Version = s.store.Version(ctx, state.Binary)
	}
	return state, nil
}

func failure(refusal *bitwardensecrets.Refusal) *bitwardenv1.Failure {
	if refusal == nil {
		return nil
	}
	return &bitwardenv1.Failure{Text: refusal.Text, Cause: refusal.Cause}
}

func (s *service) Validate(ctx context.Context, request *bitwardenv1.ValidateRequest) (*bitwardenv1.ValidateResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	token := s.stored(cfg)
	binary := s.store.Find(ctx, cfg, false)
	switch {
	case !cfg.GetBool(bitwardenconfig.Enabled):
		return &bitwardenv1.ValidateResponse{Skipped: "Bitwarden is off."}, nil
	case token == "":
		return &bitwardenv1.ValidateResponse{Skipped: "No access token is set."}, nil
	case binary == "":
		return &bitwardenv1.ValidateResponse{Skipped: "bws is not installed."}, nil
	}
	response := &bitwardenv1.ValidateResponse{OddToken: !strings.HasPrefix(token, "0.")}
	_, refusal := s.store.Projects(ctx, binary, token, strings.TrimSpace(cfg.GetString(bitwardenconfig.ServerURL)), s.store.Host().Existing(cfg))
	response.Passed, response.Failure = refusal == nil, failure(refusal)
	return response, nil
}

func (s *service) Install(ctx context.Context, request *bitwardenv1.InstallRequest) (*bitwardenv1.InstallResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	// Every install a person asks for hashes the entry again and repairs it, so a forced one is the same
	binary, err := s.store.Install(ctx, cfg)
	if err != nil {
		return nil, refuse(err.Error())
	}
	return &bitwardenv1.InstallResponse{Binary: binary, Version: s.store.Version(ctx, binary)}, nil
}

// Keeps the token in the profile's .env, the profile reading it from then on
func (s *service) keep(cfg *config.Config, token string) (string, error) {
	tokenEnv := s.store.TokenEnv(cfg)
	if err := cfg.SaveEnv(tokenEnv, token); err != nil {
		return "", err
	}
	s.store.Clear(cfg.ProfileDir(cfg.Profile()))
	s.store.Host().Reset(cfg)
	return tokenEnv, nil
}

func (s *service) SaveToken(_ context.Context, request *bitwardenv1.SaveTokenRequest) (*bitwardenv1.SaveTokenResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, refuse(needsToken)
	}
	tokenEnv, err := s.keep(cfg, token)
	if err != nil {
		return nil, err
	}
	return &bitwardenv1.SaveTokenResponse{EnvPath: cfg.EnvPath(), TokenEnv: tokenEnv}, nil
}

// The bws program, installed when it is missing
func (s *service) binary(ctx context.Context, cfg *config.Config) (string, error) {
	if binary := s.store.Find(ctx, cfg, false); binary != "" {
		return binary, nil
	}
	binary, err := s.store.Install(ctx, cfg)
	if err != nil {
		return "", refuse("icebath could not install bws, the program it reads Bitwarden with: " + secrets.Sentence(err.Error()))
	}
	return binary, nil
}

func (s *service) Projects(ctx context.Context, request *bitwardenv1.ProjectsRequest) (*bitwardenv1.ProjectsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		token = s.stored(cfg)
	}
	if token == "" {
		return nil, refuse(needsToken)
	}
	binary, err := s.binary(ctx, cfg)
	if err != nil {
		return nil, err
	}
	projects, refusal := s.store.Projects(ctx, binary, token, strings.TrimSpace(request.GetServerUrl()), s.store.Host().Existing(cfg))
	response := &bitwardenv1.ProjectsResponse{Failure: failure(refusal)}
	for _, project := range projects {
		response.Projects = append(response.Projects, &bitwardenv1.Project{Id: project.ID, Name: project.Name})
	}
	return response, nil
}

// What the next start would do with each secret pulled
func (s *service) preview(cfg *config.Config, pulled map[string]string, warnings []string) *bitwardenv1.Preview {
	tokenEnv, existing, override := s.store.TokenEnv(cfg), s.store.Host().Existing(cfg), cfg.GetBool(bitwardenconfig.Override)
	preview := &bitwardenv1.Preview{Warnings: warnings}
	for _, name := range slices.Sorted(maps.Keys(pulled)) {
		preview.Rows = append(preview.Rows, &bitwardenv1.Row{Name: name, Action: secrets.Action(name, tokenEnv, existing, override)})
	}
	return preview
}

func (s *service) Setup(ctx context.Context, request *bitwardenv1.SetupRequest) (*bitwardenv1.Preview, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	stored := s.stored(cfg)
	token, server, project := strings.TrimSpace(request.GetToken()), strings.TrimSpace(request.GetServerUrl()), strings.TrimSpace(request.GetProjectId())
	if token == "" {
		token = stored
	}
	switch {
	case token == "":
		return nil, refuse(needsToken)
	case project == "":
		return nil, refuse("Pick the Bitwarden project whose secrets icebath loads.")
	}
	binary, err := s.binary(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pulled, warnings, err := s.store.Pull(ctx, cfg, bitwardensecrets.Pull{Token: token, Project: project, Server: server, Binary: binary,
		Dir: cfg.ProfileDir(cfg.Profile()), Env: s.store.Host().Existing(cfg)})
	if err != nil {
		return nil, refuse(err.Error())
	}
	if token != stored {
		if _, err := s.keep(cfg, token); err != nil {
			return nil, err
		}
	}
	for key, value := range map[string]any{bitwardenconfig.Enabled: true, bitwardenconfig.ProjectID: project, bitwardenconfig.ServerURL: server} {
		if err := cfg.Set(key, value); err != nil {
			return nil, err
		}
	}
	preview := s.preview(cfg, pulled, warnings)
	s.store.Host().Reset(cfg)
	return preview, nil
}

func (s *service) Token(ctx context.Context, request *bitwardenv1.TokenRequest) (*bitwardenv1.TokenResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, refuse(needsToken)
	}
	response := &bitwardenv1.TokenResponse{OddToken: !strings.HasPrefix(token, "0."), Disabled: !cfg.GetBool(bitwardenconfig.Enabled)}
	if request.GetVerify() {
		binary := s.store.Find(ctx, cfg, true)
		if binary == "" {
			response.BinaryMissing = true
			return response, nil
		}
		projects, refusal := s.store.Projects(ctx, binary, token, strings.TrimSpace(cfg.GetString(bitwardenconfig.ServerURL)), s.store.Host().Existing(cfg))
		if refusal != nil {
			response.Failure = failure(refusal)
			return response, nil
		}
		response.Projects = int32(len(projects))
		if project := strings.TrimSpace(cfg.GetString(bitwardenconfig.ProjectID)); project != "" && len(projects) > 0 &&
			!slices.ContainsFunc(projects, func(p bitwardensecrets.Project) bool { return p.ID == project }) {
			response.HiddenProject = project
		}
	}
	if response.TokenEnv, err = s.keep(cfg, token); err != nil {
		return nil, err
	}
	response.EnvPath = cfg.EnvPath()
	return response, nil
}

func (s *service) Sync(ctx context.Context, request *bitwardenv1.SyncRequest) (*bitwardenv1.Preview, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	tokenEnv, token, project := s.store.TokenEnv(cfg), s.stored(cfg), strings.TrimSpace(cfg.GetString(bitwardenconfig.ProjectID))
	switch {
	case !cfg.GetBool(bitwardenconfig.Enabled):
		return nil, refuse("Bitwarden is off. Run icebath secrets bitwarden setup to turn it on.")
	case token == "":
		return nil, refuse("No access token is set in " + tokenEnv + ". Run icebath secrets bitwarden token to add one.")
	case project == "":
		return nil, refuse("No Bitwarden project is chosen. Run icebath secrets bitwarden setup to pick one.")
	}
	pulled, warnings, err := s.store.Pull(ctx, cfg, bitwardensecrets.Pull{Token: token, Project: project, Server: strings.TrimSpace(cfg.GetString(bitwardenconfig.ServerURL)),
		Dir: cfg.ProfileDir(cfg.Profile()), Env: s.store.Host().Existing(cfg)})
	if err != nil {
		return nil, refuse(err.Error())
	}
	apply := request.GetApply()
	existing, override := s.store.Host().Existing(cfg), cfg.GetBool(bitwardenconfig.Override) || apply
	preview := &bitwardenv1.Preview{Warnings: warnings}
	exported := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(pulled)) {
		action := secrets.Action(name, tokenEnv, existing, override)
		if apply && (action == "export" || action == "override") {
			exported[name] = pulled[name]
			action = map[string]string{"export": "exported", "override": "overrode"}[action]
		}
		preview.Rows = append(preview.Rows, &bitwardenv1.Row{Name: name, Action: action})
	}
	if apply {
		s.store.Host().Export(cfg, bitwardenconfig.Name, exported)
		preview.Exported = int32(len(exported))
	}
	return preview, nil
}

func (s *service) Disable(_ context.Context, request *bitwardenv1.DisableRequest) (*bitwardenv1.DisableResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := cfg.Set(bitwardenconfig.Enabled, false); err != nil {
		return nil, err
	}
	s.store.Host().Reset(cfg)
	return &bitwardenv1.DisableResponse{}, nil
}
