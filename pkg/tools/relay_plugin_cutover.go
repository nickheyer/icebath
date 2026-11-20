package tools

import (
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Environment variable selecting an explicit Relay plugins toml file
const RelayPluginsConfigEnv = "ICEBATH_NEMO_RELAY_PLUGINS_TOML"

// Exporter variables of the removed relay plugin that no longer configure Relay
var LegacyRelayExportEnvVars = []string{
	"ICEBATH_NEMO_RELAY_ATOF_ENABLED",
	"ICEBATH_NEMO_RELAY_ATOF_OUTPUT_DIRECTORY",
	"ICEBATH_NEMO_RELAY_ATOF_FILENAME",
	"ICEBATH_NEMO_RELAY_ATOF_MODE",
	"ICEBATH_NEMO_RELAY_ATIF_ENABLED",
	"ICEBATH_NEMO_RELAY_ATIF_OUTPUT_DIRECTORY",
	"ICEBATH_NEMO_RELAY_ATIF_FILENAME_TEMPLATE",
	"ICEBATH_NEMO_RELAY_ATIF_AGENT_NAME",
	"ICEBATH_NEMO_RELAY_ATIF_AGENT_VERSION",
	"ICEBATH_NEMO_RELAY_ATIF_EXPORT_TIMEOUT_S",
	"ICEBATH_NEMO_RELAY_ATIF_MODEL_NAME",
	"ICEBATH_NEMO_RELAY_ATIF_SUBAGENT_EXPORT_MODE",
}

// Sorted legacy exporter variables set to a non blank value in env
func ConfiguredLegacyRelayEnvVars(env map[string]any) []string {
	var out []string
	for _, name := range LegacyRelayExportEnvVars {
		if v, ok := env[name]; ok && v != nil && pystr.Strip(pyrepr.Str(v)) != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
