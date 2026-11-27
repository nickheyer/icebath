package terminal

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	vercelconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/vercel/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// Folder a Vercel sandbox starts in
const workspace = "/vercel/sandbox"

// The only disk size Vercel gives, in MB
const defaultDisk = 51200

// Longest the sandbox readying starts to prove the backend works may live, should stopping it fail
const testLifetime = 5 * time.Minute

type registrant struct {
	skills *skills.Registry
}

func Register(registry *terminal.Registry, skillset *skills.Registry) {
	registry.Register(registrant{skills: skillset})
}

func (r registrant) Backends() []terminal.Backend {
	return []terminal.Backend{{
		Name:      "vercel_sandbox",
		Title:     "Vercel Sandbox",
		About:     "Cloud microVM with snapshot filesystem persistence",
		Where:     "a Vercel sandbox (Linux)",
		Remote:    true,
		Container: true,
		Isolated:  true,
		Home:      "~/.icebath",
		Cwd:       workspace,
		Secrets:   []string{"VERCEL_OIDC_TOKEN", "VERCEL_TOKEN", "VERCEL_PROJECT_ID", "VERCEL_TEAM_ID"},
		Check:     check,
		Setup:     setup,
		Doctor:    doctor,
		Prepare:   prepare,
		Create: func(ctx context.Context, spec *terminal.Spec) (terminal.Environment, error) {
			return create(ctx, spec, r.skills)
		},
	}}
}

func setting(cfg *config.Config, key string) string {
	return strings.TrimSpace(cfg.GetString(key))
}

// Settings of the access token, project and team, in the order they are asked for
var tuple = []struct{ key, env string }{
	{vercelconfig.Token, "VERCEL_TOKEN"}, {vercelconfig.ProjectID, "VERCEL_PROJECT_ID"}, {vercelconfig.TeamID, "VERCEL_TEAM_ID"},
}

