package install

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Arguments of start_bootstrap as the installer UI sends them
type BootstrapArgs struct {
	Commit         *string `json:"commit"`
	Branch         *string `json:"branch"`
	IncludeDesktop *bool   `json:"include_desktop"`
	Home           *string `json:"hermes_home"`
}

// Status get_bootstrap_status reports
type BootstrapStatus struct {
	Running     bool    `json:"running"`
	Completed   bool    `json:"completed"`
	InstallRoot *string `json:"install_root"`
	LastError   *string `json:"last_error"`
}

func itoa(n int) string { return strconv.Itoa(n) }

// Value of an optional string, empty when unset
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Tees one lifecycle event to the installer log before it reaches the UI
func logLifecycle(e Event) {
	switch e.Type {
	case EventManifest:
		names := make([]string, len(e.Stages))
		for i, s := range e.Stages {
			names[i] = s.Name
		}
		slog.Info("manifest received", "stage_count", len(e.Stages), "names", strings.Join(names, ","))
	case EventStage:
		attrs := []any{"stage", e.Name, "state", string(e.State)}
		if e.DurationMs != nil {
			attrs = append(attrs, "duration_ms", *e.DurationMs)
		}
		if e.Error != nil {
			attrs = append(attrs, "error", *e.Error)
		}
		slog.Info("stage transition", attrs...)
	case EventComplete:
		slog.Info("bootstrap complete", "install_root", e.InstallRoot)
	case EventFailed:
		slog.Error("bootstrap FAILED", "stage", deref(e.Stage), "error", deref(e.Error))
	case EventLog:
		if e.Stream == Stderr {
			slog.Warn("stderr: "+e.Line, "target", "bootstrap.log", "stage", deref(e.Stage))
		} else {
			slog.Info(e.Line, "target", "bootstrap.log", "stage", deref(e.Stage))
		}
	}
}

// Runs the whole install for the UI in process, streaming manifest, stage, log and terminal events
func Bootstrap(ctx context.Context, args BootstrapArgs, emit func(Event)) (string, error) {
	send := func(e Event) {
		logLifecycle(e)
		emit(e.SanitizedForUI())
	}
	failed := func(stage, msg string) (string, error) {
		send(FailedEvent(stage, msg))
		return "", errors.New(msg)
	}
	pin := Pin{Commit: args.Commit, Branch: args.Branch}
	if pin.Commit == nil && BuildPinCommit != "" {
		pin.Commit = strPtr(BuildPinCommit)
	}
	if pin.Branch == nil && BuildPinBranch != "" {
		pin.Branch = strPtr(BuildPinBranch)
	}
	includeDesktop := args.IncludeDesktop == nil || *args.IncludeDesktop
	opts := Options{NonInteractive: true, JSON: true, IncludeDesktop: includeDesktop, InstallMethod: MethodSetupApp}
	if pin.Commit != nil {
		opts.Commit = *pin.Commit
	}
	if pin.Branch != nil {
		opts.Branch = *pin.Branch
	}
	if args.Home != nil && *args.Home != "" {
		opts.Home = *args.Home
	}
	slog.Info("bootstrap starting", "commit", opts.Commit, "branch", opts.Branch, "include_desktop", includeDesktop)
	r, err := NewRunner(opts, send)
	if err != nil {
		return failed("", "resolve install options failed: "+err.Error())
	}
	send(LogEvent("", "[bootstrap] release source "+r.opts.RepoURL, Stdout))
	manifest := InstallManifest(includeDesktop)
	send(ManifestEvent(manifest.Stages, manifest.ProtocolVersion))
	for _, stage := range manifest.Stages {
		if !includeDesktop && stage.Name == "desktop" {
			send(StageEvent(stage.Name, StateSkipped, int64Ptr(0), nil, strPtr("skipped by include_desktop=false")))
			continue
		}
		if ctx.Err() != nil {
			return failed(stage.Name, "bootstrap cancelled by user")
		}
		started := time.Now()
		send(StageEvent(stage.Name, StateRunning, nil, nil, nil))
		res, _ := r.RunStage(ctx, stage.Name)
		duration := int64Ptr(time.Since(started).Milliseconds())
		if ctx.Err() != nil {
			send(StageEvent(stage.Name, StateFailed, duration, nil, strPtr("cancelled by user")))
			return failed(stage.Name, "cancelled by user")
		}
		frame := res
		switch {
		case res.OK && res.Skipped:
			send(StageEvent(stage.Name, StateSkipped, duration, &frame, nil))
		case res.OK:
			send(StageEvent(stage.Name, StateSucceeded, duration, &frame, nil))
		default:
			reason := "exit code 1"
			if res.Reason != nil {
				reason = *res.Reason
			}
			send(StageEvent(stage.Name, StateFailed, duration, &frame, strPtr(reason)))
			return failed(stage.Name, reason)
		}
	}
	installRoot := r.InstallDir()
	marker, err := WriteBootstrapMarker(installRoot, pin)
	if err != nil {
		return failed("", "write bootstrap marker failed: "+err.Error())
	}
	if err := CopySelfToHome(); err != nil {
		slog.Warn("failed to copy installer into ICEBATH_HOME (non-fatal)", "error", err)
		send(LogEvent("", fmt.Sprintf("[bootstrap] warning: could not stage updater binary: %v", err), Stdout))
	}
	send(CompleteEvent(installRoot, marker))
	return installRoot, nil
}

// Removes what the installer placed outside the install tree, the shell PATH blocks, shortcuts and the staged installer
func UninstallHooks(report func(string)) error {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	changed, err := UnwireShellPath(userHome)
	for _, rc := range changed {
		report("removed the Icebath Agent PATH line from " + rc)
	}
	if err != nil {
		return err
	}
	for _, p := range shortcutPaths() {
		if rmErr := os.Remove(p); rmErr == nil {
			report("removed " + p)
		} else if !os.IsNotExist(rmErr) {
			return rmErr
		}
	}
	dest := InstallerDest()
	if rmErr := os.Remove(dest); rmErr == nil {
		report("removed " + dest)
	} else if !os.IsNotExist(rmErr) {
		return rmErr
	}
	marker := BootstrapMarkerPath(InstallRootOf(Home()))
	if rmErr := os.Remove(marker); rmErr == nil {
		report("removed " + marker)
	} else if !os.IsNotExist(rmErr) {
		return rmErr
	}
	return nil
}
