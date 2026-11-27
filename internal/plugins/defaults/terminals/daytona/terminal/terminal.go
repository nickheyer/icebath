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
		Setup:     setup,
		Doctor:    doctor,
		Prepare:   prepare,
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
		return errors.New("No Daytona API key is set. Set API key in /settings under Terminal › Daytona, or export DAYTONA_API_KEY.")
	}
	_, err := terminal.SandboxResources(cfg)
	return err
}

func setup(cfg *config.Config) []string {
	lines := []string{
		"Cloud sandboxes that can keep their files.",
		"Each session gets a sandbox of its own.",
		"Sign up at https://daytona.io, then set API key in /settings under Terminal › Daytona, or export DAYTONA_API_KEY.",
		"Daytona disks hold at most 10 GB, whatever Sandbox disk in /settings under Terminal asks for.",
		"Preparing Daytona on this screen checks the API key with Daytona.",
	}
	if cfg.GetBool(terminalconfig.ContainerPersistent) {
		return append(lines, "Sandbox files are kept. A session's sandbox is stopped when the session ends and started again when it comes back.")
	}
	return append(lines, "Sandbox files are not kept. A session's sandbox is deleted when the session ends.")
}

func doctor(cfg *config.Config) []terminal.Check {
	rows := []terminal.Check{{OK: false, Label: "The Daytona API key is not set.", Detail: "Commands run on Daytona only once it is."}}
	if apiKey(cfg) != "" {
		rows = []terminal.Check{{OK: true, Label: "The Daytona API key is set."}}
	}
	return append(rows, terminal.Check{OK: true, Label: "The Daytona client comes with icebath."})
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
