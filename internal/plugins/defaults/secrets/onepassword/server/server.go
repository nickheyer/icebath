package server

import (
	"context"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	onepasswordsecrets "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/secrets"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/internal/server"
	onepasswordv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1/onepasswordv1connect"
)

type service struct {
	config *config.Config
	store  *onepasswordsecrets.Store
}

func Register(registry *server.Registry, cfg *config.Config, store *onepasswordsecrets.Store) {
	registry.Register(&service{config: cfg, store: store})
}

func (s *service) Mount(services *connect.Server) {
	onepasswordv1connect.RegisterOnePasswordServiceHandler(services, s)
}

func refuse(text string) error {
	return connect.NewError(connect.CodeFailedPrecondition, text)
}

func pinned(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(onepasswordconfig.Binary))
}

func account(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(onepasswordconfig.Account))
}

func (s *service) State(ctx context.Context, request *onepasswordv1.StateRequest) (*onepasswordv1.StateResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	env := s.store.Host().Existing(cfg)
	tokenEnv := s.store.TokenEnv(cfg)
	state := &onepasswordv1.StateResponse{Enabled: cfg.GetBool(onepasswordconfig.Enabled), Account: account(cfg), TokenEnv: tokenEnv, TokenSet: env[tokenEnv] != "",
		Override: cfg.GetBool(onepasswordconfig.Override), CacheTtl: int64(cfg.GetInt(onepasswordconfig.CacheTTL)), BinaryPath: pinned(cfg), EnvPath: cfg.EnvPath()}
	if state.Binary = onepasswordsecrets.Find(pinned(cfg)); state.Binary != "" {
		state.Version = secrets.Version(ctx, state.Binary, nil)
		if !state.TokenSet {
			state.Session, _ = onepasswordsecrets.Whoami(ctx, state.Binary, state.Account, "", env)
		}
	}
	references := onepasswordsecrets.References(cfg)
	for _, name := range slices.Sorted(maps.Keys(references)) {
		state.References = append(state.References, &onepasswordv1.Reference{Env: name, Reference: references[name]})
	}
	return state, nil
}

func (s *service) Accounts(ctx context.Context, request *onepasswordv1.AccountsRequest) (*onepasswordv1.AccountsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	binary := onepasswordsecrets.Find(pinned(cfg))
	if binary == "" {
		return &onepasswordv1.AccountsResponse{BinaryMissing: true}, nil
	}
	accounts, err := onepasswordsecrets.Accounts(ctx, binary, s.store.Host().Existing(cfg))
	if err != nil {
		return &onepasswordv1.AccountsResponse{Error: err.Error()}, nil
	}
	response := &onepasswordv1.AccountsResponse{}
	for _, account := range accounts {
		response.Accounts = append(response.Accounts, &onepasswordv1.Account{Address: account.Address, Email: account.Email})
	}
	return response, nil
}

func (s *service) Setup(ctx context.Context, request *onepasswordv1.SetupRequest) (*onepasswordv1.SetupResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(request.GetBinaryPath())
	if path == "" {
		path = pinned(cfg)
	}
	binary := onepasswordsecrets.Find(path)
	if binary == "" {
		return &onepasswordv1.SetupResponse{BinaryMissing: true, BinaryPath: path}, nil
	}
	tokenEnv := strings.TrimSpace(request.GetTokenEnv())
	if tokenEnv == "" {
		tokenEnv = s.store.TokenEnv(cfg)
	}
	env := s.store.Host().Existing(cfg)
	token, given := strings.TrimSpace(request.GetToken()), strings.TrimSpace(request.GetToken()) != ""
	if !given {
		token = strings.TrimSpace(env[tokenEnv])
	}
	// A token signs in to its own account, so an account is kept only for the sign-in of op
	account := strings.TrimSpace(request.GetAccount())
	if token != "" {
		account = ""
	}
	response := &onepasswordv1.SetupResponse{Binary: binary, Version: secrets.Version(ctx, binary, nil), TokenEnv: tokenEnv, EnvPath: cfg.EnvPath(),
		EnvToken: !given && token != ""}
	identity, ok := onepasswordsecrets.Whoami(ctx, binary, account, token, env)
	if !ok {
		response.NotSignedIn = true
		return response, nil
	}
	response.Identity = identity
	// Kept only once op signed in, the token before the store is turned on so a token that could not be kept leaves the settings as they were
	if given {
		if err := cfg.SaveEnv(tokenEnv, token); err != nil {
			return nil, err
		}
		response.SavedToken = true
		s.store.Clear(cfg.ProfileDir(cfg.Profile()))
	}
	var kept any
	if account != "" {
		kept = account
	}
	changes := map[string]any{onepasswordconfig.Enabled: true, onepasswordconfig.Account: kept}
	if strings.TrimSpace(request.GetTokenEnv()) != "" {
		changes[onepasswordconfig.TokenEnv] = tokenEnv
	}
	if strings.TrimSpace(request.GetBinaryPath()) != "" {
		changes[onepasswordconfig.Binary] = path
	}
	for key, value := range changes {
		if err := cfg.Set(key, value); err != nil {
			return nil, err
		}
	}
	response.Account = account
	s.store.Host().Reset(cfg)
	return response, nil
}

func (s *service) Token(ctx context.Context, request *onepasswordv1.TokenRequest) (*onepasswordv1.TokenResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, refuse("1Password needs a service account token. Make one at https://my.1password.com under Developer › Service accounts.")
	}
	tokenEnv := s.store.TokenEnv(cfg)
	response := &onepasswordv1.TokenResponse{TokenEnv: tokenEnv, EnvPath: cfg.EnvPath(), Disabled: !cfg.GetBool(onepasswordconfig.Enabled)}
	if request.GetVerify() {
		binary := onepasswordsecrets.Find(pinned(cfg))
		if binary == "" {
			response.BinaryMissing, response.Rejected = true, true
			return response, nil
		}
		identity, accepted := onepasswordsecrets.Whoami(ctx, binary, account(cfg), token, s.store.Host().Existing(cfg))
		if !accepted {
			response.Rejected = true
			return response, nil
		}
		response.Identity = identity
	}
	if err := cfg.SaveEnv(tokenEnv, token); err != nil {
		return nil, err
	}
	s.store.Clear(cfg.ProfileDir(cfg.Profile()))
	s.store.Host().Reset(cfg)
	return response, nil
}
