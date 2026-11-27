package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/transport"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
)

// Helper task that writes what a profile is good at, which the kanban board's orchestrator routes work by
const DescribeTask = "profile_describer"

// Helper task of profile descriptions, registered with the helper models
type Describer struct{}

func (Describer) Tasks() []auxiliary.Task {
	return []auxiliary.Task{{Name: DescribeTask, Title: "Profile summaries", About: "The model that writes what each profile is good at", Order: 90,
		Timeout: 60}}
}

type service struct {
	profiles    *profile.Profiles
	tasks       *auxiliary.Registry
	providers   *provider.Registry
	transports  *transport.Registry
	credentials *credentials.Registry
	learned     *auxiliary.State
}

func Register(host *server.Server, p *profile.Profiles, tasks *auxiliary.Registry, providers *provider.Registry, transports *transport.Registry,
	creds *credentials.Registry) {
	host.Register(&service{profiles: p, tasks: tasks, providers: providers, transports: transports, credentials: creds, learned: auxiliary.NewState()})
}

func (s *service) Mount(services *connect.Server) {
	profilev1connect.RegisterProfileServiceHandler(services, s)
}

// Error a caller reads: a request that names no profile it can act on as an invalid argument, a missing one as not found, one already there
// as already existing
func refusal(err error) error {
	if err == nil {
		return nil
	}
	var value *profile.ValueError
	var dist *profile.DistributionError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, fs.ErrExist):
		return connect.NewError(connect.CodeAlreadyExists, err.Error())
	case errors.As(err, &value), errors.As(err, &dist):
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return err
}

func message(info profile.Info) *profilev1.Profile {
	return &profilev1.Profile{Name: info.Name, Active: info.Active, Default: info.Default, Path: info.Path, Sticky: info.Sticky, Model: info.Model,
		Provider: info.Provider, Skills: int32(info.Skills), HasEnv: info.HasEnv, HasSoul: info.HasSoul, Distribution: info.Distribution,
		DistributionVersion: info.DistributionVersion, DistributionSource: info.DistributionSource, Description: info.Description,
		DescriptionAuto: info.DescriptionAuto, DisplayName: info.DisplayName, BotTitle: info.BotTitle, PreviousNames: info.PreviousNames}
}

func (s *service) show(name string) (*profilev1.Profile, error) {
	info, err := s.profiles.Info(name)
	if err != nil {
		return nil, refusal(err)
	}
	return message(info), nil
}

func (s *service) List(context.Context, *profilev1.ListRequest) (*profilev1.ListResponse, error) {
	infos, err := s.profiles.Infos()
	if err != nil {
		return nil, err
	}
	response := &profilev1.ListResponse{Active: s.profiles.Active()}
	for _, info := range infos {
		response.Profiles = append(response.Profiles, message(info))
	}
	return response, nil
}

func (s *service) Show(_ context.Context, request *profilev1.ShowRequest) (*profilev1.ShowResponse, error) {
	shown, err := s.show(request.GetName())
	if err != nil {
		return nil, err
	}
	return &profilev1.ShowResponse{Profile: shown}, nil
}

func (s *service) Create(_ context.Context, request *profilev1.CreateRequest) (*profilev1.CreateResponse, error) {
	made, err := s.profiles.Create(profile.Request{Name: request.GetName(), Clone: request.GetClone(), From: request.GetFrom(), All: request.GetAll(),
		Channels: request.GetChannels(), SyncImports: request.GetSyncImports(), NoSkills: request.GetNoSkills(), Description: request.GetDescription(),
		Soul: request.GetSoul(), NoCredentials: request.GetNoCredentials(), Model: request.GetModel(), Provider: request.GetProvider()})
	if err != nil {
		return nil, refusal(err)
	}
	shown, err := s.show(made.Name)
	if err != nil {
		return nil, err
	}
	return &profilev1.CreateResponse{Profile: shown, Path: made.Path, Source: made.Source, Launch: made.Launch, Seeded: made.Seeded, Notes: made.Notes,
		Warnings: made.Warnings, Keys: made.Keys}, nil
}

