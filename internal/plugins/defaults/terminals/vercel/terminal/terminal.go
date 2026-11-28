package terminal

import (
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
	"github.com/nickheyer/icebath/pkg/strs"
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
		About:     "A cloud sandbox on your Vercel account",
		Where:     "a Vercel sandbox (Linux)",
		Remote:    true,
		Container: true,
		Isolated:  true,
		Home:      "~/.icebath",
		Cwd:       workspace,
		Secrets:   []string{"VERCEL_OIDC_TOKEN", "VERCEL_TOKEN", "VERCEL_PROJECT_ID", "VERCEL_TEAM_ID"},
		Check:     check,
		Doctor:    doctor,
		Prepare:   prepare,
		Readies:   "Fills in Project ID and Team ID from the nearest .vercel/project.json when they are not set, then starts and stops a test sandbox.",
		Create: func(ctx context.Context, spec *terminal.Spec) (terminal.Environment, error) {
			return create(ctx, spec, r.skills)
		},
	}}
}

func setting(cfg *config.Config, key string) string {
	return strings.TrimSpace(cfg.GetString(key))
}

// Settings of the access token, project and team, in the order they are asked for
var tuple = []struct{ key, env, title string }{
	{vercelconfig.Token, "VERCEL_TOKEN", "Access token"}, {vercelconfig.ProjectID, "VERCEL_PROJECT_ID", "Project ID"},
	{vercelconfig.TeamID, "VERCEL_TEAM_ID", "Team ID"},
}

// Where the Vercel settings are, as a sentence names them
const where = "on the Vercel Sandbox page of /sandbox"

// Credentials from the OIDC token's claims, else the access token with its project and team
func credentialsOf(cfg *config.Config) (credentials, error) {
	if oidc := setting(cfg, vercelconfig.OIDCToken); oidc != "" {
		parts := strings.Split(oidc, ".")
		if len(parts) < 2 {
			return credentials{}, errors.New("The Vercel OIDC token is not valid, because it is not a JWT. Copy it again into OIDC token " + where + ".")
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil {
			return credentials{}, fmt.Errorf("The Vercel OIDC token is not valid, because its claims could not be read: %v. Copy it again into OIDC token %s.",
				err, where)
		}
		var claims struct {
			ProjectID string `json:"project_id"`
			OwnerID   string `json:"owner_id"`
		}
		if err := json.Unmarshal(raw, &claims); err != nil || claims.ProjectID == "" || claims.OwnerID == "" {
			return credentials{}, errors.New("The Vercel OIDC token is not valid, because it names no project_id or owner_id. Copy it again into OIDC " +
				"token " + where + ".")
		}
		return credentials{token: oidc, team: claims.OwnerID, project: claims.ProjectID}, nil
	}
	c := credentials{token: setting(cfg, vercelconfig.Token), project: setting(cfg, vercelconfig.ProjectID), team: setting(cfg, vercelconfig.TeamID)}
	if c.token == "" || c.project == "" || c.team == "" {
		return credentials{}, errors.New("Vercel Sandbox cannot sign in. Set Access token, Project ID and Team ID together " + where + ", or OIDC " +
			"token for one-off local development.")
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
			present = append(present, field.title)
		} else {
			missing = append(missing, field.title)
		}
	}
	if setting(cfg, vercelconfig.OIDCToken) != "" {
		details := []string{"OIDC tokens are for development only. Use an access token for deployments and work that runs a long time."}
		if len(present) > 0 {
			details = append(details, strs.And(present)+" are set too, and the OIDC token wins over them.")
		}
		return auth{ok: true, label: "Signed in with the OIDC token.", details: details}
	}
	if len(missing) == 0 {
		return auth{ok: true, label: "Signed in with the access token, project ID and team ID."}
	}
	if len(present) > 0 {
		return auth{label: "Sign-in is incomplete.", details: []string{"Set: " + strs.And(present) + ".", "Not set: " + strs.And(missing) + ".",
			"Set Access token, Project ID and Team ID together " + where + "."}}
	}
	return auth{label: "Sign-in is not set up.", details: []string{"Set Access token, Project ID and Team ID together " + where + ".",
		"For development only, OIDC token works instead."}}
}

