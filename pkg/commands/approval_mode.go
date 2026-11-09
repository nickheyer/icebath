package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Persistent approval modes the approvals command accepts
var ValidApprovalModes = []string{"manual", "smart", "off"}

// Outcome of inspecting or persisting approvals.mode
type ApprovalModeResult struct {
	OK      bool
	Mode    string
	Changed bool
	Message string
}

// Mode the terminal approval guard enforces, read from a fresh config tree of the home
func effectiveApprovalMode(ctx context.Context, h string, env *tools.Env) (string, error) {
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		return "", err
	}
	fresh := *env
	fresh.Config = cfg
	return tools.GetApprovalMode(&fresh), nil
}

var facadeCaptureMu sync.Mutex

// Runs fn with the config facade stdout and notices captured, the hermes redirect of both streams
func captureFacadeOutput(fn func() error) (string, error) {
	facadeCaptureMu.Lock()
	defer facadeCaptureMu.Unlock()
	var buf bytes.Buffer
	prevOut, prevNotice := config.FacadeStdout, config.FacadeNoticeOut
	config.FacadeStdout, config.FacadeNoticeOut = &buf, &buf
	defer func() { config.FacadeStdout, config.FacadeNoticeOut = prevOut, prevNotice }()
	err := fn()
	return buf.String(), err
}

// Inspects or persists approvals.mode through the config facade, reporting the mode the guard now enforces
func RunApprovalModeCommand(ctx context.Context, h string, env *tools.Env, requestedMode string) (ApprovalModeResult, error) {
	current, err := effectiveApprovalMode(ctx, h, env)
	if err != nil {
		return ApprovalModeResult{}, err
	}
	requested := pystr.Lower(pystr.Strip(requestedMode))
	if requested == "" {
		return ApprovalModeResult{true, current, false, fmt.Sprintf("Approval mode: %s (persistent profile setting).", current)}, nil
	}
	if !slices.Contains(ValidApprovalModes, requested) {
		return ApprovalModeResult{false, current, false, "Usage: /approvals [manual|smart|off]"}, nil
	}
	output, err := captureFacadeOutput(func() error {
		return config.SetConfigValue(ctx, h, "approvals.mode", requested, false, configCommandSources(h))
	})
	var exit *config.ConfigExit
	if errors.As(err, &exit) {
		detail := pystr.Strip(output)
		if detail == "" {
			detail = "Approval mode is managed and cannot be changed."
		}
		return ApprovalModeResult{false, current, false, detail}, nil
	}
	if err != nil {
		return ApprovalModeResult{false, current, false, "Failed to save approval mode: " + err.Error()}, nil
	}
	effective, err := effectiveApprovalMode(ctx, h, env)
	if err != nil {
		return ApprovalModeResult{}, err
	}
	if effective != requested {
		return ApprovalModeResult{false, effective, false, fmt.Sprintf("Approval mode remains %s; the requested value did not become effective.", effective)}, nil
	}
	return ApprovalModeResult{true, effective, effective != current, fmt.Sprintf("Approval mode: %s (persistent profile setting).", effective)}, nil
}
