package terminal

import (
	"cmp"
	"context"
	"errors"
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
		About:     "Persistent cloud development environment",
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

const missing = "the 'daytona' backend is missing its API key or SDK"

func check(cfg *config.Config) error {
	if apiKey(cfg) == "" {
		return errors.New(missing + "; set the Daytona API key in /backends")
	}
	_, err := terminal.SandboxResources(cfg)
	return err
}

func setup(cfg *config.Config) []string {
	lines := []string{
		"Persistent cloud development environments.",
		"Each session gets a dedicated sandbox with filesystem persistence.",
		"Sign up at: https://daytona.io and set the Daytona API key in the Daytona settings (or DAYTONA_API_KEY).",
		"Daytona disks are capped at 10 GB, whatever the sandbox disk setting asks.",
		"Readying the backend checks the API key with Daytona.",
	}
	if cfg.GetBool(terminalconfig.ContainerPersistent) {
		return append(lines, "Sandbox files are kept: a session's sandbox is stopped when it goes and started again when it comes back.")
	}
	return append(lines, "Sandbox files are not kept: a session's sandbox is deleted when it goes.")
}

func doctor(cfg *config.Config) []terminal.Check {
	rows := []terminal.Check{{OK: false, Label: "Daytona API key missing", Detail: "(needed for the 'daytona' terminal backend)"}}
	if apiKey(cfg) != "" {
		rows = []terminal.Check{{OK: true, Label: "Daytona API key", Detail: "(configured)"}}
	}
	return append(rows, terminal.Check{OK: true, Label: "daytona SDK", Detail: "(installed)"})
}

// Checks the key with Daytona by listing a sandbox
func prepare(ctx context.Context, cfg *config.Config, say func(string)) error {
	if err := check(cfg); err != nil {
		return err
	}
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close(context.Background())
	say("Checking the Daytona API key...")
	limit := 1
	sandboxes := client.List(ctx, &daytona.ListSandboxesQuery{Limit: &limit})
	sandboxes.Next()
	if err := sandboxes.Err(); err != nil {
		return err
	}
	say("Daytona accepted the key: sandboxes can be made.")
	return nil
}
