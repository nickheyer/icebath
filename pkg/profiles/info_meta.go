package profiles

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
)

type distributionMeta struct{ name, version, source any }

// Name, version and source from distribution.yaml, all nil when the file is absent or not a mapping
func ReadDistributionMeta(profileDir string) (name, version, source any) {
	path := filepath.Join(profileDir, "distribution.yaml")
	m := cachedProfileRead(path, "distribution", func() distributionMeta {
		data := loadYAMLDict(path)
		if data == nil {
			return distributionMeta{}
		}
		return distributionMeta{data["name"], data["version"], data["source"]}
	}, nil)
	return m.name, m.version, m.source
}

type configModel struct{ model, provider any }

// Model and provider from the profile config.yaml read raw, nil for an absent file or a failed read
func ReadConfigModel(ctx context.Context, profileDir string) (model, provider any, err error) {
	configPath := filepath.Join(profileDir, "config.yaml")
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || !exists {
		return nil, nil, err
	}
	var sigErr error
	signature := func(p string) any {
		v, err := config.ConfigCacheVersionOrNone(ctx, p)
		if err != nil {
			sigErr = err
			return nil
		}
		if v == nil {
			return nil
		}
		return v
	}
	m := cachedProfileRead(configPath, "config-model", func() configModel {
		raw, err := config.ReadUserConfigRaw(ctx, configPath)
		if err != nil {
			return configModel{}
		}
		switch cfg := raw["model"].(type) {
		case string:
			return configModel{cfg, nil}
		case map[string]any:
			model := cfg["default"]
			if !pyTruthy(model) {
				model = cfg["model"]
			}
			return configModel{model, cfg["provider"]}
		}
		return configModel{}
	}, signature)
	if sigErr != nil {
		return nil, nil, sigErr
	}
	return m.model, m.provider, nil
}
