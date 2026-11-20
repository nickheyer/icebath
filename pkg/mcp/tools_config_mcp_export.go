package mcp

import "github.com/nickheyer/icebath/pkg/config"

// Toggles server and tool targets in each server exclude list for callers outside the package, returning servers not in config
func ApplyMCPChange(cfg config.Tree, targets []string, action string) map[string]bool {
	return applyMCPChange(cfg, targets, action)
}

// The mcp_servers block a catalog manifest translates to, before credentials and tool filters
func BuildServerConfig(entry *CatalogEntry, installDir string) (map[string]any, error) {
	return buildServerConfig(entry, installDir)
}

// Fills transport details from a known preset when none were given, an error for an unknown preset
func ApplyMCPPreset(presetName, url, command string, cmdArgs []string, serverConfig map[string]any) error {
	_, _, _, _, err := applyMCPPreset(presetName, url, command, cmdArgs, serverConfig)
	return err
}

// Env key holding the API key of a server, MCP_<NAME>_API_KEY
func EnvKeyForServer(name string) string { return envKeyForServer(name) }