// Credentials from the OIDC token's claims, else the access token with its project and team
func credentialsOf(cfg *config.Config) (credentials, error) {
	if oidc := setting(cfg, vercelconfig.OIDCToken); oidc != "" {
		parts := strings.Split(oidc, ".")
		if len(parts) < 2 {
			return credentials{}, errors.New("Invalid Vercel OIDC token: not a JWT")
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil {
			return credentials{}, fmt.Errorf("Invalid Vercel OIDC token: %v", err)
		}
		var claims struct {
			ProjectID string `json:"project_id"`
			OwnerID   string `json:"owner_id"`
		}
		if err := json.Unmarshal(raw, &claims); err != nil || claims.ProjectID == "" || claims.OwnerID == "" {
			return credentials{}, errors.New("Invalid Vercel OIDC token: missing project_id or owner_id claims")
		}
		return credentials{token: oidc, team: claims.OwnerID, project: claims.ProjectID}, nil
	}
	c := credentials{token: setting(cfg, vercelconfig.Token), project: setting(cfg, vercelconfig.ProjectID), team: setting(cfg, vercelconfig.TeamID)}
	if c.token == "" || c.project == "" || c.team == "" {
		return credentials{}, errors.New("Vercel Sandbox auth is not configured: set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID together, or VERCEL_OIDC_TOKEN for local development")
	}
	return c, nil
}

// How the profile signs in to Vercel, without its secrets
type auth struct {
	ok      bool
	label   string
	details []string
}

func describe(cfg *config.Config) auth {
	var present, missing []string
	for _, field := range tuple {
		if setting(cfg, field.key) != "" {
			present = append(present, field.env)
		} else {
			missing = append(missing, field.env)
		}
	}
	if setting(cfg, vercelconfig.OIDCToken) != "" {
		details := []string{"mode: OIDC", "active env: VERCEL_OIDC_TOKEN",
			"note: OIDC tokens are development-only; use access-token auth for deployments and long-running processes"}
		if len(present) > 0 {
			details = append(details, "also present: "+strings.Join(present, ", "))
		}
		return auth{true, "OIDC token via VERCEL_OIDC_TOKEN", details}
	}
	if len(missing) == 0 {
		return auth{true, "access token + project/team via VERCEL_TOKEN, VERCEL_PROJECT_ID, VERCEL_TEAM_ID",
			[]string{"mode: access token", "active env: VERCEL_TOKEN, VERCEL_PROJECT_ID, VERCEL_TEAM_ID"}}
	}
	if len(present) > 0 {
		return auth{false, fmt.Sprintf("partial access-token auth (missing %s)", strings.Join(missing, ", ")),
			[]string{"mode: incomplete access token", "present env: " + strings.Join(present, ", "), "missing env: " + strings.Join(missing, ", "),
				"recommended: set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID together"}}
	}
	return auth{false, "not configured", []string{"recommended: set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID",
		"development-only alternative: set VERCEL_OIDC_TOKEN"}}
}

func check(cfg *config.Config) error {
	if runtime := setting(cfg, vercelconfig.Runtime); runtime != "" && !slices.Contains(vercelconfig.Runtimes, runtime) {
		return fmt.Errorf("Vercel Sandbox runtime %s is not supported. Set %s to one of: %s.", strconv.Quote(runtime), vercelconfig.Runtime, strings.Join(vercelconfig.Runtimes, ", "))
	}
	resources, err := terminal.SandboxResources(cfg)
	if err != nil {
		return err
	}
	if resources.Disk != 0 && resources.Disk != defaultDisk {
		return fmt.Errorf("Vercel Sandbox does not support a custom disk size (%s is %d). Reset it to the default of %d MB.", terminalconfig.ContainerDisk, resources.Disk, defaultDisk)
	}
	if setting(cfg, vercelconfig.OIDCToken) != "" {
		_, err := credentialsOf(cfg)
		return err
	}
	present := 0
	for _, field := range tuple {
		if setting(cfg, field.key) != "" {
			present++
		}
	}
	if present == len(tuple) {
		return nil
	}
	head := "selected but no supported auth configuration was found. Set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID for normal use."
	if present > 0 {
		head = "selected with token auth, but VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID must all be set together."
	}
	return fmt.Errorf("Vercel Sandbox backend %s VERCEL_OIDC_TOKEN is supported for one-off local development only.", head)
}

func setup(cfg *config.Config) []string {
	lines := []string{
		"Cloud microVM sandboxes with snapshot-backed filesystem persistence.",
		"Set the Vercel image to a Vercel managed image or a VCR repository[:tag] (default " + vercelconfig.DefaultImage + ").",
		"Snapshots restore files only; live processes do not continue after sandbox recreation.",
		"Use a long-lived Vercel access token plus project/team IDs: set the access token, project ID and team ID together in the Vercel Sandbox settings (VERCEL_TOKEN, VERCEL_PROJECT_ID, VERCEL_TEAM_ID), or the OIDC token (VERCEL_OIDC_TOKEN) for one-off local development.",
		"Readying the backend fills the project and team IDs from the nearest .vercel/project.json when they are unset, then starts and stops a sandbox to prove it works.",
	}
	if runtime := setting(cfg, vercelconfig.Runtime); runtime != "" {
		lines = append(lines, fmt.Sprintf("%s=%s is deprecated by Vercel and overrides the image; unset it to use the image.", vercelconfig.Runtime, strconv.Quote(runtime)))
	}
	if resources, err := terminal.SandboxResources(cfg); err == nil && resources.Disk != 0 && resources.Disk != defaultDisk {
		lines = append(lines, fmt.Sprintf("Vercel Sandbox does not support custom disk sizing; reset %s to %d.", terminalconfig.ContainerDisk, defaultDisk))
	}
	if linked, err := linkedProject(); err == nil && len(linked) > 0 {
		lines = append(lines, "Found defaults in nearest .vercel/project.json: "+describeLinked(linked)+".")
	}
	return lines
}

func describeLinked(linked map[string]string) string {
	var parts []string
	if id := linked["projectId"]; id != "" {
		parts = append(parts, "project ID "+id)
	}
	if id := linked["orgId"]; id != "" {
		parts = append(parts, "team ID "+id)
	}
	return strings.Join(parts, ", ")
}

func doctor(cfg *config.Config) []terminal.Check {
	var rows []terminal.Check
	supported := strings.Join(vercelconfig.Runtimes, ", ")
	switch runtime := setting(cfg, vercelconfig.Runtime); {
	case runtime == "":
		rows = append(rows, terminal.Check{OK: true, Label: "Vercel image", Detail: "(" + cmp.Or(setting(cfg, vercelconfig.Image), vercelconfig.DefaultImage) + ")"})
	case slices.Contains(vercelconfig.Runtimes, runtime):
		rows = append(rows, terminal.Check{OK: true, Label: "Vercel runtime", Detail: "(" + runtime + ")"})
	default:
		rows = append(rows, terminal.Check{OK: false, Label: "Vercel runtime unsupported", Detail: "(" + runtime + "; use " + supported + ")"})
	}
	if resources, err := terminal.SandboxResources(cfg); err == nil && (resources.Disk == 0 || resources.Disk == defaultDisk) {
		rows = append(rows, terminal.Check{OK: true, Label: "Vercel disk setting", Detail: "(uses platform default)"})
	} else {
		rows = append(rows, terminal.Check{OK: false, Label: "Vercel custom disk unsupported", Detail: fmt.Sprintf("(reset %s to %d)", terminalconfig.ContainerDisk, defaultDisk)})
	}
	a := describe(cfg)
	switch {
	case a.ok:
		rows = append(rows, terminal.Check{OK: true, Label: "Vercel auth", Detail: "(" + a.label + ")"})
	case strings.HasPrefix(a.label, "partial"):
		rows = append(rows, terminal.Check{OK: false, Label: "Vercel auth incomplete", Detail: "(" + a.label + ")"})
	default:
		rows = append(rows, terminal.Check{OK: false, Label: "Vercel auth not configured", Detail: "(" + a.label + ")"})
	}
	for _, line := range a.details {
		rows = append(rows, terminal.Check{OK: true, Label: "Vercel auth " + line})
	}
	if cfg.GetBool(terminalconfig.ContainerPersistent) {
		return append(rows, terminal.Check{OK: true, Label: "Vercel persistence", Detail: "snapshot filesystem only; live processes do not survive sandbox recreation"})
	}
	return append(rows, terminal.Check{OK: true, Label: "Vercel persistence", Detail: "ephemeral filesystem"})
}

// Fills the project and team IDs the nearest .vercel/project.json links when unset, then starts a sandbox and stops it
func prepare(ctx context.Context, cfg *config.Config, say func(string)) error {
	if setting(cfg, vercelconfig.OIDCToken) == "" {
		linked, err := linkedProject()
		if err != nil {
			return err
		}
		if len(linked) > 0 {
			say("Found defaults in nearest .vercel/project.json.")
		}
		for _, field := range []struct{ key, linked, title string }{{vercelconfig.ProjectID, "projectId", "project ID"}, {vercelconfig.TeamID, "orgId", "team ID"}} {
			if id := linked[field.linked]; id != "" && setting(cfg, field.key) == "" {
				if err := cfg.Set(field.key, id); err != nil {
					return err
				}
				say("Set the Vercel " + field.title + " to " + id + ".")
			}
		}
	}
	if err := check(cfg); err != nil {
		return err
	}
	creds, err := credentialsOf(cfg)
	if err != nil {
		return err
	}
	c := &client{credentials: creds, http: newHTTP()}
	params := paramsOf(cfg, 0)
	params.timeout = testLifetime
	say("Starting a test sandbox...")
	s, err := retry("sandbox create", createAttempts, func() (session, error) { return c.create(ctx, params, "") })
	if err != nil {
		return err
	}
	defer stop(c, s)
	if err := waitRunning(ctx, c, &s); err != nil {
		return err
	}
	say("Vercel Sandbox works: a sandbox started, and is being stopped.")
	return nil
}

// Non-blank projectId and orgId of the nearest .vercel/project.json at or above the working folder
func linkedProject() (map[string]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	current, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(current); err == nil {
		current = resolved
	}
	for dir := current; ; dir = filepath.Dir(dir) {
		project := filepath.Join(dir, ".vercel", "project.json")
		if _, err := os.Stat(project); err == nil {
			return projectIDs(project), nil
		}
		if filepath.Dir(dir) == dir {
			return map[string]string{}, nil
		}
	}
}

// Ids a Vercel link file holds, none when it can't be read or is no JSON object
func projectIDs(project string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(project)
	if err != nil {
		return out
	}
	var fields map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &fields) != nil {
		return out
	}
	for _, key := range []string{"projectId", "orgId"} {
		if id, ok := fields[key].(string); ok && strings.TrimSpace(id) != "" {
			out[key] = id
		}
	}
	return out
}
