package tools

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// OnboardingProfileBuildFlag is the onboarding.seen key recording that the profile build was offered
const OnboardingProfileBuildFlag = "profile_build_offered"

// The root profile's config.yaml, where the onboarding.run flag lives for every launch profile
func onboardingRootConfigPath() string { return home.ConfigPath(home.Root()) }

// OnboardingShouldRun is the root config's explicit onboarding.run when boolean, else whether this machine never used icebath
func OnboardingShouldRun(ctx context.Context, assumeFresh bool) bool {
	if value, ok := onboardingExplicitRun(); ok {
		return value
	}
	return assumeFresh || InstallStateProbe(ctx).Fresh()
}

// The explicit boolean onboarding.run of the root config with the managed overlay, false when unset
func onboardingExplicitRun() (bool, bool) {
	doc, err := config.LoadUserEffective(onboardingRootConfigPath(), os.LookupEnv)
	if err != nil {
		return false, false
	}
	section, _ := doc["onboarding"].(map[string]any)
	value, ok := section["run"].(bool)
	return value, ok
}

// OnboardingLatchFirstInstall records on a fresh install's first desktop boot that onboarding is due and the trial reserved
func OnboardingLatchFirstInstall(ctx context.Context) (bool, error) {
	if _, explicit := onboardingExplicitRun(); explicit || !InstallStateProbe(ctx).Fresh() {
		return false, nil
	}
	if err := SetOnboardingRun(ctx, true, false); err != nil {
		return false, err
	}
	if err := ReserveInstallTrial(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// SetOnboardingRun persists onboarding.run, and with the mark the profile build offered flag, in the root config as one write
func SetOnboardingRun(ctx context.Context, value bool, markProfileOffered bool) error {
	root := home.Root()
	doc, err := config.ReadUser(root)
	if err != nil {
		return err
	}
	section, _ := doc["onboarding"].(map[string]any)
	if section == nil {
		section = map[string]any{}
		doc["onboarding"] = section
	}
	section["run"] = value
	if markProfileOffered {
		seen, _ := section["seen"].(map[string]any)
		if seen == nil {
			seen = map[string]any{}
			section["seen"] = seen
		}
		seen[OnboardingProfileBuildFlag] = true
	}
	if err := config.SaveUser(ctx, root, doc); err != nil {
		return err
	}
	OnboardingDueInvalidate()
	return nil
}
