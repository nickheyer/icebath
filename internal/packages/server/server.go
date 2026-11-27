// Connect service of the package manager, through which the pm commands run on the machine the server runs on
package server

import (
	"context"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/server"
	packagesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/packages/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/packages/v1/packagesv1connect"
)

type service struct {
	packages *packages.Packages
	config   *config.Config
}

func Register(host *server.Server, cfg *config.Config, p *packages.Packages) {
	host.Register(&service{packages: p, config: cfg})
}

func (s *service) Mount(services *connect.Server) {
	packagesv1connect.RegisterPackageServiceHandler(services, s)
}

// Reporter sending a command's lines and statuses down its stream, the first failure to send kept
type stream struct {
	send     func(*packagesv1.Event) error
	terminal *packagesv1.Terminal
	err      error
}

func (r *stream) Line(text string) {
	if r.err == nil {
		r.err = r.send(&packagesv1.Event{Kind: &packagesv1.Event_Line{Line: text}})
	}
}

func (r *stream) Status(text string) {
	if r.err == nil {
		r.err = r.send(&packagesv1.Event{Kind: &packagesv1.Event_Status{Status: text}})
	}
}

func (r *stream) Terminal() (bool, int) {
	return r.terminal.GetLive(), int(r.terminal.GetWidth())
}

// Ends a command's stream with its exit status, a failure the person can act on becoming its line
func (s *service) finish(r *stream, code int, result string, err error) error {
	done := &packagesv1.Done{ExitCode: int32(code), Result: result}
	if err != nil {
		message, known := s.packages.Failed(err)
		if !known {
			message = "✗ " + strings.TrimSpace(err.Error())
			if !strings.ContainsAny(message[len(message)-1:], ".!?") {
				message += "."
			}
		}
		done.ExitCode, done.Error = 1, message
	}
	if r.err != nil {
		return r.err
	}
	return r.send(&packagesv1.Event{Kind: &packagesv1.Event_Done{Done: done}})
}

// Runs a command of a profile down a stream
func (s *service) run(profile string, terminal *packagesv1.Terminal, send func(*packagesv1.Event) error,
	command func(cfg *config.Config, r *stream) (int, string, error)) error {
	cfg, err := server.Profile(s.config, profile)
	if err != nil {
		return err
	}
	r := &stream{send: send, terminal: terminal}
	code, result, err := command(cfg, r)
	return s.finish(r, code, result, err)
}

func (s *service) Install(ctx context.Context, request *packagesv1.InstallRequest, out packagesv1connect.PackageServiceInstallServerStream) error {
	return s.run(request.GetProfile(), request.GetTerminal(), out.Send, func(cfg *config.Config, r *stream) (int, string, error) {
		code, err := s.packages.Install(ctx, cfg, packages.InstallArgs{Names: request.GetNames(), Extras: request.GetExtras(), Without: request.GetWithout(),
			ToolsOnly: request.GetToolsOnly(), TrustRecorded: request.GetTrustRecorded(), Target: request.GetTarget()}, r)
		return code, "", err
	})
}

func (s *service) Env(_ context.Context, request *packagesv1.EnvRequest) (*packagesv1.EnvResponse, error) {
	return &packagesv1.EnvResponse{Env: s.packages.Env(request.GetNames())}, nil
}

func (s *service) Doctor(ctx context.Context, request *packagesv1.DoctorRequest, out packagesv1connect.PackageServiceDoctorServerStream) error {
	return s.run(request.GetProfile(), request.GetTerminal(), out.Send, func(cfg *config.Config, r *stream) (int, string, error) {
		code, err := s.packages.Doctor(ctx, cfg, request.GetRepair(), r)
		return code, "", err
	})
}

func (s *service) Repair(ctx context.Context, request *packagesv1.RepairRequest, out packagesv1connect.PackageServiceRepairServerStream) error {
	return s.run(request.GetProfile(), request.GetTerminal(), out.Send, func(cfg *config.Config, r *stream) (int, string, error) {
		code, err := s.packages.Repair(ctx, cfg, r)
		return code, "", err
	})
}

func (s *service) Environment(ctx context.Context, request *packagesv1.EnvironmentRequest, out packagesv1connect.PackageServiceEnvironmentServerStream) error {
	return s.run(request.GetProfile(), request.GetTerminal(), out.Send, func(cfg *config.Config, r *stream) (int, string, error) {
		python, code, err := s.packages.SideEnvironment(ctx, cfg, packages.EnvironmentArgs{Name: request.GetName(), Extras: request.GetExtras(),
			Requirements: request.GetRequirements()}, r)
		return code, python, err
	})
}

func (s *service) Collect(ctx context.Context, request *packagesv1.CollectRequest, out packagesv1connect.PackageServiceCollectServerStream) error {
	return s.run(request.GetProfile(), request.GetTerminal(), out.Send, func(cfg *config.Config, r *stream) (int, string, error) {
		code, err := s.packages.Collect(ctx, cfg, r)
		return code, "", err
	})
}

func (s *service) Status(_ context.Context, request *packagesv1.StatusRequest) (*packagesv1.StatusResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	text, found := s.packages.Status(cfg)
	return &packagesv1.StatusResponse{Text: text, Found: found}, nil
}

func (s *service) Update(ctx context.Context, request *packagesv1.UpdateRequest, out packagesv1connect.PackageServiceUpdateServerStream) error {
	return s.run(request.GetProfile(), request.GetTerminal(), out.Send, func(cfg *config.Config, r *stream) (int, string, error) {
		code, err := s.packages.Update(ctx, cfg, packages.UpdateArgs{Names: request.GetNames(), Check: request.GetCheck(), Target: request.GetTarget(),
			SkipTarget: request.GetSkipTargets(), UV: request.GetUv(), NPM: request.GetNpm(), Termux: request.GetTermux()}, r)
		return code, "", err
	})
}

func (s *service) Lock(ctx context.Context, request *packagesv1.LockRequest, out packagesv1connect.PackageServiceLockServerStream) error {
	args := packages.LockArgs{Name: request.Bump, Version: request.Version}
	if usage := args.Usage(); usage != "" {
		return connect.NewError(connect.CodeInvalidArgument, usage)
	}
	return s.run(request.GetProfile(), request.GetTerminal(), out.Send, func(cfg *config.Config, r *stream) (int, string, error) {
		code, err := s.packages.Lock(ctx, cfg, args, r)
		return code, "", err
	})
}
