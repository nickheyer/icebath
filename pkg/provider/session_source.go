package provider

import (
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Interactive UI transport labels a one shot child inherits but does not belong to
var UITransportSources = map[string]bool{"tui": true, "desktop": true}

// Source label of finite non interactive CLI runs
const OneshotSource = "oneshot"

// Session row source from the bound session context, a one shot run getting oneshot, else the platform or cli
func SessionSourceFor(env *tools.Env, platform string) string {
	source := pystr.Strip(tools.SessionEnvValue(env, "ICEBATH_SESSION_SOURCE"))
	singleQuery := tools.SessionEnvValue(env, "ICEBATH_SINGLE_QUERY_SESSION") == "1"
	explicit := tools.SessionEnvValue(env, "ICEBATH_SESSION_SOURCE_EXPLICIT") == "1"
	if singleQuery && !explicit && UITransportSources[source] {
		source = ""
	}
	effective := platform
	if effective == "" {
		effective = "cli"
	}
	if singleQuery && source == "" && effective == "cli" {
		return OneshotSource
	}
	if source != "" {
		return source
	}
	return effective
}
