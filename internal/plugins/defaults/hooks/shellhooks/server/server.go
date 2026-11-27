// Connect service of the shell hooks, through which the hooks commands, the launch's consent prompt and the hooks screen reach the hooks
// of the machine the server runs on
package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	shellhooks "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/hooks"
	"github.com/nickheyer/icebath/internal/server"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1/hooksv1connect"
)

type service struct {
	config *config.Config
	shell  *shellhooks.Registrant
	// Host whose registrants that ask for consent a launch's --accept-hooks gives it to
	hooks *hooks.Hooks
}

func Register(registry *server.Registry, cfg *config.Config, shell *shellhooks.Registrant, host *hooks.Hooks) {
	registry.Register(&service{config: cfg, shell: shell, hooks: host})
}

func (s *service) Mount(services *connect.Server) {
	hooksv1connect.RegisterHooksServiceHandler(services, s)
}

// Error of a change refused for what was asked, its message as it is; any other failure as it is
func refused(err error) error {
	var refusal *shellhooks.Refusal
	if errors.As(err, &refusal) {
		return connect.NewError(connect.CodeFailedPrecondition, refusal.Message)
	}
	return err
}

func (s *service) List(_ context.Context, request *hooksv1.ListRequest) (*hooksv1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	return s.listing(cfg)
}

// Every shell hook and outbound webhook of the profile as the commands and the screen show them
func (s *service) listing(cfg *config.Config) (*hooksv1.ListResponse, error) {
	views, targets, err := s.shell.Overview(cfg)
	if err != nil {
		return nil, err
	}
	out := &hooksv1.ListResponse{Hooks: []*hooksv1.Hook{}, Webhooks: []*hooksv1.Webhook{}, Events: shellhooks.Events(), Points: hooks.Points(),
		AcceptAll: cfg.GetBool(shellconfig.AutoAccept)}
	for _, view := range views {
		out.Hooks = append(out.Hooks, &hooksv1.Hook{Event: view.Event, Command: view.Command, Matcher: view.Matcher.Text, Timeout: int32(view.Timeout),
			FailClosed: view.FailClosed, Allowed: view.Allowed, ApprovedAt: view.ApprovedAt, Changed: view.Drift == 1, ChangedWas: view.DriftWas,
			ChangedNow: view.DriftNow})
	}
	for _, target := range targets {
		out.Webhooks = append(out.Webhooks, &hooksv1.Webhook{Label: target.Label(), Url: target.URL, Events: target.Events, Matcher: target.Matcher.Text,
			Timeout: int32(target.Timeout), Signed: target.Secret != ""})
	}
	return out, nil
}

func (s *service) Change(_ context.Context, request *hooksv1.ChangeRequest) (*hooksv1.ChangeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	event, command := request.GetEvent(), request.GetCommand()
	removed := 0
	switch request.GetVerb() {
	case "add":
		err = s.shell.Add(cfg, event, command, request.GetMatcher(), int(request.GetTimeout()))
	case "remove":
		if removed, err = s.shell.Remove(cfg, event, command); err == nil && removed == 0 {
			err = &shellhooks.Refusal{Message: "There is no hook " + command + " on " + event + "."}
		}
	case "allow":
		err = s.shell.Approve(cfg, event, command)
	case "revoke":
		removed, err = s.shell.Revoke(cfg, command)
	default:
		err = &shellhooks.Refusal{Message: "icebath can't make the change “" + request.GetVerb() + "” to a hook."}
	}
	if err != nil {
		return nil, refused(err)
	}
	listing, err := s.listing(cfg)
	if err != nil {
		return nil, err
	}
	return &hooksv1.ChangeResponse{Hooks: listing, Removed: int32(removed)}, nil
}

// How one run ended, as the commands and the screen show it
func run(r *shellhooks.Run) *hooksv1.Run {
	if r == nil {
		return nil
	}
	out := &hooksv1.Run{Error: r.Error, TimedOut: r.TimedOut, Seconds: r.Seconds, Stdout: r.Stdout, Stderr: r.Stderr, Directive: shellhooks.Wire(r.Directive)}
	if r.Code != nil {
		out.Exited, out.ExitCode = true, int32(*r.Code)
	}
	return out
}

func (s *service) Test(ctx context.Context, request *hooksv1.TestRequest) (*hooksv1.TestResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if request.GetCommand() != "" {
		r, err := s.shell.TestOne(ctx, cfg, request.GetEvent(), request.GetCommand(), request.GetTool())
		if err != nil {
			return nil, refused(err)
		}
		return &hooksv1.TestResponse{Runs: []*hooksv1.Tested{{Command: request.GetCommand(), Run: run(r)}}}, nil
	}
	var fields map[string]any
	if payload := request.GetPayload(); payload != "" {
		if err := json.Unmarshal([]byte(payload), &fields); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "The sample event is not a JSON object: "+strings.TrimSuffix(err.Error(), ".")+".")
		}
	}
	specs, runs, err := s.shell.Test(ctx, cfg, request.GetEvent(), request.GetTool(), fields)
	if err != nil {
		return nil, err
	}
	out := &hooksv1.TestResponse{}
	for i, spec := range specs {
		out.Runs = append(out.Runs, &hooksv1.Tested{Command: spec.Command, Run: run(runs[i])})
	}
	return out, nil
}

func (s *service) Check(ctx context.Context, request *hooksv1.CheckRequest) (*hooksv1.CheckResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	var specs []shellhooks.Spec
	if request.GetEvent() == "" && request.GetCommand() == "" {
		specs = s.shell.Configured(cfg)
	} else {
		spec, ok := s.shell.Find(cfg, request.GetEvent(), request.GetCommand())
		if !ok {
			return nil, refused(&shellhooks.Refusal{Message: "There is no hook " + request.GetCommand() + " on " + request.GetEvent() + "."})
		}
		specs = []shellhooks.Spec{spec}
	}
	out := &hooksv1.CheckResponse{}
	for _, spec := range specs {
		check, err := s.shell.Check(ctx, cfg, spec)
		if err != nil {
			return nil, err
		}
		out.Checks = append(out.Checks, &hooksv1.Checked{Event: spec.Event, Command: spec.Command, Timeout: int32(spec.Timeout), Runnable: check.Runnable,
			Allowed: check.Allowed, ApprovedAt: check.ApprovedAt, Compared: check.Compared, Changed: check.Compared && check.Drift == 1,
			ChangedWas: check.DriftWas, ChangedNow: check.DriftNow, Run: run(check.Run), Output: check.Output, Problems: int32(check.Problems)})
	}
	return out, nil
}

func (s *service) Accept(ctx context.Context, request *hooksv1.AcceptRequest) (*hooksv1.AcceptResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := s.hooks.Accept(ctx, cfg); err != nil {
		return nil, err
	}
	return &hooksv1.AcceptResponse{}, nil
}
