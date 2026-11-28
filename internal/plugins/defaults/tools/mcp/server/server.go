package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/mcp"
	mcptools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/tools"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/tools"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
)

type service struct {
	config   *config.Config
	manager  *mcptools.Manager
	sessions *sessions.Registry
	tools    *tools.Registry
	// Prompts sessions wait on the user for, which the bridge lists and answers
	approvals *approvals.Approvals
	// Hooks of the tools mcp serve runs, which its --accept-hooks gives consent to
	hooks *hooks.Hooks
	mu    sync.Mutex
	// Messaging bridge of each profile mcp serve reads
	bridges map[string]*bridge
}

func Register(registry *server.Registry, cfg *config.Config, manager *mcptools.Manager, stores *sessions.Registry, toolset *tools.Registry,
	checks *approvals.Approvals, host *hooks.Hooks) {
	registry.Register(&service{config: cfg, manager: manager, sessions: stores, tools: toolset, approvals: checks, hooks: host,
		bridges: map[string]*bridge{}})
}

func (s *service) Mount(services *connect.Server) {
	mcpv1connect.RegisterMcpServiceHandler(services, s)
}

// Stops every server of every profile and every bridge as the server shuts down
func (s *service) Close() error {
	s.mu.Lock()
	for name, b := range s.bridges {
		b.stop()
		delete(s.bridges, name)
	}
	s.mu.Unlock()
	return s.manager.Close()
}

// Settings of the profile named, else of the session's profile, else the one in use
func (s *service) scope(ctx context.Context, profile, session string) (*config.Config, error) {
	if profile != "" || session == "" {
		return server.Profile(s.config, profile)
	}
	store, err := s.sessions.Store(s.config.GetString(sessionsconfig.Store))
	if err != nil {
		return nil, err
	}
	found, err := store.Get(ctx, session)
	if err != nil {
		return nil, err
	}
	return server.Profile(s.config, found.GetProfile())
}

func table(text string) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "The settings of the server cannot be read: "+err.Error()+".")
	}
	return m, nil
}

func encode(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, toString(item))
	}
	return out
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if text, ok := v.(string); ok {
		return text
	}
	data, _ := json.Marshal(v)
	return string(data)
}

func counts(m map[string]int) map[string]int32 {
	out := map[string]int32{}
	for k, v := range m {
		out[k] = int32(v)
	}
	return out
}

func (s *service) List(_ context.Context, request *mcpv1.ListRequest) (*mcpv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &mcpv1.ListResponse{}
	for _, status := range s.manager.Status(cfg) {
		raw := status.Raw
		filter, _ := raw["tools"].(map[string]any)
		_, included := filter["include"].([]any)
		server := &mcpv1.Server{Name: status.Name, Transport: status.Transport, State: status.State, Error: status.Error, Tools: int32(status.Tools),
			Enabled: status.Enabled, Plugin: status.Plugin, Oauth: status.OAuth, SignedIn: status.SignedIn, Url: toString(raw["url"]),
			Command: toString(raw["command"]), Args: mcptools.Args(raw["args"]), Missing: status.Missing, Included: included, Include: stringList(filter["include"]),
			Exclude: stringList(filter["exclude"]), Auth: toString(raw["auth"]), Settings: encode(raw), Sampling: counts(status.Sampling),
			Elicitation: counts(status.Elicitation), Shown: status.Words(), Values: status.Reads}
		switch env := raw["env"].(type) {
		case []any:
			for _, item := range env {
				name, _, _ := strings.Cut(toString(item), "=")
				server.Env = append(server.Env, name)
			}
		case map[string]any:
			for name := range env {
				server.Env = append(server.Env, name)
			}
		}
		slices.Sort(server.Env)
		response.Servers = append(response.Servers, server)
	}
	return response, nil
}

func specs(list []mcp.EnvSpec) []*mcpv1.EnvSpec {
	out := make([]*mcpv1.EnvSpec, len(list))
	for i, spec := range list {
		out[i] = &mcpv1.EnvSpec{Name: spec.Name, Prompt: spec.Prompt, Required: spec.Required, Secret: spec.Secret, Default: spec.Default}
	}
	return out
}

func (s *service) Catalog(_ context.Context, request *mcpv1.CatalogRequest) (*mcpv1.CatalogResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	response := &mcpv1.CatalogResponse{}
	for _, entry := range s.manager.Catalog(cfg) {
		out := &mcpv1.Entry{Name: entry.Name, Description: entry.Description, Source: entry.Source, Transport: entry.Launch.Type, Url: entry.Launch.URL,
			Command: entry.Launch.Command, Args: entry.Launch.Args, Auth: entry.Auth.Type, Provider: entry.Auth.Provider, Requires: specs(entry.Auth.Env),
			PostInstall: entry.PostInstall, ConnectorSlug: entry.ConnectorSlug, FromSource: entry.Install != nil, DefaultEnabled: entry.Tools.Enabled,
			DefaultExcluded: entry.Tools.Excluded, HasOauthClient: entry.Auth.OAuth["client_id"] != nil, Problem: entry.Problem, Future: entry.Future,
			Manifest: entry.Manifest}
		if entry.Problem == "" {
			out.Missing = specs(s.manager.Missing(cfg, entry))
		}
		if stored := s.manager.Stored(cfg, entry.Name); stored != nil {
			out.Installed = true
			enabled, set := stored["enabled"]
			out.Enabled = !set || enabled == true || toString(enabled) == "true"
		}
		response.Entries = append(response.Entries, out)
	}
	return response, nil
}

