// Connect service of the computer use driver, through which the computer-use commands reach the driver of the machine the server runs on
package server

import (
	"context"
	"sync"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/cua"
	"github.com/nickheyer/icebath/internal/server"
	computerusev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/computeruse/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/computeruse/v1/computerusev1connect"
)

type service struct {
	config *config.Config
	driver *cua.Driver
}

func Register(registry *server.Registry, cfg *config.Config, driver *cua.Driver) {
	registry.Register(&service{config: cfg, driver: driver})
}

func (s *service) Mount(services *connect.Server) {
	computerusev1connect.RegisterComputerUseServiceHandler(services, s)
}

// Writes what it is given as events of one kind
type events func(text string)

func (e events) Write(p []byte) (int, error) {
	e(string(p))
	return len(p), nil
}

// Runs a command on the profile's settings, streaming what it prints and then its exit code
func (s *service) stream(name string, out interface {
	Send(*computerusev1.Event) error
},
	run func(cfg *config.Config, stdout, stderr events) (int, error)) error {
	cfg, err := server.Profile(s.config, name)
	if err != nil {
		return err
	}
	// Events are sent one at a time, the first failure to send kept and the rest let go
	var mu sync.Mutex
	var sendErr error
	send := func(event *computerusev1.Event) {
		mu.Lock()
		defer mu.Unlock()
		if sendErr == nil {
			sendErr = out.Send(event)
		}
	}
	stdout := events(func(text string) { send(&computerusev1.Event{Kind: &computerusev1.Event_Out{Out: text}}) })
	stderr := events(func(text string) { send(&computerusev1.Event{Kind: &computerusev1.Event_Err{Err: text}}) })
	code, err := run(cfg, stdout, stderr)
	if err != nil {
		return err
	}
	send(&computerusev1.Event{Kind: &computerusev1.Event_ExitCode{ExitCode: int32(code)}})
	return sendErr
}

func (s *service) Install(ctx context.Context, request *computerusev1.InstallRequest, out computerusev1connect.ComputerUseServiceInstallServerStream) error {
	return s.stream(request.GetProfile(), out, func(cfg *config.Config, stdout, _ events) (int, error) {
		return s.driver.InstallCommand(ctx, cfg, cua.Printer{Out: stdout, Color: request.GetColor()})
	})
}

func (s *service) Status(ctx context.Context, request *computerusev1.StatusRequest, out computerusev1connect.ComputerUseServiceStatusServerStream) error {
	return s.stream(request.GetProfile(), out, func(cfg *config.Config, stdout, _ events) (int, error) {
		return s.driver.StatusCommand(ctx, cfg, stdout)
	})
}

func (s *service) Doctor(ctx context.Context, request *computerusev1.DoctorRequest, out computerusev1connect.ComputerUseServiceDoctorServerStream) error {
	return s.stream(request.GetProfile(), out, func(cfg *config.Config, stdout, stderr events) (int, error) {
		return s.driver.Doctor(ctx, cfg, stdout, stderr, request.GetInclude(), request.GetSkip(), request.GetJson(), request.GetColor())
	})
}

func (s *service) Permissions(ctx context.Context, request *computerusev1.PermissionsRequest,
	out computerusev1connect.ComputerUseServicePermissionsServerStream) error {
	return s.stream(request.GetProfile(), out, func(cfg *config.Config, stdout, _ events) (int, error) {
		return s.driver.PermissionsCommand(ctx, cfg, stdout, request.GetJson())
	})
}

func (s *service) Grant(ctx context.Context, request *computerusev1.GrantRequest, out computerusev1connect.ComputerUseServiceGrantServerStream) error {
	return s.stream(request.GetProfile(), out, func(cfg *config.Config, stdout, stderr events) (int, error) {
		return s.driver.GrantCommand(ctx, cfg, stdout, stderr)
	})
}
