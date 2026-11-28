package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
		About:     "A cloud sandbox on your Modal account",
		Where:     "a Modal sandbox (Linux)",
		Remote:    true,
		Container: true,
		Isolated:  true,
		Home:      terminal.SandboxProfile,
		Secrets:   []string{"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"},
		Check:     check,
		Doctor:    doctor,
		Prepare:   prepare,
		Readies:   "Builds the sandbox image on Modal, so the first command waits for no build.",
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

// Why a sandbox can't be made, as the agent is told it
const unavailable = "Modal backend selected but no direct Modal credentials/config was found."

func check(cfg *config.Config) error {
	if !credentialed(cfg) {
		return errors.New("No Modal token is set and ~/.modal.toml holds none. Set Token ID and Token secret on the Modal page of /sandbox, or " +
			"choose another place to run commands there.")
	}
	_, err := terminal.SandboxResources(cfg)
	return err
}

// The token Modal is reached with, and whether sandbox files are kept
func doctor(cfg *config.Config) []terminal.Check {
	checks := []terminal.Check{{OK: true, Label: "Modal account", State: "Ready", Detail: "Sandboxes run on your own Modal account."}}
	if id, secret := token(cfg); id == "" || secret == "" {
		checks[0].Detail = "Sandboxes run on the Modal account of ~/.modal.toml."
		if !credentialed(cfg) {
			checks[0] = terminal.Check{Label: "Modal account", State: "No token", Detail: "No Modal token is set, and ~/.modal.toml holds none.",
				Fix:     "Get a token ID and secret at modal.com/settings and set Token ID and Token secret, or sign in with the modal program.",
				Setting: modalconfig.TokenID}
		}
	}
	if cfg.GetBool(terminalconfig.ContainerPersistent) {
		return append(checks, terminal.Check{OK: true, Label: "Sandbox files", State: "Kept", Detail: "A session's files are saved when its " +
			"sandbox stops and brought back when the session returns."})
	}
	return append(checks, terminal.Check{OK: true, Label: "Sandbox files", State: "Not kept", Detail: "Each session gets a container of its " +
		"own, and its files go with it."})
}

// Builds the sandbox image on Modal, so the first command waits for no build
func prepare(ctx context.Context, cfg *config.Config, say func(string)) error {
	if err := check(cfg); err != nil {
		return err
	}
	client, err := newClient(cfg)
	if err != nil {
		return fmt.Errorf("Could not set up the Modal client: %w", err)
	}
	defer client.Close()
	say("Finding the " + app + " app on Modal…")
	application, err := client.Apps.FromName(ctx, app, &modal.AppFromNameParams{CreateIfMissing: true})
	if err != nil {
		return fmt.Errorf("Modal did not open the %s app: %w", app, err)
	}
	spec := image(cfg)
	resolved, err := resolveImage(ctx, client, spec)
	if err != nil {
		return fmt.Errorf("Modal could not find the image %s: %w", spec, err)
	}
	say("Building the sandbox image " + spec + " on Modal. The first build takes a while…")
	built, err := resolved.Build(ctx, application, nil)
	if err != nil {
		return fmt.Errorf("Modal could not build the image %s: %w", spec, err)
	}
	say("The sandbox image is ready on Modal as " + built.ImageID + ".")
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
