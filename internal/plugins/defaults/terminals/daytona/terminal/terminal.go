package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/daytona/clients/sdk-go/pkg/daytona"
	"github.com/daytona/clients/sdk-go/pkg/types"

	"github.com/nickheyer/icebath/internal/config"
	daytonaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/daytona/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
)

type registrant struct {
	skills *skills.Registry
}

func Register(registry *terminal.Registry, skillset *skills.Registry) {
	registry.Register(registrant{skills: skillset})
}

func (r registrant) Backends() []terminal.Backend {
	return []terminal.Backend{{
		Name:      "daytona",
		Title:     "Daytona",
		About:     "A cloud sandbox on your Daytona account",
		Where:     "a Daytona workspace (Linux)",
		Remote:    true,
		Container: true,
		Isolated:  true,
		Home:      "~/.icebath",
		Secrets:   []string{"DAYTONA_API_KEY"},
		Check:     check,
		Doctor:    doctor,
		Prepare:   prepare,
		Readies:   "Checks the API key with Daytona.",
		Create: func(ctx context.Context, spec *terminal.Spec) (terminal.Environment, error) {
			return create(ctx, spec, r.skills)
		},
	}}
}

func apiKey(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(daytonaconfig.APIKey))
}

// Daytona client of the profile's key, address and region
func newClient(cfg *config.Config) (*daytona.Client, error) {
	return daytona.NewClientWithConfig(&types.DaytonaConfig{
		APIKey: apiKey(cfg),
		APIUrl: strings.TrimSpace(cfg.GetString(daytonaconfig.APIURL)),
		Target: strings.TrimSpace(cfg.GetString(daytonaconfig.Target)),
	})
}

// Image a sandbox is made from: icebath's own sandbox as a Dockerfile Daytona builds, any other name as it is
func image(cfg *config.Config) any {
	name := cmp.Or(strings.TrimSpace(cfg.GetString(daytonaconfig.Image)), sandbox.Image)
	if sandbox.Default(name) {
		return daytona.FromDockerfile(sandbox.Dockerfile)
	}
	return name
}

// Why a sandbox can't be made, as the agent is told it
const missing = "the 'daytona' backend is missing its API key or SDK"

func check(cfg *config.Config) error {
	if apiKey(cfg) == "" {
		return errors.New("No Daytona API key is set. Set API key on the Daytona page of /sandbox, or export DAYTONA_API_KEY.")
	}
	_, err := terminal.SandboxResources(cfg)
	return err
}

// The API key, the size of a disk, and whether sandbox files are kept
func doctor(cfg *config.Config) []terminal.Check {
	checks := []terminal.Check{{OK: true, Label: "Daytona account", State: "Ready", Detail: "Each session gets a sandbox of its own on your Daytona account."}}
	if apiKey(cfg) == "" {
		checks[0] = terminal.Check{Label: "Daytona account", State: "No API key", Detail: "No Daytona API key is set.",
			Fix: "Sign up at daytona.io and set API key, or export DAYTONA_API_KEY.", Setting: daytonaconfig.APIKey}
	}
	checks = append(checks, terminal.Check{OK: true, Label: "Disk", State: "Up to 10 GB", Detail: "Daytona disks hold at most 10 GB, whatever " +
		"Sandbox disk asks for."})
	if cfg.GetBool(terminalconfig.ContainerPersistent) {
		return append(checks, terminal.Check{OK: true, Label: "Sandbox files", State: "Kept", Detail: "A session's sandbox stops when the " +
			"session ends and starts again when the session comes back."})
	}
	return append(checks, terminal.Check{OK: true, Label: "Sandbox files", State: "Not kept", Detail: "A session's sandbox is deleted when the " +
		"session ends."})
}

// Checks the key with Daytona by listing a sandbox
func prepare(ctx context.Context, cfg *config.Config, say func(string)) error {
	if err := check(cfg); err != nil {
		return err
	}
	client, err := newClient(cfg)
	if err != nil {
		return fmt.Errorf("Could not set up the Daytona client: %w", err)
	}
	defer client.Close(context.Background())
	say("Checking the Daytona API key…")
	limit := 1
	sandboxes := client.List(ctx, &daytona.ListSandboxesQuery{Limit: &limit})
	sandboxes.Next()
	if err := sandboxes.Err(); err != nil {
		return fmt.Errorf("Daytona did not accept the API key: %w", err)
	}
	say("Daytona accepted the API key, so sandboxes can be made there.")
	return nil
}