func (s *service) Delete(_ context.Context, request *profilev1.DeleteRequest) (*profilev1.DeleteResponse, error) {
	done, err := s.profiles.Delete(request.GetName())
	if err != nil {
		return nil, refusal(err)
	}
	return &profilev1.DeleteResponse{Path: done.Path, Lines: done.Lines, Later: done.Later, Settled: done.Settled, Failure: done.Failure}, nil
}

func (s *service) Use(_ context.Context, request *profilev1.UseRequest) (*profilev1.UseResponse, error) {
	if err := s.profiles.Use(request.GetName()); err != nil {
		return nil, refusal(err)
	}
	shown, err := s.show(request.GetName())
	if err != nil {
		return nil, err
	}
	return &profilev1.UseResponse{Profile: shown}, nil
}

func (s *service) Rename(_ context.Context, request *profilev1.RenameRequest) (*profilev1.RenameResponse, error) {
	done, err := s.profiles.Rename(request.GetOld(), request.GetNew())
	if err != nil {
		return nil, refusal(err)
	}
	return &profilev1.RenameResponse{Path: done.Path, Lines: done.Lines, Later: done.Later, Settled: done.Settled}, nil
}

// Text the profile's helper model writes for the prompt, on the models of that profile
func (s *service) complete(name string) func(ctx context.Context, system, user string) (string, error) {
	return func(ctx context.Context, system, user string) (string, error) {
		temperature := 0.3
		client := &auxiliary.Client{Tasks: s.tasks, Config: s.profiles.Config().In(name), Providers: s.providers, Transports: s.transports,
			Credentials: s.credentials, State: s.learned}
		return client.Complete(ctx, auxiliary.Request{Task: DescribeTask, Messages: auxiliary.Prompt(system, user), MaxTokens: 400,
			Temperature: &temperature})
	}
}

func (s *service) Describe(ctx context.Context, request *profilev1.DescribeRequest) (*profilev1.DescribeResponse, error) {
	name := request.GetName()
	switch {
	case request.GetAll() && !request.GetAuto():
		return nil, connect.NewError(connect.CodeInvalidArgument, "icebath profile describe needs --auto with --all.")
	case request.GetAll() && (request.GetText() != "" || name != ""):
		return nil, connect.NewError(connect.CodeInvalidArgument, "icebath profile describe takes either --all or a profile name and --text, not both.")
	case !request.GetAll() && name == "":
		return nil, connect.NewError(connect.CodeInvalidArgument, "icebath profile describe needs a profile name, or --all with --auto.")
	case request.GetText() != "" && request.GetAuto():
		return nil, connect.NewError(connect.CodeInvalidArgument, "icebath profile describe takes --text or --auto, not both.")
	}
	if !request.GetAuto() && request.GetText() == "" {
		canon, err := profile.Canon(name)
		if err != nil {
			return nil, refusal(err)
		}
		if !s.profiles.Exists(canon) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Sprintf("There is no profile named %s. Run icebath profile list to see your profiles.", canon))
		}
		meta := profile.ReadMeta(s.profiles.Dir(canon))
		return &profilev1.DescribeResponse{Description: meta.Description, DescriptionAuto: meta.DescriptionAuto}, nil
	}
	if text := request.GetText(); text != "" {
		if err := s.profiles.SetDescription(name, text); err != nil {
			return nil, refusal(err)
		}
		return &profilev1.DescribeResponse{Outcomes: []*profilev1.Outcome{{Name: name, Ok: true, Reason: "described", Description: text}}}, nil
	}
	targets := []string{name}
	if request.GetAll() {
		missing, err := s.profiles.Describable(true)
		if err != nil {
			return nil, err
		}
		targets = missing
	}
	response := &profilev1.DescribeResponse{}
	for _, target := range targets {
		outcome, err := s.profiles.Describe(ctx, target, request.GetOverwrite(), s.complete(target))
		if err != nil {
			return nil, refusal(err)
		}
		response.Outcomes = append(response.Outcomes, &profilev1.Outcome{Name: outcome.Name, Ok: outcome.OK, Reason: outcome.Reason,
			Description: outcome.Description})
	}
	return response, nil
}

