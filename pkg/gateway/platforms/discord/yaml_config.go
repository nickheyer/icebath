package discord

import "context"

// Extras of the config.yaml discord section the adapter reads from its platform config, nil when none; the rest are settings
func applyYAMLConfig(ctx context.Context, yamlCfg, discordCfg map[string]any) (map[string]any, error) {
	mentions, ok := discordCfg["allow_mentions"].(map[string]any)
	if !ok {
		return nil, nil
	}
	cp := make(map[string]any, len(mentions))
	for k, v := range mentions {
		cp[k] = v
	}
	return map[string]any{"allow_mentions": cp}, nil
}