func check(cfg *config.Config) error {
	if runtime := setting(cfg, vercelconfig.Runtime); runtime != "" && !slices.Contains(vercelconfig.Runtimes, runtime) {
		return fmt.Errorf("Vercel Sandbox has no runtime %s. Set Runtime %s to %s, or clear it.", strconv.Quote(runtime), where,
			strings.Join(vercelconfig.Runtimes, ", "))
	}
	resources, err := terminal.SandboxResources(cfg)
	if err != nil {
		return err
	}
	if resources.Disk != 0 && resources.Disk != defaultDisk {
		return fmt.Errorf("Vercel Sandbox does not take a custom disk size, and Sandbox disk, under Advanced in /sandbox, is %s. Set it back to %s.",
			strs.Counted(float64(resources.Disk), "MB"), strs.Counted(defaultDisk, "MB"))
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
	if present > 0 {
		return errors.New("Vercel Sandbox cannot sign in, because Access token, Project ID and Team ID must all be set together " + where + ". An " +
			"OIDC token works only for one-off local development.")
	}
	return errors.New("Vercel Sandbox cannot sign in. Set Access token, Project ID and Team ID " + where + ", since an OIDC token works only for " +
		"one-off local development.")
}

func describeLinked(linked map[string]string) string {
	var parts []string
	if id := linked["projectId"]; id != "" {
		parts = append(parts, "the project ID "+id)
	}
	if id := linked["orgId"]; id != "" {
		parts = append(parts, "the team ID "+id)
	}
	return strs.And(parts)
}

// The image or runtime, the disk, how Vercel is signed in to, the link file of the folder and whether sandbox files are kept
func doctor(cfg *config.Config) []terminal.Check {
	var checks []terminal.Check
	if runtime := setting(cfg, vercelconfig.Runtime); runtime != "" && !slices.Contains(vercelconfig.Runtimes, runtime) {
		checks = append(checks, terminal.Check{Label: "Runtime name", State: "Not known", Detail: "Vercel Sandbox has no runtime " +
			strconv.Quote(runtime) + ".", Fix: "Set Runtime to " + strings.Join(vercelconfig.Runtimes, ", ") + ", or clear it.", Setting: vercelconfig.Runtime})
	}
	if resources, err := terminal.SandboxResources(cfg); err == nil && (resources.Disk == 0 || resources.Disk == defaultDisk) {
		checks = append(checks, terminal.Check{OK: true, Label: "Disk", State: "Vercel's size"})
	} else {
		checks = append(checks, terminal.Check{Label: "Disk", State: "Not allowed", Detail: "Vercel Sandbox does not take a custom disk size.",
			Fix: "Set Sandbox disk, under Advanced in Sandbox, back to " + strs.Counted(defaultDisk, "MB") + "."})
	}
	a := describe(cfg)
	signIn := terminal.Check{OK: a.ok, Label: "Sign-in", State: "Signed in", Detail: strings.Join(append([]string{a.label}, a.details...), " ")}
	if !a.ok {
		signIn.State, signIn.Fix, signIn.Setting = "Not signed in", "Set Access token, Project ID and Team ID together, or export VERCEL_TOKEN, "+
			"VERCEL_PROJECT_ID and VERCEL_TEAM_ID. For one-off local development, OIDC token works instead.", vercelconfig.Token
	}
	checks = append(checks, signIn)
	if linked, err := linkedProject(); err == nil && len(linked) > 0 {
		checks = append(checks, terminal.Check{OK: true, Label: "Linked project", State: "Found", Detail: "The nearest .vercel/project.json names " +
			describeLinked(linked) + "."})
	}
	if cfg.GetBool(terminalconfig.ContainerPersistent) {
		return append(checks, terminal.Check{OK: true, Label: "Sandbox files", State: "Kept", Detail: "A kept sandbox brings back its files only. " +
			"Programs that were running do not carry on after the sandbox is made again."})
	}
	return append(checks, terminal.Check{OK: true, Label: "Sandbox files", State: "Not kept", Detail: "They go when the sandbox does."})
}

// Fills the project and team IDs the nearest .vercel/project.json links when unset, then starts a sandbox and stops it
func prepare(ctx context.Context, cfg *config.Config, say func(string)) error {
	if setting(cfg, vercelconfig.OIDCToken) == "" {
		linked, err := linkedProject()
		if err != nil {
			return fmt.Errorf("The working folder could not be read to find .vercel/project.json: %w", err)
		}
		if len(linked) > 0 {
			say("Found " + describeLinked(linked) + " in the nearest .vercel/project.json.")
		}
		for _, field := range []struct{ key, linked, title string }{{vercelconfig.ProjectID, "projectId", "Project ID"}, {vercelconfig.TeamID, "orgId", "Team ID"}} {
			if id := linked[field.linked]; id != "" && setting(cfg, field.key) == "" {
				if err := cfg.Set(field.key, id); err != nil {
					return err
				}
				say("Set " + field.title + " to " + id + ".")
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
	say("Starting a test sandbox…")
	s, err := retry("sandbox create", createAttempts, func() (session, error) { return c.create(ctx, params, "") })
	if err != nil {
		return fmt.Errorf("Vercel did not start a test sandbox: %w", err)
	}
	defer stop(c, s)
	if err := waitRunning(ctx, c, &s); err != nil {
		return err
	}
	say("Vercel Sandbox works. The test sandbox started and is being stopped.")
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