func (s *service) Save(_ context.Context, request *mcpv1.SaveRequest) (*mcpv1.SaveResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	settings, err := table(request.GetSettings())
	if err != nil {
		return nil, err
	}
	issues, err := s.manager.Save(cfg, request.GetName(), settings)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &mcpv1.SaveResponse{Issues: issues}, nil
}

func (s *service) Check(_ context.Context, request *mcpv1.CheckRequest) (*mcpv1.CheckResponse, error) {
	settings, err := table(request.GetSettings())
	if err != nil {
		return nil, err
	}
	return &mcpv1.CheckResponse{Issues: s.manager.Check(request.GetName(), settings)}, nil
}

func (s *service) Remove(_ context.Context, request *mcpv1.RemoveRequest) (*mcpv1.RemoveResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	var removed bool
	if request.GetPurge() {
		removed, err = s.manager.Uninstall(cfg, request.GetName())
	} else {
		removed, err = s.manager.Remove(cfg, request.GetName())
	}
	if err != nil {
		return nil, err
	}
	return &mcpv1.RemoveResponse{Removed: removed}, nil
}

func (s *service) Enable(_ context.Context, request *mcpv1.EnableRequest) (*mcpv1.EnableResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.manager.Enable(cfg, request.GetName(), request.GetEnabled()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &mcpv1.EnableResponse{}, nil
}

func (s *service) Probe(ctx context.Context, request *mcpv1.ProbeRequest) (*mcpv1.ProbeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	settings, err := table(request.GetSettings())
	if err != nil {
		return nil, err
	}
	if settings == nil {
		if settings = s.manager.Stored(cfg, request.GetName()); settings == nil {
			return nil, connect.NewError(connect.CodeNotFound, "No MCP server is called “"+request.GetName()+"”. Run icebath mcp list to see yours.")
		}
	}
	timeout := time.Duration(request.GetConnectTimeout() * float64(time.Second))
	probe, err := s.manager.Probe(ctx, cfg, request.GetName(), settings, timeout, request.GetValues())
	response := &mcpv1.ProbeResponse{Ok: err == nil, Initialized: probe.Initialized, SchemaChars: counts(probe.SchemaChars)}
	if err != nil {
		response.Error, response.NextStep = err.Error(), mcptools.NextStep(request.GetName(), err)
		if failed, ok := errors.AsType[*mcptools.ProbeError](err); ok {
			response.Class = failed.Class
		}
		return response, nil
	}
	for _, t := range probe.Tools {
		response.Tools = append(response.Tools, &mcpv1.ToolSummary{Name: t.Name, Description: t.Description})
	}
	if probe.Prompts != nil {
		response.Prompts = new(int32(*probe.Prompts))
	}
	if probe.Resources != nil {
		response.Resources = new(int32(*probe.Resources))
	}
	return response, nil
}

func (s *service) SetFilter(_ context.Context, request *mcpv1.SetFilterRequest) (*mcpv1.SetFilterResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	values := request.GetValues()
	if request.GetClear() {
		values = nil
	} else if values == nil {
		values = []string{}
	}
	if err := s.manager.SetFilter(cfg, request.GetName(), request.GetMode(), values); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return &mcpv1.SetFilterResponse{}, nil
}

func (s *service) SetValue(_ context.Context, request *mcpv1.SetValueRequest) (*mcpv1.SetValueResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.manager.SetValue(cfg, request.GetName(), request.GetValue()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &mcpv1.SetValueResponse{}, nil
}

func (s *service) HasValue(_ context.Context, request *mcpv1.HasValueRequest) (*mcpv1.HasValueResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return &mcpv1.HasValueResponse{Set: s.manager.Value(cfg, request.GetName()) != ""}, nil
}

func (s *service) SaveBearer(_ context.Context, request *mcpv1.SaveBearerRequest) (*mcpv1.SaveBearerResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	headers, err := s.manager.SaveBearer(cfg, request.GetName(), request.GetToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &mcpv1.SaveBearerResponse{Headers: encode(headers), Key: mcp.KeyEnv(request.GetName())}, nil
}

func (s *service) InstallConfig(_ context.Context, request *mcpv1.InstallConfigRequest) (*mcpv1.InstallConfigResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	entry, ok := s.manager.Entry(cfg, request.GetEntry())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "“"+request.GetEntry()+"” is not in the catalog. Run icebath mcp catalog to see what it holds.")
	}
	return &mcpv1.InstallConfigResponse{Settings: encode(s.manager.InstallConfig(cfg, entry, request.GetInstallDir(), request.GetValues()))}, nil
}

func (s *service) GitInstall(ctx context.Context, request *mcpv1.GitInstallRequest) (*mcpv1.GitInstallResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	entry, ok := s.manager.Entry(cfg, request.GetEntry())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "“"+request.GetEntry()+"” is not in the catalog.")
	}
	var out bytes.Buffer
	dir, err := s.manager.GitInstall(ctx, cfg, entry, &out)
	response := &mcpv1.GitInstallResponse{Dir: dir, Output: out.String()}
	if err != nil {
		response.Error = err.Error()
		if failed, ok := errors.AsType[*mcptools.InstallError](err); ok {
			response.Class = failed.Class
		}
	}
	return response, nil
}

