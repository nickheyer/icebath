// Connect service of the agent desktop, through which the screen commands and the app reach the screens of the machine the server runs on
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	botdesktopv1 "github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1/botdesktopv1connect"
)

// Longest an install waits for its sudo password
const passwordWait = 5 * time.Minute

type service struct {
	config  *config.Config
	desktop *botdesktop.Desktop

	mu sync.Mutex
	// Installs waiting for their sudo password, by id
	passwords map[string]chan string
}

func Register(srv *server.Server, cfg *config.Config, desktop *botdesktop.Desktop) {
	srv.Register(&service{config: cfg, desktop: desktop, passwords: map[string]chan string{}})
}

func (s *service) Mount(services *connect.Server) {
	botdesktopv1connect.RegisterDesktopServiceHandler(services, s)
}

func (s *service) profile(name string) (*config.Config, error) {
	cfg, err := s.config.Of(name)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return cfg, nil
}

// A failure the person reads as it is
func refusal(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, err.Error())
}

func lease(l botdesktop.Lease) *botdesktopv1.Lease {
	return &botdesktopv1.Lease{Holder: l.Holder, ViewerHash: l.Hash(), Since: l.Since, Reason: l.Reason, Epoch: l.Epoch}
}

func screen(st botdesktop.Status) *botdesktopv1.Screen {
	out := &botdesktopv1.Screen{Profile: st.Profile, Supported: st.Supported, Installed: st.Installed, Missing: st.Missing, Running: st.Running,
		Pid: int32(st.PID), Display: st.Display, Socket: st.Socket, Geometry: st.Geometry, InstallCommand: st.Install, Browser: st.Browser,
		Blocker: st.Blocker, MemoryAvailableMb: int32(st.Available), MemoryLimitMb: int32(st.Limit), Placement: st.Placement, Sandbox: st.Sandbox, Lease: lease(st.Lease)}
	if sw := st.Switch; sw != nil {
		out.ImageSwitch = &botdesktopv1.ImageSwitch{CurrentImage: sw.Current, TargetImage: sw.Target, Containers: int32(len(sw.Sandboxes))}
	}
	return out
}

func (s *service) status(ctx context.Context, cfg *config.Config) (*botdesktopv1.Screen, error) {
	st, err := s.desktop.Status(ctx, cfg)
	if err != nil {
		return nil, refusal(err)
	}
	return screen(st), nil
}

func (s *service) Status(ctx context.Context, request *botdesktopv1.StatusRequest) (*botdesktopv1.Screen, error) {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return nil, err
	}
	return s.status(ctx, cfg)
}

func (s *service) Start(ctx context.Context, request *botdesktopv1.StartRequest) (*botdesktopv1.Screen, error) {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return nil, err
	}
	if _, err := s.desktop.Start(ctx, cfg, botdesktop.DefaultWait); err != nil {
		return nil, refusal(err)
	}
	return s.status(ctx, cfg)
}

func (s *service) Stop(ctx context.Context, request *botdesktopv1.StopRequest) (*botdesktopv1.StopReply, error) {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return nil, err
	}
	if botdesktop.Supported() {
		l, err := s.desktop.Release(cfg, botdesktop.Handback{Unless: !request.GetForce()})
		if err != nil {
			return nil, err
		}
		if l.Holder == botdesktop.Human {
			return nil, connect.NewError(connect.CodePermissionDenied, "A person is using the agent desktop, so it was not stopped. Stop it with force to end what they are doing on it.")
		}
	}
	stopped, err := s.desktop.Stop(ctx, cfg, nil)
	if err != nil {
		return nil, refusal(err)
	}
	status, err := s.status(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &botdesktopv1.StopReply{Stopped: stopped, Status: status}, nil
}

func (s *service) Thumbnail(ctx context.Context, request *botdesktopv1.ThumbnailRequest) (*botdesktopv1.ThumbnailReply, error) {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return nil, err
	}
	withheld := &botdesktopv1.ThumbnailReply{Suppressed: "human_has_control"}
	before := s.desktop.Lease(cfg)
	if before.Holder == botdesktop.Human {
		return withheld, nil
	}
	shot, err := s.desktop.Thumbnail(ctx, cfg, botdesktop.ThumbWidth, botdesktop.ThumbHeight, botdesktop.ThumbQuality)
	if err != nil {
		return nil, refusal(err)
	}
	if s.desktop.Epoch(cfg) != before.Epoch {
		return withheld, nil
	}
	if len(shot) == 0 {
		return &botdesktopv1.ThumbnailReply{}, nil
	}
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "img_"+random()+".jpg")
	if err := os.WriteFile(path, shot, 0o600); err != nil {
		return nil, err
	}
	return &botdesktopv1.ThumbnailReply{Path: path}, nil
}

func random() string {
	raw := make([]byte, 6)
	rand.Read(raw)
	return hex.EncodeToString(raw)
}

func (s *service) Take(_ context.Context, request *botdesktopv1.TakeRequest) (*botdesktopv1.Lease, error) {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return nil, err
	}
	if request.GetViewer() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "Taking the agent desktop needs the ID of the viewer who takes it.")
	}
	l, err := s.desktop.Take(cfg, request.GetViewer(), request.GetReason())
	if err != nil {
		return nil, err
	}
	return lease(l), nil
}

