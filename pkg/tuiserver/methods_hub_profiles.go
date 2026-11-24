package tuiserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Longest an automatic description may take the auxiliary model
const profileDescribeTimeout = 90 * time.Second

var profilesHubRegistry = &handlerRegistry{}

func init() {
	guardedRPC(profilesHubRegistry, tuigateway.ProfilesOverview.Name(), 5065, rpcShape{}, profilesOverview)
	guardedRPC(profilesHubRegistry, tuigateway.ProfilesDescribe.Name(), 5066, rpcShape{}, profilesDescribe)
	guardedRPC(profilesHubRegistry, tuigateway.ProfilesAlias.Name(), 5067, rpcShape{}, profilesAlias)
	addLongHandlers(tuigateway.ProfilesOverview.Name(), tuigateway.ProfilesDescribe.Name())
	profilesHubRegistry.install()
}

// Text of a listing value that may be nil, empty for nil
func profileText(v any) string {
	if v == nil {
		return ""
	}
	return anyutil.String(v)
}

// Every profile of the install with the sticky default marked, as the profiles screen shows it
func profilesOverview(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	if _, err := tuigateway.ProfilesOverview.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	infos, err := manage.ListProfiles(ctx, true)
	if err != nil {
		return nil, err
	}
	active := profiles.GetActiveProfile(home.Root())
	out := tuigateway.ProfilesOverviewResult{Active: active, Profiles: make([]tuigateway.ProfileOverviewRow, 0, len(infos))}
	for _, info := range infos {
		dist := profileText(info.DistributionName)
		if version := profileText(info.DistributionVersion); dist != "" && version != "" {
			dist += " " + version
		}
		out.Profiles = append(out.Profiles, tuigateway.ProfileOverviewRow{Name: info.Name, Path: info.Path, IsDefault: info.IsDefault,
			Active: info.Name == active, DisplayName: info.DisplayName, Model: profileText(info.Model), Provider: profileText(info.Provider),
			Description: info.Description, DescriptionAuto: info.DescriptionAuto, SkillCount: int64(info.SkillCount),
			Alias: info.AliasName, AliasPath: info.AliasPath, Distribution: dist, DistributionSource: profileText(info.DistributionSource),
			PreviousNames: info.PreviousNames, ServiceRunning: info.GatewayRunning, HasEnv: info.HasEnv})
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Sets a profile's description from the text, or has the auxiliary model write one
func profilesDescribe(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ProfilesDescribe.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	canon, dir, err := profiles.ExistingProfileDir(p.Name)
	if err != nil {
		return rpcErr(rid, 4061, err.Error(), nil), nil
	}
	if p.Auto {
		outcome, err := manage.DescribeProfile(ctx, canon, p.Overwrite, profileDescribeTimeout)
		if err != nil {
			return nil, err
		}
		result := tuigateway.ProfilesDescribeResult{Name: canon, Auto: true, Reason: outcome.Reason}
		if outcome.OK && outcome.Description != nil {
			result.Description = *outcome.Description
		} else {
			result.Description = profiles.ReadProfileMeta(dir).Description
		}
		return ok(rid, tuigateway.Plain(result)), nil
	}
	text := strings.TrimSpace(p.Text)
	auto := false
	if err := profiles.WriteProfileMeta(ctx, dir, profiles.MetaUpdate{Description: &text, DescriptionAuto: &auto}); err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(tuigateway.ProfilesDescribeResult{Name: canon, Description: text})), nil
}

// Writes the shell alias that opens a profile, or removes it
func profilesAlias(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ProfilesAlias.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	canon, _, err := profiles.ExistingProfileDir(p.Name)
	if err != nil {
		return rpcErr(rid, 4061, err.Error(), nil), nil
	}
	if p.Remove {
		path, err := profiles.FindOurWrapper(canon)
		if err != nil {
			return nil, err
		}
		removed, err := profiles.RemoveWrapperScript(canon)
		if err != nil {
			return nil, err
		}
		return ok(rid, tuigateway.Plain(tuigateway.ProfilesAliasResult{Name: canon, Path: path, Removed: removed})), nil
	}
	wrapper, collision, err := manage.WriteAlias(canon)
	if err != nil {
		return nil, err
	}
	if collision != "" {
		return rpcErr(rid, 4062, fmt.Sprintf("an alias named %s would shadow %s", canon, collision), nil), nil
	}
	return ok(rid, tuigateway.Plain(tuigateway.ProfilesAliasResult{Name: canon, Path: wrapper})), nil
}
