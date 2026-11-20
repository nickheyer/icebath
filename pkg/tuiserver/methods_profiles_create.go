package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func init() {
	addLongHandlers("profiles.create")
}

// Provisions a profile and reads its receipt when asked, a raised failure coming back as the error beside whatever was made
func provisionProfile(ctx context.Context, req manage.ProfileCreateRequest, report bool) (result *manage.ProvisionResult, receipt *tuigateway.ProfileCreateReceipt, err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = errors.New(failureText(failure))
		}
	}()
	if result, err = manage.ProvisionProfile(ctx, req); err != nil || !report {
		return result, nil, err
	}
	made := manage.ReceiptOf(ctx, req, result)
	return result, &made, nil
}

// Creates a profile in the profile the connection runs in, a refused request answering 4062 with the kind and text a client raises again
func profilesCreate(ctx context.Context, rid any, params map[string]any) map[string]any {
	name := strings.TrimSpace(relayString(params["name"]))
	if name == "" {
		_, data := manage.Refusal(&profiles.ValueError{Msg: "profile name cannot be empty"})
		return rpcErr(rid, 4061, "name required", data)
	}
	flag := func(key string, unset bool) bool {
		if value := params[key]; value != nil {
			return isTruthyValue(value)
		}
		return unset
	}
	cloneAll := flag("clone_all", false)
	req := manage.ProfileCreateRequest{Name: name, CloneAll: cloneAll, NoSkills: flag("no_skills", false),
		CloneChannels: flag("clone_channels", false), SyncImports: flag("sync_imports", false), NoAlias: flag("no_alias", false),
		NoCredentials: !flag("mirror_credentials", true), IsolatedAuth: !flag("share_auth", true),
		Model: strings.TrimSpace(relayString(params["model"])), Provider: strings.TrimSpace(relayString(params["provider"]))}
	if cloneFrom := strings.TrimSpace(relayString(params["clone_from"])); cloneFrom != "" {
		req.CloneFrom = &cloneFrom
	}
	req.CloneConfig = flag("clone_config", false) || (req.CloneFrom != nil && !cloneAll)
	if description := strings.TrimSpace(relayString(params["description"])); description != "" {
		req.Description = &description
	}
	if soul, isText := params["soul"].(string); isText && strings.TrimSpace(soul) != "" {
		req.Soul = &soul
	}
	result, receipt, err := provisionProfile(ctx, req, flag("report", false))
	if err != nil {
		message, data := manage.Refusal(err)
		if result == nil && manage.RefusalKind(err) != "" {
			return rpcErr(rid, 4062, message, data)
		}
		pkgLog().Warn(fmt.Sprintf("profiles.create '%s' failed", name), "error", err)
		return rpcErr(rid, 5062, message, data)
	}
	return ok(rid, tuigateway.Plain(tuigateway.ProfilesCreateResult{Ok: true, Name: name, Path: result.Path, SoulWritten: req.Soul != nil,
		ModelSet: result.ModelSet, Mirrored: result.Mirrored, Warnings: result.Warnings, Receipt: receipt}))
}
