package config

import "context"

// Writes the raw user config of home h, reporting the change to the saved observer when one is installed
func WriteUserConfig(ctx context.Context, h, configPath string, userConfig map[string]any) error {
	return writeUserConfig(ctx, h, configPath, userConfig)
}
