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
		About:     "HPC-friendly rootless container (Apptainer or Singularity).",
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
		return fmt.Errorf("neither 'apptainer' nor 'singularity' is on PATH; install Apptainer: %s", installURL)
	}
	argv := []string{exe, "--version"}
	captured, err := terminal.Capture(context.Background(), argv, 5*time.Second, nil)
	if err == nil && captured.Code != 0 {
		err = fmt.Errorf("exited with code %d", captured.Code)
	}
	if err != nil {
		if errors.Is(err, terminal.ErrTimedOut) {
			err = fmt.Errorf("Command '%s' timed out after 5 seconds", terminal.Join(argv))
		}
		text := fmt.Sprintf("Apptainer/Singularity is installed but `%s --version` failed (%s), so the install looks broken", filepath.Base(exe), err)
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
		return "", &terminal.MissingError{Reason: "Neither 'apptainer' nor 'singularity' was found in PATH. Install Apptainer (" + installURL + ") or " +
			"Singularity and ensure the CLI is available."}
	}
	captured, err := terminal.Capture(ctx, []string{exe, "version"}, 10*time.Second, nil)
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		return "", fmt.Errorf("'%s version' timed out.", filepath.Base(exe))
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("Singularity backend selected but '%s' could not be executed.", filepath.Base(exe))
	case err != nil:
		return "", err
	case captured.Code != 0:
		stderr := []rune(strings.TrimSpace(captured.Stderr))
		return "", fmt.Errorf("'%s version' failed (exit code %d): %s", filepath.Base(exe), captured.Code, string(stderr[:min(len(stderr), 200)]))
	}
	return exe, nil
}

// The program found or where to install it, and the image instances start from
func setup(cfg *config.Config) []string {
	line := "Singularity/Apptainer not found in PATH! Install: " + installURL
	if exe := program(); exe != "" {
		line = "Found " + filepath.Base(exe) + "."
	}
	return []string{line, "Image: " + configured(cfg) + " (change it with Apptainer image in /settings under Terminal → Singularity/Apptainer)."}
}

// The program found, or where to install it
func doctor(*config.Config) []terminal.Check {
	if exe := program(); exe != "" {
		return []terminal.Check{{OK: true, Label: "Singularity/Apptainer", Detail: "(" + filepath.Base(exe) + " found)"}}
	}
	return []terminal.Check{{Label: "Singularity/Apptainer", Detail: "(not found on PATH; install: " + installURL + ")"}}
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
	say("Sandbox image " + image + " ready")
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