func (s *service) PurgeIdentity(_ context.Context, request *profilev1.PurgeIdentityRequest) (*profilev1.PurgeIdentityResponse, error) {
	done, err := s.profiles.PurgeIdentity(request.GetName())
	if err != nil {
		return nil, refusal(err)
	}
	return &profilev1.PurgeIdentityResponse{Lines: done.Lines, Settled: done.Settled}, nil
}

func (s *service) MigrateIdentity(_ context.Context, request *profilev1.MigrateIdentityRequest) (*profilev1.MigrateIdentityResponse, error) {
	done, err := s.profiles.MigrateIdentity(request.GetOld(), request.GetNew())
	if err != nil {
		return nil, refusal(err)
	}
	return &profilev1.MigrateIdentityResponse{Lines: done.Lines, Settled: done.Settled}, nil
}

func (s *service) Export(_ context.Context, request *profilev1.ExportRequest) (*profilev1.ExportResponse, error) {
	name := request.GetName()
	if name == "" {
		name = s.profiles.Active()
	}
	data, file, err := s.profiles.Export(name)
	if err != nil {
		return nil, refusal(err)
	}
	canon, _ := profile.Canon(name)
	return &profilev1.ExportResponse{Archive: data, FileName: file, Name: canon}, nil
}

func (s *service) Import(_ context.Context, request *profilev1.ImportRequest) (*profilev1.ImportResponse, error) {
	dir, err := s.profiles.Import(request.GetArchive(), request.GetName())
	if err != nil {
		return nil, refusal(err)
	}
	return &profilev1.ImportResponse{Name: filepath.Base(dir), Path: dir}, nil
}

func manifest(m *profile.Manifest, set map[string]bool) *profilev1.Manifest {
	out := &profilev1.Manifest{Name: m.Name, Version: m.Version, Description: m.Description, Requires: m.Requires, Author: m.Author, License: m.License,
		Owned: m.Owned, Source: m.Source, InstalledAt: m.InstalledAt}
	for _, r := range m.Env {
		requirement := &profilev1.Requirement{Name: r.Name, Description: r.Description, Required: r.Required, Set: set[r.Name]}
		if r.Default != nil {
			if data, err := yaml.Marshal(r.Default); err == nil {
				requirement.Default = strings.TrimSpace(string(data))
			}
		}
		out.Env = append(out.Env, requirement)
	}
	return out
}

func plan(p *profile.Plan) *profilev1.Plan {
	return &profilev1.Plan{Manifest: manifest(p.Manifest, p.Set), Provenance: p.Provenance, Target: p.Target, Existing: p.Existing,
		Distribution: p.Distribution, PreservesConfig: p.PreservesConfig, HasCron: p.HasCron}
}

func (s *service) Plan(ctx context.Context, request *profilev1.PlanRequest) (*profilev1.PlanResponse, error) {
	work, err := os.MkdirTemp("", "icebath_dist_preview_")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	found, err := s.profiles.Plan(ctx, request.GetSource(), work, request.GetName())
	if err != nil {
		return nil, refusal(err)
	}
	return &profilev1.PlanResponse{Plan: plan(found)}, nil
}

func (s *service) Install(ctx context.Context, request *profilev1.InstallRequest) (*profilev1.InstallResponse, error) {
	done, err := s.profiles.Install(ctx, request.GetSource(), request.GetName(), request.GetForce())
	if err != nil {
		return nil, refusal(err)
	}
	return &profilev1.InstallResponse{Plan: plan(done)}, nil
}

func (s *service) Update(ctx context.Context, request *profilev1.UpdateRequest) (*profilev1.UpdateResponse, error) {
	done, err := s.profiles.Update(ctx, request.GetName(), request.GetForceConfig())
	if err != nil {
		return nil, refusal(err)
	}
	return &profilev1.UpdateResponse{Plan: plan(done)}, nil
}

func (s *service) Info(_ context.Context, request *profilev1.InfoRequest) (*profilev1.InfoResponse, error) {
	found, err := s.profiles.Manifest(request.GetName())
	if err != nil {
		return nil, refusal(err)
	}
	if found == nil {
		return &profilev1.InfoResponse{}, nil
	}
	return &profilev1.InfoResponse{Found: true, Manifest: manifest(found, nil)}, nil
}
