package acpclient

import (
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Marker base URL the former Copilot alias exported as ACP_MARKER_BASE_URL
const CopilotMarkerBaseURL = "acp://copilot"

const (
	copilotPlaceholder = "copilot-acp"
	copilotArgsEnvVar  = "ICEBATH_COPILOT_ACP_ARGS"
	ghExtensionError   = "Icebath ACP mode requires the NEW GitHub Copilot CLI (github.com/github/copilot-cli), but the binary it just " +
		"spawned is the deprecated `gh copilot` extension.\n\n" +
		"Install the new CLI:\n  npm install -g @github/copilot\n  # then verify with: copilot --help\n\n" +
		"If `copilot` already resolves to the new CLI but you still see this,\npoint Icebath at it explicitly:\n" +
		"  export ICEBATH_COPILOT_ACP_COMMAND=/path/to/new/copilot\n\n" +
		"Alternative: use the `copilot` provider (no ACP, hits the Copilot API\ndirectly with a Copilot subscription " +
		"token) via `icebath setup`.\n\nOriginal error:\n"
)

var (
	copilotCommandEnvVars = []string{"ICEBATH_COPILOT_ACP_COMMAND", "COPILOT_CLI_PATH"}
	ghExtensionRequired   = []string{"gh-copilot"}
	ghExtensionMarkers    = []string{"has been deprecated", "no commands will be executed"}
	formerCopilotWarned   atomic.Bool
)

// Logs the alias deprecation once per process on the silent py.warnings channel
func warnFormerCopilotName(name string) {
	if formerCopilotWarned.CompareAndSwap(false, true) {
		slog.Default().With("logger", "py.warnings").Debug("DeprecationWarning: agent.copilot_acp_client." + name +
			" is deprecated; subclass agent.acp_client.ACPClient instead.")
	}
}

// True when the entry meta copilotEnablement reads disabled, AttributeError for a non dict entry or meta
func copilotDisabled(entry any) (bool, error) {
	meta, err := attrGet(entry, "_meta")
	if err != nil {
		return false, err
	}
	enablement, err := attrGet(or(meta, pyjson.NewObject()), "copilotEnablement")
	if err != nil {
		return false, err
	}
	return pystr.Lower(strField(enablement)) == "disabled", nil
}

// True when stderr names the gh-copilot extension together with one of its deprecation markers
func isGhExtensionBanner(stderrText string) bool {
	lower := pystr.Lower(stderrText)
	hit := func(needles []string) bool {
		for _, n := range needles {
			if strings.Contains(lower, n) {
				return true
			}
		}
		return false
	}
	return hit(ghExtensionRequired) && hit(ghExtensionMarkers)
}

// Install guidance for the deprecated gh copilot extension followed by its stderr, false for other exits
func copilotEarlyExitError(stderrText string) (string, bool) {
	if isGhExtensionBanner(stderrText) {
		return ghExtensionError + stderrText, true
	}
	return "", false
}

// Historical Copilot launch defaults, labels, model filter and early exit diagnosis
func copilotVendor() Vendor {
	v := BaseVendor()
	v.DefaultCommand = "copilot"
	v.DefaultArgs = []string{"--acp", "--stdio"}
	v.CommandEnvVars = copilotCommandEnvVars
	v.ArgsEnvVar = copilotArgsEnvVar
	v.MarkerBaseURL = CopilotMarkerBaseURL
	v.PlaceholderModel = copilotPlaceholder
	v.Label = "Copilot ACP"
	v.MissingCommandHint = "Install GitHub Copilot CLI or set ICEBATH_COPILOT_ACP_COMMAND/COPILOT_CLI_PATH."
	v.UnsupportedTransportHint = "Either install a CLI that ships with --acp support (e.g. `@github/copilot` late 2025+), or set " +
		"ICEBATH_COPILOT_ACP_COMMAND / ICEBATH_COPILOT_ACP_ARGS to a working pair."
	v.ModelEntryDisabled = copilotDisabled
	v.EarlyExitError = copilotEarlyExitError
	return v
}

// Copilot vendor a former alias client subclass customises, warning once for the deprecated alias
func CopilotVendor() Vendor {
	warnFormerCopilotName("CopilotACPClient")
	return copilotVendor()
}

// Launch command from the Copilot command env vars, else copilot
func CopilotResolveCommand() string {
	warnFormerCopilotName("_resolve_command")
	return (&Client{Vendor: copilotVendor()}).resolveCommand()
}

// Shell split Copilot args env var, else the acp stdio pair
func CopilotResolveArgs() ([]string, error) {
	warnFormerCopilotName("_resolve_args")
	return (&Client{Vendor: copilotVendor()}).resolveArgs()
}

// Ordered unique ids of the entries the Copilot enablement filter keeps
func CopilotEnabledIDList(entries any, key string) ([]string, error) {
	warnFormerCopilotName("_enabled_id_list")
	return enabledIDList(entries, key, copilotDisabled)
}

// Model ids session/new advertises with Copilot disabled entries dropped
func CopilotSessionModelIDs(session any) ([]string, error) {
	warnFormerCopilotName("_session_model_ids")
	return sessionModelIDs(session, copilotDisabled)
}

// Legacy availableModels ids with Copilot disabled entries dropped
func CopilotLegacySessionModelIDs(session any) ([]string, error) {
	warnFormerCopilotName("_legacy_session_model_ids")
	return legacySessionModelIDs(session, copilotDisabled)
}

// Model selection request skipping the copilot-acp default model id and Copilot disabled entries
func CopilotModelSelectionRequest(session any, requestedModel string) (string, *pyjson.Object, bool, error) {
	warnFormerCopilotName("_model_selection_request")
	return modelSelectionRequest(session, requestedModel, copilotPlaceholder, copilotDisabled)
}
