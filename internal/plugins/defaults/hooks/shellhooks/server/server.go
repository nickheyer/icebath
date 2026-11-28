// Connect service of the shell hooks, through which the hooks commands, the launch's consent prompt and the page of Hooks reach the hooks
// of the machine the server runs on, each change ending in the one function of what it changes
package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

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
	shell.Guard(cfg)
	registry.Register(&service{config: cfg, shell: shell, hooks: host})
}

func (s *service) Mount(services *connect.Server) {
	hooksv1connect.RegisterHooksServiceHandler(services, s)
}

// Error of a change refused for what was asked, its message as it is; any other failure as it is
func refused(err error) error {
	if refusal, ok := errors.AsType[*shellhooks.Refusal](err); ok {
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

// Every shell hook and outbound webhook of the profile as the commands and the page show them, each with its state
func (s *service) listing(cfg *config.Config) (*hooksv1.ListResponse, error) {
	views, targets, err := s.shell.Overview(cfg)
	if err != nil {
		return nil, err
	}
	accept := cfg.GetBool(shellconfig.AutoAccept)
	out := &hooksv1.ListResponse{Hooks: []*hooksv1.Hook{}, Webhooks: []*hooksv1.Webhook{}, Events: shellhooks.Events(), Points: hooks.Points(),
		AcceptAll: accept, Ignored: shellhooks.Ignored(cfg)}
	for _, view := range views {
		point, _ := hooks.PointOf(view.Event)
		out.Hooks = append(out.Hooks, &hooksv1.Hook{Event: view.Event, Command: view.Command, Matcher: view.Matcher.Text, Timeout: int32(view.Timeout),
			FailClosed: view.FailClosed, Allowed: view.Allowed, ApprovedAt: view.ApprovedAt, Changed: view.Drift == 1, ChangedWas: view.DriftWas,
			ChangedNow: view.DriftNow, State: view.State(accept), Tool: point.Tool, Blocking: point.Blocking, Unasked: accept && !view.Allowed})
	}
	for _, target := range targets {
		webhook := &hooksv1.Webhook{Label: target.Label(), Url: target.URL, Events: target.Events, Matcher: target.Matcher.Text,
			Timeout: int32(target.Timeout), Signed: target.Secret != "", Key: target.Key, Name: target.Name, SecretEnv: target.SecretEnv,
			State: target.State()}
		for _, event := range target.Events {
			if point, _ := hooks.PointOf(event); point.Tool {
				webhook.Tool = true
			}
		}
		if target.Missing {
			webhook.Unsigned = target.SecretEnv + " is not set"
		}
		out.Webhooks = append(out.Webhooks, webhook)
	}
	return out, nil
}

// Changes of a hook's fields an edit asks for, by their names in the settings
func fields(request *hooksv1.ChangeRequest) map[string]any {
	out := map[string]any{}
	for _, field := range request.GetFields() {
		switch field {
		case shellconfig.Command:
			out[field] = request.GetTo()
		case shellconfig.Matcher:
			out[field] = request.GetMatcher()
		case shellconfig.Timeout:
			if request.GetTimeout() > 0 {
				out[field] = int(request.GetTimeout())
			} else {
				out[field] = nil
			}
		case shellconfig.Block:
			if request.GetFailClosed() {
				out[field] = true
			} else {
				out[field] = nil
			}
		default:
			out[field] = nil
		}
	}
	return out
}

func (s *service) Change(_ context.Context, request *hooksv1.ChangeRequest) (*hooksv1.ChangeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	event, command := request.GetEvent(), strings.TrimSpace(request.GetCommand())
	title := shellconfig.PointOf(event).Title
	// A hook that is there already is named as the settings keep it, though a shell made the home folder of its ~
	verb := request.GetVerb()
	if verb != "add" {
		command = s.shell.Stored(cfg, event, command)
	}
	removed, said := 0, ""
	switch verb {
	case "add":
		said, err = s.shell.AddHook(cfg, shellhooks.Hook{Event: event, Command: command, Matcher: request.GetMatcher(), Timeout: int(request.GetTimeout()),
			Block: request.GetFailClosed()}, request.GetAllow())
	case "edit":
		said, err = s.shell.EditHook(cfg, event, command, fields(request))
	case "remove":
		if removed, err = s.shell.Remove(cfg, event, command); err == nil && removed == 0 {
			err = &shellhooks.Refusal{Message: "There is no hook " + command + " on " + title + "."}
		}
		said = "Removed " + command + " from " + title + "."
	case "allow":
		err = s.shell.Approve(cfg, event, command)
		said = command + " is now allowed."
	case "revoke":
		removed, err = s.shell.Revoke(cfg, command)
		said = command + " is no longer allowed. It stays in your settings."
		if removed == 0 {
			said = "You had not allowed " + command + ", so nothing changed."
		}
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
	return &hooksv1.ChangeResponse{Hooks: listing, Removed: int32(removed), Said: said}, nil
}

func (s *service) ChangeWebhook(_ context.Context, request *hooksv1.WebhookRequest) (*hooksv1.ChangeResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	key, said := request.GetKey(), ""
	switch request.GetVerb() {
	case "add":
		key, said, err = s.shell.AddWebhook(cfg, request.GetUrl(), request.GetEvents())
	case "edit":
		var value any
		if err = yaml.Unmarshal([]byte(request.GetValue()), &value); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "The value can't be read: "+err.Error())
		}
		said, err = s.shell.EditWebhook(cfg, key, request.GetField(), value)
	case "remove":
		said, err = s.shell.RemoveWebhook(cfg, key)
	default:
		err = &shellhooks.Refusal{Message: "icebath can't make the change “" + request.GetVerb() + "” to a webhook."}
	}
	if err != nil {
		return nil, refused(err)
	}
	listing, err := s.listing(cfg)
	if err != nil {
		return nil, err
	}
	return &hooksv1.ChangeResponse{Hooks: listing, Said: said, Key: key}, nil
}

func (s *service) TestWebhook(ctx context.Context, request *hooksv1.TestWebhookRequest) (*hooksv1.TestWebhookResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	ok, status, said, err := s.shell.TestWebhook(ctx, cfg, request.GetKey())
	if err != nil {
		return nil, refused(err)
	}
	return &hooksv1.TestWebhookResponse{Ok: ok, Status: int32(status), Said: said}, nil
}

// How one run ended, as the commands and the page show it
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
		command := s.shell.Stored(cfg, request.GetEvent(), request.GetCommand())
		r, err := s.shell.TestOne(ctx, cfg, request.GetEvent(), command, request.GetTool())
		if err != nil {
			return nil, refused(err)
		}
		return &hooksv1.TestResponse{Runs: []*hooksv1.Tested{{Command: command, Run: run(r)}}}, nil
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
		spec, ok := s.shell.Find(cfg, request.GetEvent(), s.shell.Stored(cfg, request.GetEvent(), request.GetCommand()))
		if !ok {
			return nil, refused(&shellhooks.Refusal{Message: "There is no hook " + request.GetCommand() + " on " + shellconfig.PointOf(request.GetEvent()).Title + "."})
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
	waiting := func() int {
		views, _, _ := s.shell.Overview(cfg)
		n := 0
		for _, view := range views {
			if !view.Allowed {
				n++
			}
		}
		return n
	}
	before := waiting()
	if err := s.hooks.Accept(ctx, cfg); err != nil {
		return nil, err
	}
	return &hooksv1.AcceptResponse{Allowed: int32(before - waiting())}, nil
}
