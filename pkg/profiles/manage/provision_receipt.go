package manage

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Value of one best effort read of a receipt, the fallback when the read fails, a config outage raised
func receiptRead[T any](label string, fallback T, read func() (T, error)) T {
	var value T
	err := attempt(func() (err error) {
		value, err = read()
		return err
	})
	if err == nil {
		return value
	}
	if configOutage(err) {
		panic(err)
	}
	pkgLog().Warn(fmt.Sprintf("profile-create %s failed", label), "error", err)
	return fallback
}

// Whether the live multiplexer recorded the profile among those it serves, nil when none recorded what it serves
func servedNow(name string) *bool {
	type record struct {
		served   []string
		recorded bool
	}
	seen := receiptRead("served profiles", record{}, func() (record, error) {
		served, recorded, err := RecordedServedProfiles()
		return record{served, recorded}, err
	})
	if !seen.recorded {
		return nil
	}
	return receiptRead("served profiles", (*bool)(nil), func() (*bool, error) {
		canon, err := profiles.NormalizeProfileName(name)
		if err != nil {
			return nil, err
		}
		found := false
		for _, served := range seen.served {
			servedCanon, err := profiles.NormalizeProfileName(served)
			if err != nil {
				return nil, err
			}
			found = found || servedCanon == canon
		}
		return &found, nil
	})
}

// Mirrored flag of a receipt
func mirroredFlag(done bool) tuigateway.MirroredValue { return tuigateway.MirroredValue{Done: done} }

// What profile create prints about a finished create, every read best effort since the profile exists whatever a read says
func ReceiptOf(ctx context.Context, req ProfileCreateRequest, result *ProvisionResult) tuigateway.ProfileCreateReceipt {
	path := result.Path
	launch := receiptRead("launch label", "default", func() (string, error) { return profiles.GetActiveProfileName(ctx), nil })
	source := launch
	if req.CloneFrom != nil && *req.CloneFrom != "" {
		source = *req.CloneFrom
	}
	receipt := tuigateway.ProfileCreateReceipt{
		Path: path, LaunchLabel: launch, SourceLabel: source,
		DisplayPath: receiptRead("display path", path, func() (string, error) { return home.Display(path), nil }),
		Mirrored: map[string]tuigateway.MirroredValue{"env": mirroredFlag(result.Mirrored.Env), "auth": result.Mirrored.Auth,
			"model_inherited": mirroredFlag(result.Mirrored.ModelInherited), "voice": mirroredFlag(result.Mirrored.Voice)},
		Warnings: append([]string{}, result.Warnings...), ClonedPlugins: []string{}, ChannelsLeftBehind: []string{}, SharedChannels: []string{},
		SkillsOptedOut: truthy(result.SkillsSeeded["skipped_opt_out"]), ServedNow: servedNow(req.Name),
	}
	if !req.Fresh() {
		if !req.CloneAll {
			receipt.ClonedPlugins = receiptRead("cloned plugins", []string{}, func() ([]string, error) { return ClonedPluginNames(path), nil })
			if provider := receiptRead("cloned memory provider", "", func() (string, error) { return memory.ClonedMemoryProvider(ctx, path), nil }); provider != "" {
				receipt.MemoryProvider = &provider
			}
		}
		sourceDir := receiptRead("source profile", "", func() (string, error) { return profiles.GetProfileDir(source) })
		switch {
		case sourceDir == "" || !isDir(sourceDir):
		case req.CloneChannels:
			receipt.SharedChannels = receiptRead("shared channels", []string{}, func() ([]string, error) {
				return SharedChannelCredentials(ctx, path, sourceDir)
			})
		default:
			receipt.ChannelsLeftBehind = receiptRead("channels left behind", []string{}, func() ([]string, error) {
				return ChannelPlatformsConfigured(ctx, sourceDir)
			})
		}
	}
	if copied, counted := sizedLen(result.SkillsSeeded["copied"]); counted {
		receipt.SkillsSynced = &copied
	}
	if result.AliasPath != "" {
		receipt.AliasPath = &result.AliasPath
	}
	if result.AliasCollision != "" {
		receipt.AliasCollision = &result.AliasCollision
	}
	return receipt
}

// Length of a list the skill sync reports, false for anything that is not a list
func sizedLen(v any) (int64, bool) {
	switch list := v.(type) {
	case []string:
		return int64(len(list)), true
	case []any:
		return int64(len(list)), true
	}
	return 0, false
}
