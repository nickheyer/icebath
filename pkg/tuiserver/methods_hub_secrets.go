package tuiserver

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var hubSecretsRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubSecretsRegistry, tuigateway.SecretsState.Name(), 5196, rpcShape{}, secretsState)
	scopedRPC(hubSecretsRegistry, tuigateway.SecretsProjects.Name(), 5196, rpcShape{}, secretsProjects)
	scopedRPC(hubSecretsRegistry, tuigateway.SecretsChange.Name(), 5196, rpcShape{}, secretsChange)
	addLongHandlers(tuigateway.SecretsProjects.Name(), tuigateway.SecretsChange.Name())
	hubSecretsRegistry.install()
}

// What a refused Bitwarden token usually means, by the cause the store gave
var secretsCauses = map[string]string{
	"region": " This usually means the token is for another Bitwarden region; pick the EU cloud or your own server.",
	"token":  " This usually means the access token is wrong or was revoked.",
}

// Answer for a failed secrets request: a refusal goes back as its message with what it usually means, anything else as the failure it is
func secretsFailed(rid any, err error) (map[string]any, error) {
	var refused *provider.SecretSourceRefusal
	if errors.As(err, &refused) {
		return rpcErr(rid, 4196, refused.Message+secretsCauses[refused.Cause], nil), nil
	}
	return nil, err
}

// Both secret stores of the profile as the secret stores screen shows them
func secretsStateOf(ctx context.Context) (tuigateway.SecretsStateResult, error) {
	bitwarden, onePassword, err := provider.SecretSourcesState(home.Dir(ctx))
	if err != nil {
		return tuigateway.SecretsStateResult{}, err
	}
	out := tuigateway.SecretsStateResult{
		Bitwarden: tuigateway.SecretsBitwarden{Enabled: bitwarden.Enabled, TokenEnv: bitwarden.TokenEnv, TokenSet: bitwarden.TokenSet, ProjectID: bitwarden.ProjectID,
			ServerURL: bitwarden.ServerURL, Binary: bitwarden.Binary, Version: bitwarden.Version},
		OnePassword: tuigateway.SecretsOnePassword{Enabled: onePassword.Enabled, TokenEnv: onePassword.TokenEnv, TokenSet: onePassword.TokenSet, Account: onePassword.Account,
			Binary: onePassword.Binary, Version: onePassword.Version, References: make([]tuigateway.SecretReference, 0, len(onePassword.References))},
	}
	for _, ref := range onePassword.References {
		out.OnePassword.References = append(out.OnePassword.References, tuigateway.SecretReference{Env: ref[0], Reference: ref[1]})
	}
	return out, nil
}

// Both secret stores of the profile for the secret stores screen
func secretsState(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	if _, err := tuigateway.SecretsState.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	out, err := secretsStateOf(ctx)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Bitwarden projects an access token sees
func secretsProjects(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.SecretsProjects.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	projects, err := provider.BitwardenProjectsFor(ctx, home.Dir(ctx), p.Token, p.ServerURL)
	if err != nil {
		return secretsFailed(rid, err)
	}
	out := tuigateway.SecretsProjectsResult{Projects: make([]tuigateway.SecretsProject, 0, len(projects))}
	for _, project := range projects {
		out.Projects = append(out.Projects, tuigateway.SecretsProject{ID: project.ID, Name: project.Name})
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Puts what a setup or a check found into the answer of a change
func secretsPreviewInto(out *tuigateway.SecretsChangeResult, preview *provider.SecretPreview) {
	out.Warnings = preview.Warnings
	for _, row := range preview.Rows {
		out.Rows = append(out.Rows, tuigateway.SecretsPreviewRow{Name: row.Name, Action: row.Action})
	}
}

// Carries out one change to the Bitwarden store
func secretsChangeBitwarden(ctx context.Context, h string, p tuigateway.SecretsChangeParams, out *tuigateway.SecretsChangeResult) error {
	switch p.Verb {
	case "setup":
		preview, err := provider.SetUpBitwarden(h, p.Token, p.ServerURL, p.ProjectID)
		if err != nil {
			return err
		}
		secretsPreviewInto(out, preview)
	case "token":
		projects, err := provider.RotateBitwardenToken(ctx, h, p.Token)
		if err != nil {
			return err
		}
		out.Projects = int64(projects)
	case "check":
		preview, err := provider.PreviewBitwarden(h)
		if err != nil {
			return err
		}
		secretsPreviewInto(out, preview)
	case "off":
		return provider.DisableSecretSource(h, "bitwarden")
	default:
		return &provider.SecretSourceRefusal{Message: "unknown Bitwarden change: " + p.Verb}
	}
	return nil
}

// Carries out one change to the 1Password store
func secretsChangeOnePassword(ctx context.Context, h string, p tuigateway.SecretsChangeParams, out *tuigateway.SecretsChangeResult) error {
	var err error
	switch p.Verb {
	case "setup":
		out.Identity, err = provider.SetUpOnePassword(ctx, h, p.Account, p.TokenEnv, p.Token, p.BinaryPath)
	case "token":
		out.Identity, err = provider.RotateOnePasswordToken(ctx, h, p.Token)
	case "map":
		err = provider.MapOnePasswordReference(h, p.Env, p.Reference)
	case "unmap":
		err = provider.UnmapOnePasswordReference(h, p.Env)
	case "check":
		var preview *provider.SecretPreview
		if preview, err = provider.PreviewOnePassword(h); err == nil {
			secretsPreviewInto(out, preview)
		}
	case "off":
		err = provider.DisableSecretSource(h, "onepassword")
	default:
		err = &provider.SecretSourceRefusal{Message: "unknown 1Password change: " + p.Verb}
	}
	return err
}

// One change to a secret store, answered with the stores after it
func secretsChange(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.SecretsChange.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	var out tuigateway.SecretsChangeResult
	switch p.Source {
	case "bitwarden":
		err = secretsChangeBitwarden(ctx, home.Dir(ctx), p, &out)
	case "onepassword":
		err = secretsChangeOnePassword(ctx, home.Dir(ctx), p, &out)
	default:
		err = &provider.SecretSourceRefusal{Message: "unknown secret store: " + p.Source}
	}
	if err != nil {
		return secretsFailed(rid, err)
	}
	if out.State, err = secretsStateOf(ctx); err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}
