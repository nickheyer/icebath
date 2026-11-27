package server

import (
	"cmp"
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
	changes := map[string]any{onepasswordconfig.Enabled: true}
	if path != "" {
		changes[onepasswordconfig.Binary] = path
	}
	if given := strings.TrimSpace(request.GetAccount()); given != "" {
		changes[onepasswordconfig.Account] = given
	}
	tokenEnv := strings.TrimSpace(request.GetTokenEnv())
	if tokenEnv == "" {
		tokenEnv = s.store.TokenEnv(cfg)
	}
	changes[onepasswordconfig.TokenEnv] = tokenEnv
	response := &onepasswordv1.SetupResponse{Binary: binary, Version: secrets.Version(ctx, binary, nil), TokenEnv: tokenEnv, EnvPath: cfg.EnvPath()}
	// The token goes into the .env before the store is turned on, so a token that could not be kept leaves the settings as they were
	if token := strings.TrimSpace(request.GetToken()); token != "" {
		if err := cfg.SaveEnv(tokenEnv, token); err != nil {
			return nil, err
		}
		response.SavedToken = true
	}
	env := s.store.Host().Existing(cfg)
	response.EnvToken = !response.SavedToken && env[tokenEnv] != ""
	// Only with no token at all is the op session asked who it is, with the account the setup leaves set
	who := cmp.Or(strings.TrimSpace(request.GetAccount()), account(cfg))
	if !response.SavedToken && !response.EnvToken {
		response.Identity, _ = onepasswordsecrets.Whoami(ctx, binary, who, "", env)
	}
	for key, value := range changes {
		if err := cfg.Set(key, value); err != nil {
			return nil, err
		}
	}
	response.Account = account(cfg)
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

func (s *service) Map(_ context.Context, request *onepasswordv1.MapRequest) (*onepasswordv1.MapResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.GetEnv())
	valid, warnings := onepasswordsecrets.Validate(map[string]string{name: request.GetReference()})
	response := &onepasswordv1.MapResponse{Warnings: warnings, Enabled: cfg.GetBool(onepasswordconfig.Enabled)}
	reference, ok := valid[name]
	if !ok {
		return response, nil
	}
	if err := cfg.Set(onepasswordconfig.References+"."+name, reference); err != nil {
		return nil, err
	}
	response.Reference = reference
	s.store.Host().Reset(cfg)
	return response, nil
}

func (s *service) Unmap(_ context.Context, request *onepasswordv1.UnmapRequest) (*onepasswordv1.UnmapResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(request.GetEnv())
	if _, mapped := onepasswordsecrets.References(cfg)[strings.ToUpper(name)]; !mapped {
		return &onepasswordv1.UnmapResponse{}, nil
	}
	if err := cfg.Set(onepasswordconfig.References+"."+name, nil); err != nil {
		return nil, err
	}
	s.store.Host().Reset(cfg)
	return &onepasswordv1.UnmapResponse{Mapped: true}, nil
}

func (s *service) Sync(ctx context.Context, request *onepasswordv1.SyncRequest) (*onepasswordv1.SyncResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if !cfg.GetBool(onepasswordconfig.Enabled) {
		return nil, refuse("1Password is off. Run icebath secrets onepassword setup to turn it on.")
	}
	references := onepasswordsecrets.References(cfg)
	if len(references) == 0 {
		return &onepasswordv1.SyncResponse{}, nil
	}
	tokenEnv, existing, override := s.store.TokenEnv(cfg), s.store.Host().Existing(cfg), cfg.GetBool(onepasswordconfig.Override)
	pull := onepasswordsecrets.Pull{Account: account(cfg), TokenEnv: tokenEnv, Pinned: pinned(cfg), Dir: cfg.ProfileDir(cfg.Profile()), Env: existing}
	response := &onepasswordv1.SyncResponse{}
	if request.GetApply() {
		valid, warnings := onepasswordsecrets.Validate(references)
		response.Warnings = warnings
		wanted := map[string]string{}
		var skipped []string
		for _, name := range slices.Sorted(maps.Keys(valid)) {
			if name == tokenEnv || (!override && existing[name] != "") {
				skipped = append(skipped, name)
				continue
			}
			wanted[name] = valid[name]
		}
		exported := map[string]string{}
		if len(wanted) > 0 {
			if onepasswordsecrets.Find(pinned(cfg)) == "" {
				return nil, refuse(onepasswordsecrets.Missing(pinned(cfg)) + " " + onepasswordsecrets.InstallHint)
			}
			pull.References = wanted
			got, more, err := s.store.Pull(ctx, pull)
			if err != nil {
				return nil, refuse(err.Error())
			}
			response.Warnings = append(response.Warnings, more...)
			exported = got
		}
		for _, name := range slices.Sorted(maps.Keys(exported)) {
			response.Rows = append(response.Rows, &onepasswordv1.Row{Name: name, Action: "exported"})
		}
		for _, name := range skipped {
			response.Rows = append(response.Rows, &onepasswordv1.Row{Name: name, Action: "skipped"})
		}
		s.store.Host().Export(cfg, onepasswordconfig.Name, exported)
		response.Exported = int32(len(exported))
		return response, nil
	}
	pull.References = references
	got, warnings, err := s.store.Pull(ctx, pull)
	if err != nil {
		return nil, refuse(err.Error())
	}
	response.Warnings = warnings
	for _, name := range slices.Sorted(maps.Keys(references)) {
		action := secrets.Action(name, tokenEnv, existing, override)
		if _, resolved := got[name]; !resolved && action != "skip_token" {
			action = "unresolved"
		}
		response.Rows = append(response.Rows, &onepasswordv1.Row{Name: name, Action: action})
	}
	return response, nil
}

func (s *service) Disable(_ context.Context, request *onepasswordv1.DisableRequest) (*onepasswordv1.DisableResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := cfg.Set(onepasswordconfig.Enabled, false); err != nil {
		return nil, err
	}
	s.store.Host().Reset(cfg)
	return &onepasswordv1.DisableResponse{}, nil
}