func (s *service) Release(_ context.Context, request *botdesktopv1.ReleaseRequest) (*botdesktopv1.Lease, error) {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return nil, err
	}
	h := botdesktop.Handback{}
	if !request.GetForce() {
		if request.GetViewer() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, "Handing the agent desktop back needs the ID of the viewer who holds it, or force.")
		}
		h.Viewer = request.GetViewer()
	}
	l, err := s.desktop.Release(cfg, h)
	if err != nil {
		return nil, err
	}
	if l.Holder == botdesktop.Human {
		return nil, connect.NewError(connect.CodePermissionDenied, "Another viewer holds the agent desktop, so it was not handed back. Hand it back with force to take it from them.")
	}
	return lease(l), nil
}

func (s *service) Install(ctx context.Context, request *botdesktopv1.InstallRequest, out botdesktopv1connect.DesktopServiceInstallServerStream) error {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return err
	}
	if !botdesktop.Supported() {
		return refusal(errors.New("The agent desktop works only when icebath runs on Linux."))
	}
	if s.desktop.InSandbox(cfg) {
		return refusal(errors.New("The agent desktop of this profile runs inside the sandbox where the agent's commands run, so there is nothing " +
			"to install on this computer. Give that sandbox an image that has the desktop, such as the image icebath builds."))
	}
	command := botdesktop.InstallCommand()
	if command == "" {
		return refusal(errors.New("This computer has none of the package managers that can install the agent desktop: apt-get, dnf or pacman."))
	}
	if err := s.desktop.Claim(cfg); err != nil {
		return refusal(err)
	}
	var mu sync.Mutex
	send := func(event *botdesktopv1.InstallEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() == nil {
			out.Send(event)
		}
	}
	send(&botdesktopv1.InstallEvent{Kind: &botdesktopv1.InstallEvent_Command{Command: command}})
	password := func() (string, error) {
		id := random()
		answer := make(chan string, 1)
		s.mu.Lock()
		s.passwords[id] = answer
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.passwords, id)
			s.mu.Unlock()
		}()
		send(&botdesktopv1.InstallEvent{Kind: &botdesktopv1.InstallEvent_Password{Password: id}})
		select {
		case given := <-answer:
			return given, nil
		case <-ctx.Done():
		case <-time.After(passwordWait):
		}
		return "", nil
	}
	line := func(text string) { send(&botdesktopv1.InstallEvent{Kind: &botdesktopv1.InstallEvent_Line{Line: text}}) }
	// The install outlives a client that goes away, which can no longer answer for it
	code, err := s.desktop.Install(context.WithoutCancel(ctx), cfg, true, password, line)
	if err != nil {
		line("The install failed: " + err.Error())
		code = 1
	}
	status, err := s.status(context.WithoutCancel(ctx), cfg)
	if err != nil {
		return err
	}
	send(&botdesktopv1.InstallEvent{Kind: &botdesktopv1.InstallEvent_Done{Done: &botdesktopv1.InstallDone{Code: int32(code), Status: status}}})
	return nil
}

func (s *service) Password(_ context.Context, request *botdesktopv1.PasswordRequest) (*botdesktopv1.PasswordReply, error) {
	s.mu.Lock()
	answer := s.passwords[request.GetId()]
	s.mu.Unlock()
	if answer == nil {
		return nil, connect.NewError(connect.CodeNotFound, "No install is waiting for a password.")
	}
	select {
	case answer <- request.GetPassword():
	default:
	}
	return &botdesktopv1.PasswordReply{}, nil
}

func (s *service) Switch(ctx context.Context, request *botdesktopv1.SwitchRequest) (*botdesktopv1.SwitchReply, error) {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return nil, err
	}
	image, err := s.desktop.SwitchImage(ctx, cfg, request.GetApprove())
	if err != nil {
		return nil, refusal(err)
	}
	status, err := s.status(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &botdesktopv1.SwitchReply{Image: image, Status: status}, nil
}

func (s *service) Watch(ctx context.Context, request *botdesktopv1.WatchRequest, out botdesktopv1connect.DesktopServiceWatchServerStream) error {
	cfg, err := s.profile(request.GetProfile())
	if err != nil {
		return err
	}
	changes := make(chan botdesktop.Event, 16)
	stop := s.desktop.Listen(cfg, func(of *config.Config, event botdesktop.Event) {
		if of.Profile() != cfg.Profile() {
			return
		}
		select {
		case changes <- event:
		default:
		}
	})
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-changes:
			change := &botdesktopv1.Change{}
			if event.Kind == botdesktop.Leased {
				change.Kind = &botdesktopv1.Change_Lease{Lease: lease(event.Lease)}
			} else {
				status, err := s.status(ctx, cfg)
				if err != nil {
					return err
				}
				change.Kind = &botdesktopv1.Change_Status{Status: status}
			}
			if err := out.Send(change); err != nil {
				return err
			}
		}
	}
}