func (s *service) Login(ctx context.Context, request *mcpv1.LoginRequest, stream mcpv1connect.McpServiceLoginServerStream) error {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return err
	}
	settings, err := table(request.GetSettings())
	if err != nil {
		return err
	}
	var signin *mcptools.Signin
	if settings != nil {
		signin, err = s.manager.LoginWith(cfg, request.GetName(), settings, request.GetFlow())
	} else {
		signin, err = s.manager.Login(cfg, request.GetName(), request.GetFlow())
	}
	if err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	link, code, err := signin.Login.Link(ctx)
	if err == nil {
		if err := stream.Send(&mcpv1.LoginEvent{Id: signin.ID, Link: link, Code: code, Redirect: signin.Login.Redirect()}); err != nil {
			signin.Login.Fail(err)
			return err
		}
	}
	select {
	case <-signin.Done:
	case <-ctx.Done():
		signin.Login.Fail(errors.New("The sign-in was canceled."))
		<-signin.Done
	}
	event := &mcpv1.LoginEvent{Id: signin.ID, Done: true, Tools: int32(signin.Tools), NoToken: signin.NoToken}
	if signin.Err != nil {
		event.Error = mcp.Redact(signin.Err.Error())
	}
	return stream.Send(event)
}

func (s *service) Callback(_ context.Context, request *mcpv1.CallbackRequest) (*mcpv1.CallbackResponse, error) {
	signin, ok := s.manager.Signin(request.GetId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "That sign-in is no longer running.")
	}
	if err := signin.Login.Deliver(request.GetPasted()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &mcpv1.CallbackResponse{}, nil
}

func (s *service) CancelLogin(_ context.Context, request *mcpv1.CancelLoginRequest) (*mcpv1.CancelLoginResponse, error) {
	if signin, ok := s.manager.Signin(request.GetId()); ok {
		signin.Login.Fail(errors.New("You stopped waiting, so the sign-in was canceled."))
	}
	return &mcpv1.CancelLoginResponse{}, nil
}

func (s *service) Reload(ctx context.Context, request *mcpv1.ReloadRequest) (*mcpv1.ReloadResponse, error) {
	cfg, err := s.scope(ctx, request.GetProfile(), request.GetSession())
	if err != nil {
		return nil, err
	}
	r := s.manager.Reload(ctx, cfg)
	return &mcpv1.ReloadResponse{Reconnected: r.Reconnected, Added: r.Added, Removed: r.Removed, Failed: r.Failed, Tools: int32(r.Tools),
		Servers: int32(r.Servers)}, nil
}

func operation(op *mcptools.Operation) *mcpv1.Operation {
	out := &mcpv1.Operation{Id: op.ID, Session: op.Session, Action: op.Action, DeadlineUnix: op.Deadline.Unix(), Settled: op.Settled, Seq: int32(op.Seq),
		ToolCall: op.ToolCall}
	for _, t := range op.Targets {
		out.Targets = append(out.Targets, &mcpv1.Target{Name: t.Name, Action: t.Action, State: t.State, Detail: t.Detail, Instructions: t.Instructions,
			ConnectUrl: t.ConnectURL, Required: specs(t.Required), Tools: t.Tools, DiscoveryError: t.DiscoveryError, Signin: t.Signin})
	}
	return out
}

func (s *service) Watch(ctx context.Context, request *mcpv1.WatchRequest, stream mcpv1connect.McpServiceWatchServerStream) error {
	cfg, err := s.scope(ctx, request.GetProfile(), request.GetSession())
	if err != nil {
		return err
	}
	s.manager.Start(cfg)
	for op := range s.manager.WatchOperations(ctx, request.GetSession()) {
		if err := stream.Send(operation(op)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *service) Respond(_ context.Context, request *mcpv1.RespondRequest) (*mcpv1.RespondResponse, error) {
	var answers []mcptools.Answer
	for _, a := range request.GetAnswers() {
		answers = append(answers, mcptools.Answer{Name: a.GetName(), Status: a.GetStatus(), Env: a.GetEnv()})
	}
	if err := s.manager.Respond(request.GetId(), answers, request.GetSettle()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &mcpv1.RespondResponse{}, nil
}

func (s *service) Wake(_ context.Context, request *mcpv1.WakeRequest) (*mcpv1.WakeResponse, error) {
	s.manager.Wake(request.GetId())
	return &mcpv1.WakeResponse{}, nil
}

func (s *service) Retry(_ context.Context, request *mcpv1.RetryRequest) (*mcpv1.RetryResponse, error) {
	if err := s.manager.Retry(request.GetId(), request.GetNames()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &mcpv1.RetryResponse{}, nil
}
