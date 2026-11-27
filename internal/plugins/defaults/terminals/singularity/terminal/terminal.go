package terminal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

const (
	// Value of terminal.backend that picks the backend
	name = "singularity"
	// Where to get Apptainer
	installURL = "https://apptainer.org/docs/admin/main/installation.html"
	// Where an instance sees the profile folder: one any user reaches, as instances run as the user
	home = "/opt/icebath"
)

// The Singularity backend, and what it keeps across its instances
type registrant struct {
	// Skills whose folders and needs an instance is handed
	skills *skills.Registry

	mu sync.Mutex
	// Scratch folders instances of this run used, swept as icebath stops
	scratches []string
	// Last verdict on the scratch folder's size, and when it was taken
	usage struct {
		stamp time.Time
		set   bool
		over  bool
	}
	// Held while a .sif image is built
	building sync.Mutex
}

func Register(registry *terminal.Registry, skills *skills.Registry) {
	registry.Register(&registrant{skills: skills})
}

func (r *registrant) Backends() []terminal.Backend {
	return []terminal.Backend{{
		Name:      name,
		Title:     "Singularity/Apptainer",
		About:     "A rootless Apptainer container, as on shared clusters",
		Where:     "a Singularity container (Linux)",
		Remote:    true,
		Container: true,
		Isolated:  true,
		Home:      home,
		Cwd:       "~",
		Check:     check,
		Setup:     setup,
		Doctor:    doctor,
		Prepare:   r.prepare,
		Create:    r.create,
	}}
}

// The apptainer or singularity program on PATH, apptainer first; empty when neither is installed
func program() string {
	for _, candidate := range []string{"apptainer", "singularity"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}

// Whether the profile's commands run in Singularity, which its checks are logged for; listing the backends checks the others too
func selected(cfg *config.Config) bool {
	return cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.GetString(terminalconfig.Backend)), name)
}

// Why the backend can't run: no program, or one that fails to tell its version. Logged when it is the backend in use, as a listing of
// every backend checks it too
func check(cfg *config.Config) error {
	exe := program()
	if exe == "" {
		if selected(cfg) {
			slog.Error("Singularity backend selected but neither 'apptainer' nor 'singularity' is on PATH. Install Apptainer: " + installURL)
		}
		return fmt.Errorf("Neither apptainer nor singularity is on your PATH. Install Apptainer from %s.", installURL)
	}
	argv := []string{exe, "--version"}
	captured, err := terminal.Capture(context.Background(), argv, 5*time.Second, nil)
	why := ""
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		why = "got no answer within 5 seconds"
	case err != nil:
		why = "could not run: " + strings.TrimRight(err.Error(), ".")
	case captured.Code != 0:
		why = fmt.Sprintf("ended with exit code %d", captured.Code)
	}
	if why != "" {
		text := fmt.Sprintf("Apptainer is installed, but `%s --version` %s, so the installation looks broken. Install Apptainer again from %s.",
			filepath.Base(exe), why, installURL)
		if selected(cfg) {
			slog.Error(text)
		}
		return errors.New(text)
	}
	return nil
}

// The program when it answers, else why it can't be used
func available(ctx context.Context) (string, error) {
	exe := program()
	if exe == "" {
		return "", &terminal.MissingError{Reason: "Neither apptainer nor singularity is on your PATH. Install Apptainer from " + installURL +
			", or Singularity, so its program is on your PATH."}
	}
	captured, err := terminal.Capture(ctx, []string{exe, "version"}, 10*time.Second, nil)
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		return "", fmt.Errorf("`%s version` got no answer within 10 seconds.", filepath.Base(exe))
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("The %s program could not be run.", filepath.Base(exe))
	case err != nil:
		return "", fmt.Errorf("`%s version` could not run: %w", filepath.Base(exe), err)
	case captured.Code != 0:
		stderr := []rune(strings.TrimSpace(captured.Stderr))
		return "", fmt.Errorf("`%s version` failed with exit code %d: %s", filepath.Base(exe), captured.Code, string(stderr[:min(len(stderr), 200)]))
	}
	return exe, nil
}

// The program found or where to install it, and the image instances start from
func setup(cfg *config.Config) []string {
	line := "Neither apptainer nor singularity is on your PATH. Install Apptainer from " + installURL + "."
	if exe := program(); exe != "" {
		line = "Found " + filepath.Base(exe) + "."
	}
	return []string{line, "Sandboxes start from the image " + configured(cfg) + ". Change it with Apptainer image in /settings under Terminal › " +
		"Singularity/Apptainer."}
}

// The program found, or where to install it
func doctor(*config.Config) []terminal.Check {
	if exe := program(); exe != "" {
		return []terminal.Check{{OK: true, Label: "Found " + filepath.Base(exe) + "."}}
	}
	return []terminal.Check{{Label: "Neither apptainer nor singularity is on your PATH.", Detail: "Install Apptainer from " + installURL + "."}}
}

// Finds the program and builds the .sif image instances start from
func (r *registrant) prepare(ctx context.Context, cfg *config.Config, say func(line string)) error {
	for _, line := range setup(cfg) {
		say(line)
	}
	exe, err := available(ctx)
	if err != nil {
		return err
	}
	image, err := r.sif(ctx, cfg, reference(configured(cfg)), exe, say)
	if err != nil {
		return err
	}
	say("The sandbox image " + image + " is ready.")
	return nil
}

// Removes what instances of this run left in their scratch folders, persistent overlays included, as icebath stops
func (r *registrant) Sweep() {
	r.mu.Lock()
	scratches := r.scratches
	r.mu.Unlock()
	for _, scratch := range scratches {
		for _, p := range entries(scratch) {
			os.RemoveAll(p)
			slog.Info(fmt.Sprintf("Removed orphaned: %s", p))
		}
	}
}
