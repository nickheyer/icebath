package terminal

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	modal "github.com/modal-labs/modal-client/go"

	"github.com/nickheyer/icebath/internal/config"
	modalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/modal/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
)

// App the sandboxes run under on Modal
const app = "icebath-agent"

type registrant struct {
	skills *skills.Registry
}

func Register(registry *terminal.Registry, skillset *skills.Registry) {
	registry.Register(registrant{skills: skillset})
}

func (r registrant) Backends() []terminal.Backend {
	return []terminal.Backend{{
		Name:      "modal",
		Title:     "Modal",
		About:     "Serverless cloud sandbox",
		Where:     "a Modal sandbox (Linux)",
		Remote:    true,
		Container: true,
		Isolated:  true,
		Home:      terminal.SandboxProfile,
		Secrets:   []string{"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"},
		Check:     check,
		Setup:     setup,
		Doctor:    doctor,
		Prepare:   prepare,
		Create: func(ctx context.Context, spec *terminal.Spec) (terminal.Environment, error) {
			return create(ctx, spec, r.skills)
		},
	}}
}

// Token the settings hold, the profile's own or the environment's
func token(cfg *config.Config) (string, string) {
	return strings.TrimSpace(cfg.GetString(modalconfig.TokenID)), strings.TrimSpace(cfg.GetString(modalconfig.TokenSecret))
}

// Whether a Modal token pair is set or ~/.modal.toml holds one
func credentialed(cfg *config.Config) bool {
	if id, secret := token(cfg); id != "" && secret != "" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".modal.toml"))
	return err == nil
}

// Modal client of the profile's token, the one of ~/.modal.toml when none is set
func newClient(cfg *config.Config) (*modal.Client, error) {
	id, secret := token(cfg)
	return modal.NewClientWithOptions(&modal.ClientParams{TokenID: id, TokenSecret: secret})
}

// Image setting, icebath's own sandbox when unset
func image(cfg *config.Config) string {
	return cmp.Or(strings.TrimSpace(cfg.GetString(modalconfig.Image)), sandbox.Image)
}

const unavailable = "Modal backend selected but no direct Modal credentials/config was found."

func check(cfg *config.Config) error {
	if !credentialed(cfg) {
		return errors.New(unavailable + " Configure Modal or pick another place to run commands in /backends.")
	}
	_, err := terminal.SandboxResources(cfg)
	return err
}

func setup(cfg *config.Config) []string {
	lines := []string{
		"Serverless cloud sandboxes. Each session gets its own container.",
		"Runs on your own Modal account: https://modal.com",
		"Set the Modal token ID and secret from https://modal.com/settings in the Modal settings (or MODAL_TOKEN_ID and MODAL_TOKEN_SECRET), or sign in with the modal CLI so ~/.modal.toml holds them.",
		"Readying the backend builds the sandbox image on Modal ahead of the first command.",
	}
	if cfg.GetBool(terminalconfig.ContainerPersistent) {
		lines = append(lines, "Sandbox files are kept: a session's filesystem is snapshotted when its sandbox goes and restored when it comes back.")
	}
	return lines
}

func doctor(cfg *config.Config) []terminal.Check {
	if credentialed(cfg) {
		return []terminal.Check{{OK: true, Label: "Modal credentials", Detail: "(configured)"}}
	}
	return []terminal.Check{{OK: false, Label: "Modal credentials missing", Detail: "(needed for the 'modal' terminal backend)"}}
}

// Builds the sandbox image on Modal, so the first command waits for no build
func prepare(ctx context.Context, cfg *config.Config, say func(string)) error {
	if err := check(cfg); err != nil {
		return err
	}
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	say("Finding the " + app + " app on Modal...")
	application, err := client.Apps.FromName(ctx, app, &modal.AppFromNameParams{CreateIfMissing: true})
	if err != nil {
		return err
	}
	spec := image(cfg)
	resolved, err := resolveImage(ctx, client, spec)
	if err != nil {
		return err
	}
	say("Building the sandbox image " + spec + " on Modal; the first build takes a while...")
	built, err := resolved.Build(ctx, application, nil)
	if err != nil {
		return err
	}
	say("Sandbox image ready: " + built.ImageID)
	return nil
}

// Image of a setting: an im- id loads by id, icebath's own sandbox builds on Modal from its Dockerfile, and a registry reference gets pip
// repaired first, Python too on Debian and Ubuntu
func resolveImage(ctx context.Context, client *modal.Client, spec string) (*modal.Image, error) {
	if strings.HasPrefix(spec, "im-") {
		return client.Images.FromID(ctx, spec, nil)
	}
	if sandbox.Default(spec) {
		base, commands, err := sandbox.Recipe()
		if err != nil {
			return nil, err
		}
		return client.Images.FromRegistry(base, nil).DockerfileCommands(commands, nil), nil
	}
	setup := []string{"RUN rm -rf /usr/local/lib/python*/site-packages/pip* 2>/dev/null; python -m ensurepip --upgrade --default-pip 2>/dev/null || true"}
	if lower := strings.ToLower(spec); strings.Contains(lower, "ubuntu") || strings.Contains(lower, "debian") {
		setup = append([]string{"RUN apt-get update -qq && apt-get install -y -qq python3 python3-venv > /dev/null 2>&1 || true"}, setup...)
	}
	return client.Images.FromRegistry(spec, nil).DockerfileCommands(setup, nil), nil
}
